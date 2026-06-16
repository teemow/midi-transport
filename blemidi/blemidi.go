// Package blemidi implements the BLE-MIDI transport on Linux. After the Phase A
// hardware spike it is a HYBRID of two cooperating planes:
//
//   - Control plane (BlueZ over D-Bus, pure-Go): discovery, pairing (an Agent1)
//     and — for the fallback — connection. See dbus.go / agent.go.
//   - Data plane (primary): the PipeWire/WirePlumber bluez5 bridge exposes each
//     bonded BLE-MIDI endpoint as an ALSA-seq port; we send/receive MIDI over
//     that port with gomidi (CGO + ALSA). See alsa.go.
//   - Data plane (fallback): raw GATT — AcquireWrite + notify on the BLE-MIDI
//     I/O characteristic, with BLE-MIDI timestamp framing. See gatt.go /
//     framing.go. Used on headless / no-PipeWire hosts.
//
// Why the split: on a PipeWire desktop WirePlumber auto-claims the BLE-MIDI I/O
// characteristic, so a second writer (us) gets org.bluez.Error.NotAuthorized on
// AcquireWrite. PipeWire does NOT pair, so BlueZ still owns discovery+pairing.
// Hence: pair via BlueZ, then drive MIDI through the PipeWire-bridged ALSA-seq
// port; fall back to raw GATT only where PipeWire is absent.
//
// BLE-MIDI GATT identifiers (BLE-MIDI specification):
//   - Service UUID:       03B80E5A-EDE8-4B33-A751-6CE34EC4C700
//   - MIDI I/O char UUID: 7772E5DB-3868-4112-A1A9-F2669D106BF3
//
// Addressing unit is (endpoint, channel): one WIDI Thru6 endpoint fans out to
// the pedals by MIDI channel. Channel-less traffic (e.g. the SL-2's MIDI clock)
// is just a system message in Event.Data and needs no special handling here.
//
// Linux-only by design (BlueZ + ALSA). A CoreBluetooth/WinRT backend can be
// added later behind transport.Transport.
package blemidi

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/teemow/midi-transport"
)

// Service / characteristic UUIDs from the BLE-MIDI specification (BlueZ reports
// UUIDs lowercased, so we compare lowercased throughout).
const (
	ServiceUUID = "03b80e5a-ede8-4b33-a751-6ce34ec4c700"
	IOCharUUID  = "7772e5db-3868-4112-a1a9-f2669d106bf3"
)

const defaultDiscoverTimeout = 8 * time.Second

// dataPlane is the per-endpoint MIDI byte path (ALSA-seq or raw GATT). Both
// implementations live behind this interface so the transport is agnostic to
// which one a given host/endpoint ended up using.
type dataPlane interface {
	// Send emits one raw MIDI message (status + data, channel already applied).
	Send(midi []byte) error
	// Listen streams decoded inbound MIDI as transport events until ctx is done.
	Listen(ctx context.Context) (<-chan transport.Event, error)
	// Close releases the underlying ports / file descriptors.
	Close() error
}

// bleDevice is the transport's per-endpoint state.
type bleDevice struct {
	address string // uppercased BLE address (the endpoint id)
	name    string // friendly name/alias (used to match the ALSA-seq port)
	path    dbus.ObjectPath
	plane   dataPlane
	viaGATT bool // true if the raw-GATT fallback owns the BLE link
}

// Transport is the hybrid BLE-MIDI backend.
type Transport struct {
	adapterName     string
	discoverTimeout time.Duration

	mu          sync.Mutex
	conn        *dbus.Conn
	adapter     dbus.BusObject
	cancelAgent func()
	devices     map[string]*bleDevice // keyed by uppercased BLE address
}

// Option configures the transport.
type Option func(*Transport)

// WithAdapter selects a non-default BlueZ adapter (default "hci0").
func WithAdapter(name string) Option {
	return func(t *Transport) {
		if name != "" {
			t.adapterName = name
		}
	}
}

