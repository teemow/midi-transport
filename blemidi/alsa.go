//go:build cgo

package blemidi

import (
	"context"
	"fmt"

	"github.com/teemow/midi-transport"
	"gitlab.com/gomidi/midi/v2"
	"gitlab.com/gomidi/midi/v2/drivers"
	_ "gitlab.com/gomidi/midi/v2/drivers/rtmididrv" // registers the ALSA-backed driver
)

// alsaDataPlane is the primary BLE-MIDI data plane on a PipeWire desktop. The
// WirePlumber bluez5 plugin bridges each bonded BLE-MIDI endpoint into the ALSA
// sequencer (a client named after the device's BLE name), so we send and
// receive plain MIDI bytes over that ALSA-seq port via gomidi instead of
// owning the GATT characteristic (which PipeWire holds — see gatt.go).
//
// Requires CGO + ALSA (the gomidi rtmidi driver links libasound).
type alsaDataPlane struct {
	out drivers.Out
	in  drivers.In // nil if the endpoint exposes no inbound port
}

// openALSADataPlane resolves the ALSA-seq out (and, if present, in) port whose
// name contains the BLE endpoint name. It fails if no matching out port exists
// (e.g. PipeWire is not bridging the device), letting the caller fall back to
// the raw-GATT data plane.
func openALSADataPlane(name string) (dataPlane, error) {
	if name == "" {
		return nil, fmt.Errorf("blemidi: empty endpoint name; cannot resolve ALSA-seq port")
	}
	out, err := midi.FindOutPort(name)
	if err != nil {
		return nil, fmt.Errorf("blemidi: no ALSA-seq out port matching %q (is PipeWire bridging it?): %w", name, err)
	}
	if err := out.Open(); err != nil {
		return nil, fmt.Errorf("blemidi: open ALSA-seq out %q: %w", name, err)
	}
	dp := &alsaDataPlane{out: out}
	if in, err := midi.FindInPort(name); err == nil {
		dp.in = in // opened lazily by Listen
	}
	return dp, nil
}

func (a *alsaDataPlane) Send(m []byte) error {
	if err := a.out.Send(m); err != nil {
		return fmt.Errorf("blemidi: ALSA-seq send: %w", err)
	}
	return nil
}

func (a *alsaDataPlane) Listen(ctx context.Context) (<-chan transport.Event, error) {
	if a.in == nil {
		return nil, fmt.Errorf("blemidi: endpoint exposes no inbound ALSA-seq port")
	}
	out := make(chan transport.Event, 64)
	stop, err := midi.ListenTo(a.in, func(msg midi.Message, _ int32) {
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
		return nil, fmt.Errorf("blemidi: listen ALSA-seq: %w", err)
	}
	go func() {
		<-ctx.Done()
		stop()
		close(out)
	}()
	return out, nil
}

func (a *alsaDataPlane) Close() error {
	if a.in != nil {
		_ = a.in.Close()
	}
	if a.out != nil {
		return a.out.Close()
	}
	return nil
}
