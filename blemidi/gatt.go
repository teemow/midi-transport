package blemidi

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/godbus/dbus/v5"
	"github.com/teemow/midi-transport"
)

// gattDataPlane is the raw-GATT (BlueZ over D-Bus) data plane: it writes
// BLE-MIDI packets through an AcquireWrite file descriptor and decodes inbound
// notifications from the characteristic's PropertiesChanged signals.
//
// This path is the headless / no-PipeWire fallback. On a PipeWire desktop the
// WirePlumber bluez5 plugin claims the I/O characteristic, so AcquireWrite
// returns org.bluez.Error.NotAuthorized (the central Phase A finding); there
// the ALSA-seq data plane is used instead. Using this path on such a host
// requires a WirePlumber rule disabling bluetooth.midi for the endpoint.
type gattDataPlane struct {
	conn     *dbus.Conn
	charPath dbus.ObjectPath
	char     dbus.BusObject
	mtu      uint16

	// wmu guards w: Send writes to it concurrently with Close clearing it.
	wmu sync.Mutex
	w   *os.File
}

// newGATTDataPlane enables notifications and acquires a write fd on the
// resolved BLE-MIDI characteristic.
func newGATTDataPlane(ctx context.Context, conn *dbus.Conn, charPath dbus.ObjectPath) (*gattDataPlane, error) {
	char := conn.Object(bluezBus, charPath)
	if call := char.Call(gattCharIface+".StartNotify", 0); call.Err != nil {
		return nil, fmt.Errorf("blemidi: start notify on %s: %w", charPath, call.Err)
	}

	var fd dbus.UnixFD
	var mtu uint16
	// BLE-MIDI is write-without-response; WriteValue is frequently rejected for
	// such characteristics, so we acquire a dedicated write fd (the low-latency
	// path) instead.
	call := char.CallWithContext(ctx, gattCharIface+".AcquireWrite", 0, map[string]dbus.Variant{})
	if call.Err != nil {
		char.Call(gattCharIface+".StopNotify", 0)
		return nil, fmt.Errorf("blemidi: acquire write on %s: %w", charPath, call.Err)
	}
	if err := call.Store(&fd, &mtu); err != nil {
		char.Call(gattCharIface+".StopNotify", 0)
		return nil, fmt.Errorf("blemidi: decode acquire-write reply: %w", err)
	}

	return &gattDataPlane{
		conn:     conn,
		charPath: charPath,
		char:     char,
		w:        os.NewFile(uintptr(fd), "ble-midi-write"),
		mtu:      mtu,
	}, nil
}

func (g *gattDataPlane) Send(midi []byte) error {
	g.wmu.Lock()
	defer g.wmu.Unlock()
	if g.w == nil {
		return fmt.Errorf("blemidi: GATT write fd closed")
	}
	// Reserve the 3-byte ATT write header from the negotiated MTU to get the
	// usable characteristic payload; 0 (some BlueZ versions) means "unknown",
	// treated as no limit. Frame and write under the lock so a concurrent Close
	// cannot close the fd out from under the write.
	usable := 0
	if g.mtu > 3 {
		usable = int(g.mtu) - 3
	}
	for _, pkt := range FrameMessageMTU(midi, usable) {
		if _, err := g.w.Write(pkt); err != nil {
			return fmt.Errorf("blemidi: GATT write: %w", err)
		}
	}
	return nil
}

func (g *gattDataPlane) Listen(ctx context.Context) (<-chan transport.Event, error) {
	sigCh := make(chan *dbus.Signal, 64)
	g.conn.Signal(sigCh)
	matchOpts := []dbus.MatchOption{
		dbus.WithMatchObjectPath(g.charPath),
		dbus.WithMatchInterface(propsIface),
		dbus.WithMatchMember("PropertiesChanged"),
	}
	if err := g.conn.AddMatchSignal(matchOpts...); err != nil {
		g.conn.RemoveSignal(sigCh)
		return nil, fmt.Errorf("blemidi: watch PropertiesChanged: %w", err)
	}

	out := make(chan transport.Event, 64)
	go func() {
		defer close(out)
		// Remove both the match rule (server-side) and the signal channel so a
		// repeated Listen/Disconnect cycle does not leak match rules on the bus.
		defer g.conn.RemoveSignal(sigCh)
		defer func() { _ = g.conn.RemoveMatchSignal(matchOpts...) }()
		// One decoder for the whole session so a SysEx spanning several
		// notifications is reassembled across packets.
		dec := &Decoder{}
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-sigCh:
				raw, ok := inboundValue(sig, g.charPath)
				if !ok {
					continue
				}
				for _, m := range dec.Decode(raw) {
					ev := transport.Event{Kind: transport.MIDIEvent, Data: m}
					if ch, ok := channelOf(m); ok {
						ev.Channel = ch
					}
					select {
					case out <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	return out, nil
}

func (g *gattDataPlane) Close() error {
	g.wmu.Lock()
	if g.w != nil {
		_ = g.w.Close()
		g.w = nil
	}
	g.wmu.Unlock()
	if g.char != nil {
		g.char.Call(gattCharIface+".StopNotify", 0)
	}
	return nil
}

// inboundValue extracts the raw characteristic Value from a PropertiesChanged
// signal for the given characteristic, if present.
func inboundValue(sig *dbus.Signal, charPath dbus.ObjectPath) ([]byte, bool) {
	if sig == nil || sig.Path != charPath || sig.Name != propsIface+".PropertiesChanged" {
		return nil, false
	}
	if len(sig.Body) < 2 {
		return nil, false
	}
	if iface, _ := sig.Body[0].(string); iface != gattCharIface {
		return nil, false
	}
	changed, _ := sig.Body[1].(map[string]dbus.Variant)
	v, ok := changed["Value"]
	if !ok {
		return nil, false
	}
	raw, ok := v.Value().([]byte)
	return raw, ok
}