// WithDiscoverTimeout sets how long Discover scans before returning.
func WithDiscoverTimeout(d time.Duration) Option {
	return func(t *Transport) {
		if d > 0 {
			t.discoverTimeout = d
		}
	}
}

// New returns a BLE-MIDI transport bound to the default BlueZ adapter. It does
// not touch D-Bus until the first BLE operation (so the daemon starts on hosts
// without BlueZ, and the package tests need no system bus).
func New(opts ...Option) (*Transport, error) {
	t := &Transport{
		adapterName:     "hci0",
		discoverTimeout: defaultDiscoverTimeout,
		devices:         map[string]*bleDevice{},
	}
	for _, o := range opts {
		o(t)
	}
	return t, nil
}

func (t *Transport) ID() string { return "blemidi" }

// Close releases the data planes, the pairing agent and the D-Bus connection.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, d := range t.devices {
		if d.plane != nil {
			_ = d.plane.Close()
			d.plane = nil
		}
	}
	if t.cancelAgent != nil {
		t.cancelAgent()
		t.cancelAgent = nil
	}
	if t.conn != nil {
		err := t.conn.Close()
		t.conn = nil
		return err
	}
	return nil
}

// Discover scans for BLE-MIDI endpoints (service-UUID advertisers plus bonded
// devices) and returns them. It also records each device's path/name so a later
// Pair/Connect by address can resolve without re-scanning.
func (t *Transport) Discover(ctx context.Context) ([]transport.Endpoint, error) {
	if err := t.ensureBus(); err != nil {
		return nil, err
	}
	found, err := t.discover(ctx)
	if err != nil && len(found) == 0 {
		return nil, err
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]transport.Endpoint, 0, len(found))
	for path, props := range found {
		addr := strings.ToUpper(strProp(props, "Address"))
		if addr == "" {
			continue
		}
		name := deviceLabel(props)
		d := t.deviceLocked(addr)
		d.path = path
		d.name = name
		out = append(out, transport.Endpoint{
			ID:        addr,
			Name:      name,
			Transport: t.ID(),
			Paired:    boolPropMap(props, "Paired"),
			Connected: boolPropMap(props, "Connected"),
		})
	}
	return out, nil
}

// Pair bonds with the endpoint and marks it Trusted. It does not open a data
// path; that happens in Connect (the PipeWire bridge connects on its own once
// the device is bonded+trusted).
func (t *Transport) Pair(ctx context.Context, endpointID string) error {
	if err := t.ensureBus(); err != nil {
		return err
	}
	path, err := t.resolveDevicePath(ctx, endpointID)
	if err != nil {
		return err
	}
	if err := t.pairDevice(ctx, path); err != nil {
		return err
	}
	t.recordPath(endpointID, path)
	return nil
}

// Connect establishes the data path for the endpoint, preferring the PipeWire/
// ALSA-seq bridge and falling back to raw GATT where PipeWire is absent.
func (t *Transport) Connect(ctx context.Context, endpointID string) error {
	if err := t.ensureBus(); err != nil {
		return err
	}
	path, err := t.resolveDevicePath(ctx, endpointID)
	if err != nil {
		return err
	}
	// The endpoint must be bonded+trusted for either data path (the GATT char is
	// encrypted; the PipeWire bridge only claims bonded endpoints).
	if err := t.pairDevice(ctx, path); err != nil {
		return err
	}

	name := t.deviceName(endpointID, path)

	// Primary: the PipeWire/WirePlumber ALSA-seq bridge.
	plane, err := openALSADataPlane(name)
	if err == nil {
		t.setPlane(endpointID, path, name, plane, false)
		log.Printf("blemidi: %s (%s) connected via PipeWire ALSA-seq bridge", name, endpointID)
		return nil
	}
	log.Printf("blemidi: ALSA-seq path unavailable for %s (%v); trying raw GATT", endpointID, err)

	// Fallback: raw GATT (headless / no PipeWire). Needs an encrypted link.
	if err := t.gattConnect(ctx, path); err != nil {
		return err
	}
	charPath, err := t.resolveIOChar(ctx, path)
	if err != nil {
		return err
	}
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("blemidi: bus connection closed")
	}
	plane, err = newGATTDataPlane(ctx, conn, charPath)
	if err != nil {
		return fmt.Errorf("blemidi: GATT data plane (a PipeWire host needs the bluetooth.midi WirePlumber rule disabled): %w", err)
	}
	t.setPlane(endpointID, path, name, plane, true)
	log.Printf("blemidi: %s (%s) connected via raw GATT", name, endpointID)
	return nil
}

