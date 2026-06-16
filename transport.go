// Package transport defines the pluggable backend interface used to reach
// physical/software MIDI and OSC targets, and the value types shared by all
// backends. Adding a new protocol means implementing Transport (the code-level
// extension point); adding a new device is just YAML (see package device).
package transport

import "context"

// EventKind distinguishes MIDI byte payloads from OSC packets from raw
// (vendor HID) reports.
type EventKind int

const (
	MIDIEvent EventKind = iota
	OSCEvent
	// RawEvent carries an opaque byte payload that has no MIDI/OSC framing:
	// a vendor HID report (e.g. the Neuro or Torpedo Remote pipe), moved
	// verbatim by the usbhid transport. The bytes live in Data.
	RawEvent
)

// Event is a fully-rendered control change ready to emit on a transport. The
// engine renders a (DeviceType, Control, value) tuple into one or more Events.
type Event struct {
	Kind EventKind

	// MIDI fields (Kind == MIDIEvent); also the raw report payload for
	// Kind == RawEvent (a HID report moved verbatim, no channel applied).
	Channel int    // 0-15
	Data    []byte // raw MIDI status+data bytes (channel already applied), or a raw HID report

	// OSC fields (Kind == OSCEvent).
	OSCAddr string
	OSCArgs []any
}

// Endpoint is a discovered, addressable transport target: a BLE peripheral, a
// USB MIDI port, or an OSC host:port.
type Endpoint struct {
	ID        string // stable id: BLE address, ALSA port name, or host:port
	Name      string // friendly name
	Transport string // owning transport id
	Paired    bool
	Connected bool
}

// Transport is a pluggable backend.
//
//   - blemidi owns BLE discovery + pairing + connection (BlueZ/D-Bus) and the
//     BLE-MIDI GATT data path, including the inbound notify channel.
//   - osc targets a host:port (Pair/Connect are no-ops).
//   - usbmidi wraps ALSA MIDI ports via gomidi (bonus).
type Transport interface {
	ID() string

	// Discover enumerates currently reachable endpoints.
	Discover(ctx context.Context) ([]Endpoint, error)

	// Pair establishes a bond (BLE). No-op for transports that do not pair.
	Pair(ctx context.Context, endpointID string) error

	// Connect / Disconnect manage an active link to an endpoint.
	Connect(ctx context.Context, endpointID string) error
	Disconnect(ctx context.Context, endpointID string) error

	// Send emits one event to a connected endpoint.
	Send(ctx context.Context, endpointID string, ev Event) error

	// Listen streams inbound events from an endpoint (for MIDI-learn and
	// desired-state reconciliation). The channel closes when ctx is done.
	Listen(ctx context.Context, endpointID string) (<-chan Event, error)
}
