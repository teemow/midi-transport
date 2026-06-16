//go:build cgo

// Package usbmidi implements the USB/ALSA MIDI transport via
// gitlab.com/gomidi/midi over the rtmidi (ALSA) driver. Endpoints are ALSA
// sequencer port names; there is no pairing.
//
// It is an independent verification path for the rig: a USB-capable pedal can
// be driven over BLE and read back over USB (or vice-versa), so feedback works
// even when a device does not echo on the bus it was driven over.
//
// This file is the real backend and requires CGO + ALSA headers (the rtmidi
// driver links libasound). Pure-Go (CGO_ENABLED=0) builds compile the stub in
// usbmidi_stub.go instead.
package usbmidi

import (
	"context"
	"fmt"
	"sync"

	"github.com/teemow/midi-transport"
	"gitlab.com/gomidi/midi/v2"
	"gitlab.com/gomidi/midi/v2/drivers"
	_ "gitlab.com/gomidi/midi/v2/drivers/rtmididrv" // registers the ALSA-backed driver
)

// Transport is the USB/ALSA MIDI backend.
type Transport struct {
	mu    sync.Mutex
	ports map[string]*usbPort // endpoint name -> open ports
}

// usbPort holds an endpoint's opened ALSA-seq out (and, if present, in) ports.
type usbPort struct {
	out drivers.Out
	in  drivers.In // nil if the endpoint exposes no inbound port
}

// New returns a USB-MIDI transport.
func New() (*Transport, error) {
	return &Transport{ports: map[string]*usbPort{}}, nil
}

func (t *Transport) ID() string { return "usbmidi" }

// Discover lists ALSA MIDI ports. An endpoint is keyed by its out-port name;
// Paired is always true (USB needs no bonding) and Connected reflects whether
// we currently hold the port open.
func (t *Transport) Discover(ctx context.Context) ([]transport.Endpoint, error) {
	ins := map[string]bool{}
	for _, p := range midi.GetInPorts() {
		ins[p.String()] = true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []transport.Endpoint
	for _, p := range midi.GetOutPorts() {
		name := p.String()
		out = append(out, transport.Endpoint{
			ID:        name,
			Name:      name,
			Transport: t.ID(),
			Paired:    true,
			Connected: t.ports[name] != nil,
		})
		delete(ins, name) // an out+in pair is one endpoint
	}
	// Inbound-only ports (rare, but possible) still make valid endpoints.
	for name := range ins {
		out = append(out, transport.Endpoint{ID: name, Name: name, Transport: t.ID(), Paired: true})
	}
	return out, nil
}

// Pair is a no-op for USB-MIDI.
func (t *Transport) Pair(ctx context.Context, endpointID string) error { return nil }

// Connect opens the named ALSA out port (and the matching in port if one
// exists) so the endpoint is ready for Send/Listen. It is idempotent.
func (t *Transport) Connect(ctx context.Context, endpointID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ports[endpointID] != nil {
		return nil
	}
	out, err := findOutPort(endpointID)
	if err != nil {
		return err
	}
	if err := out.Open(); err != nil {
		return fmt.Errorf("usbmidi: open out %q: %w", endpointID, err)
	}
	p := &usbPort{out: out}
	if in, err := findInPort(endpointID); err == nil {
		p.in = in // opened lazily by midi.ListenTo
	}
	t.ports[endpointID] = p
	return nil
}

// Disconnect closes the endpoint's ports.
func (t *Transport) Disconnect(ctx context.Context, endpointID string) error {
	t.mu.Lock()
	p := t.ports[endpointID]
	delete(t.ports, endpointID)
	t.mu.Unlock()
	if p == nil {
		return nil
	}
	if p.in != nil && p.in.IsOpen() {
		_ = p.in.Close()
	}
	if p.out != nil {
		return p.out.Close()
	}
	return nil
}

// Send writes raw MIDI bytes (status + data, channel already applied) to the
// endpoint's ALSA out port.
func (t *Transport) Send(ctx context.Context, endpointID string, ev transport.Event) error {
	if ev.Kind != transport.MIDIEvent {
		return fmt.Errorf("usbmidi: cannot send %v event (MIDI only)", ev.Kind)
	}
	t.mu.Lock()
	p := t.ports[endpointID]
	t.mu.Unlock()
	if p == nil || p.out == nil {
		return fmt.Errorf("usbmidi: endpoint %s not connected", endpointID)
	}
	if err := p.out.Send(ev.Data); err != nil {
		return fmt.Errorf("usbmidi: send to %s: %w", endpointID, err)
	}
	return nil
}

// Listen streams inbound MIDI from the endpoint's ALSA in port as
// transport.Events. The channel closes when ctx is done.
func (t *Transport) Listen(ctx context.Context, endpointID string) (<-chan transport.Event, error) {
	t.mu.Lock()
	p := t.ports[endpointID]
	t.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("usbmidi: endpoint %s not connected", endpointID)
	}
	if p.in == nil {
		return nil, fmt.Errorf("usbmidi: endpoint %s exposes no inbound port", endpointID)
	}
	out := make(chan transport.Event, 64)
	stop, err := midi.ListenTo(p.in, func(msg midi.Message, _ int32) {
		ev := transport.Event{Kind: transport.MIDIEvent, Data: msg.Bytes()}
		if ch, ok := channelOf(msg.Bytes()); ok {
			ev.Channel = ch
		}
		select {
		case out <- ev:
		case <-ctx.Done():
		}
	}, midi.UseSysEx())
	if err != nil {
		return nil, fmt.Errorf("usbmidi: listen %q: %w", endpointID, err)
	}
	go func() {
		<-ctx.Done()
		stop()
		close(out)
	}()
	return out, nil
}

// findOutPort resolves an ALSA out port by exact name, falling back to gomidi's
// substring match so a slightly different alias still connects.
func findOutPort(name string) (drivers.Out, error) {
	for _, p := range midi.GetOutPorts() {
		if p.String() == name {
			return p, nil
		}
	}
	p, err := midi.FindOutPort(name)
	if err != nil {
		return nil, fmt.Errorf("usbmidi: no ALSA out port matching %q (amidi -l to list): %w", name, err)
	}
	return p, nil
}

func findInPort(name string) (drivers.In, error) {
	for _, p := range midi.GetInPorts() {
		if p.String() == name {
			return p, nil
		}
	}
	return midi.FindInPort(name)
}

// channelOf extracts the 0-based MIDI channel from a channel-voice message,
// returning ok=false for system messages (real-time, common, SysEx).
func channelOf(m []byte) (int, bool) {
	if len(m) == 0 {
		return 0, false
	}
	s := m[0]
	if s >= 0x80 && s < 0xF0 {
		return int(s & 0x0F), true
	}
	return 0, false
}