// Disconnect tears down the endpoint's data path. The BLE link is dropped only
// when the raw-GATT fallback owned it; the PipeWire path leaves the link to
// WirePlumber.
func (t *Transport) Disconnect(ctx context.Context, endpointID string) error {
	t.mu.Lock()
	d := t.devices[strings.ToUpper(endpointID)]
	if d == nil || d.plane == nil {
		t.mu.Unlock()
		return nil
	}
	plane := d.plane
	viaGATT := d.viaGATT
	path := d.path
	conn := t.conn
	d.plane = nil
	d.viaGATT = false
	t.mu.Unlock()

	err := plane.Close()
	if viaGATT && conn != nil {
		disconnectDevice(ctx, conn, path)
	}
	return err
}

// Send emits one MIDI event to a connected endpoint.
func (t *Transport) Send(ctx context.Context, endpointID string, ev transport.Event) error {
	if ev.Kind != transport.MIDIEvent {
		return fmt.Errorf("blemidi: cannot send %v event (MIDI only)", ev.Kind)
	}
	plane, err := t.planeFor(endpointID)
	if err != nil {
		return err
	}
	return plane.Send(ev.Data)
}

// Listen streams inbound MIDI events from a connected endpoint.
func (t *Transport) Listen(ctx context.Context, endpointID string) (<-chan transport.Event, error) {
	plane, err := t.planeFor(endpointID)
	if err != nil {
		return nil, err
	}
	return plane.Listen(ctx)
}

// --- internal device-map helpers (lock-guarded) -----------------------------

func (t *Transport) planeFor(endpointID string) (dataPlane, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.devices[strings.ToUpper(endpointID)]
	if d == nil || d.plane == nil {
		return nil, fmt.Errorf("blemidi: endpoint %s not connected", endpointID)
	}
	return d.plane, nil
}

// deviceLocked returns the bleDevice for addr (already uppercased), creating
// it if absent. The caller must hold t.mu.
func (t *Transport) deviceLocked(addr string) *bleDevice {
	d := t.devices[addr]
	if d == nil {
		d = &bleDevice{address: addr}
		t.devices[addr] = d
	}
	return d
}

func (t *Transport) recordPath(endpointID string, path dbus.ObjectPath) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deviceLocked(strings.ToUpper(endpointID)).path = path
}

// deviceName returns the best-known friendly name for an endpoint, reading it
// from BlueZ (Alias/Name) when the device map has none yet.
func (t *Transport) deviceName(endpointID string, path dbus.ObjectPath) string {
	addr := strings.ToUpper(endpointID)
	t.mu.Lock()
	if d := t.devices[addr]; d != nil && d.name != "" {
		name := d.name
		t.mu.Unlock()
		return name
	}
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return ""
	}

	dev := conn.Object(bluezBus, path)
	for _, prop := range []string{"Alias", "Name"} {
		if v, err := dev.GetProperty(deviceIface + "." + prop); err == nil {
			if s, _ := v.Value().(string); s != "" {
				return s
			}
		}
	}
	return ""
}

func (t *Transport) setPlane(endpointID string, path dbus.ObjectPath, name string, plane dataPlane, viaGATT bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.deviceLocked(strings.ToUpper(endpointID))
	if d.plane != nil {
		_ = d.plane.Close()
	}
	d.path = path
	if name != "" {
		d.name = name
	}
	d.plane = plane
	d.viaGATT = viaGATT
}
