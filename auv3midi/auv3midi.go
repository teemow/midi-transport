// Package auv3midi implements the auv3midi transport: the LAN control channel
// into AUM that drives AUv3 plugins. It is a thin adapter over
// internal/midicontrol.Hub — the off-MCP WebSocket the ProbeMidiBrain dials —
// presented as a transport.Transport so a device type can simply declare
// `transport: auv3midi` and the engine resolves/sends it like any other backend.
//
// The brain is the agent's "hands": each rendered MIDI event is decoded into a
// midicontrol.Command and pushed to the connected brain, which re-emits it on
// its host MIDI-out inside AUM. The channel is outbound-only — there is no
// inbound MIDI from the brain — so Listen yields no events, and Connect/Pair are
// no-ops (the brain dials the daemon, not the other way round).
package auv3midi

import (
	"context"

	"github.com/teemow/midi-transport/midicontrol"
	"github.com/teemow/midi-transport"
)

// ID is the transport id device types declare to route over the brain channel.
const ID = "auv3midi"

// BrainEndpoint is the single logical endpoint the transport exposes: there is
// at most one ProbeMidiBrain per rig (the hub holds one connection), and the
// brain re-emits inside AUM regardless of any per-device endpoint string, so the
// device's endpoint field is unused for routing. Discover surfaces this id when
// a brain is connected. It is exported so the bind/discovery surface can name
// the brain endpoint from one source instead of re-typing the literal.
const BrainEndpoint = "brain"

// Transport adapts the ProbeMidiBrain control hub to the transport.Transport
// interface. The hub holds the live brain connection and serializes command
// frames to it; this type only translates between wire MIDI events and the
// hub's command frames.
type Transport struct {
	hub *midicontrol.Hub
}

// New returns an auv3midi transport over the given control hub. hub must be
// non-nil (it is shared with the LAN receiver that registers the brain
// connection).
func New(hub *midicontrol.Hub) *Transport {
	return &Transport{hub: hub}
}

func (t *Transport) ID() string { return ID }

// Discover surfaces the connected brain as a single endpoint, so a discovery
// pass can show whether the auv3midi channel is live. It returns nothing when
// no brain is connected.
func (t *Transport) Discover(ctx context.Context) ([]transport.Endpoint, error) {
	if !t.hub.Connected() {
		return nil, nil
	}
	return []transport.Endpoint{{
		ID:        BrainEndpoint,
		Name:      "ProbeMidiBrain (" + t.hub.Remote() + ")",
		Transport: ID,
		Connected: true,
	}}, nil
}

// Pair is a no-op: the brain channel has no pairing.
func (t *Transport) Pair(ctx context.Context, endpointID string) error { return nil }

// Connect is a no-op: the brain dials the daemon (the hub registers the
// connection from the LAN receiver), so there is nothing to open here. Send
// surfaces midicontrol.ErrNoBrain when nothing is connected.
func (t *Transport) Connect(ctx context.Context, endpointID string) error { return nil }

// Disconnect is a no-op: the brain connection is owned by the hub/receiver.
func (t *Transport) Disconnect(ctx context.Context, endpointID string) error { return nil }

// Send decodes a rendered MIDI event into a brain command frame and pushes it
// through the hub, where the brain re-emits it inside AUM. The endpoint is
// ignored (one brain per rig). Non-MIDI events and MIDI the brain protocol
// cannot carry (SysEx, pitch bend, channel pressure) are skipped silently;
// device types that need those should speak a hardware transport instead.
func (t *Transport) Send(ctx context.Context, endpointID string, ev transport.Event) error {
	if ev.Kind != transport.MIDIEvent {
		return nil
	}
	cmd, ok := midicontrol.CommandFromMIDI(ev.Data)
	if !ok {
		return nil
	}
	return t.hub.Send(ctx, cmd)
}

// Listen returns a channel that yields no events and closes when ctx is done:
// the brain channel is outbound-only (the agent's "hands", not its "ears"), so
// there is no inbound MIDI to reverse-map for feedback/learn. Returning a live
// no-op channel keeps StartInboundForDevices uniform across transports.
func (t *Transport) Listen(ctx context.Context, endpointID string) (<-chan transport.Event, error) {
	out := make(chan transport.Event)
	go func() {
		<-ctx.Done()
		close(out)
	}()
	return out, nil
}
