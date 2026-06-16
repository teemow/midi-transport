package blemidi

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

// BlueZ D-Bus well-known names and interfaces (productionized from the Phase A
// blespike, which proved each of these calls against the live rig).
const (
	bluezBus          = "org.bluez"
	adapterIface      = "org.bluez.Adapter1"
	deviceIface       = "org.bluez.Device1"
	gattCharIface     = "org.bluez.GattCharacteristic1"
	agentManagerIface = "org.bluez.AgentManager1"
	agentIface        = "org.bluez.Agent1"
	propsIface        = "org.freedesktop.DBus.Properties"
	objMgrIface       = "org.freedesktop.DBus.ObjectManager"
)

// ensureBus lazily connects to the system D-Bus, powers the adapter on, and
// registers the pairing agent. It is a no-op once connected. Connecting lazily
// (rather than in New) lets the daemon start, and the package tests run, on
// hosts without BlueZ; the connection is only required when BLE is actually
// used.
func (t *Transport) ensureBus() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn != nil {
		return nil
	}

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("blemidi: connect system bus: %w", err)
	}
	adapter := conn.Object(bluezBus, dbus.ObjectPath("/org/bluez/"+t.adapterName))
	if err := setProp(adapter, adapterIface, "Powered", dbus.MakeVariant(true)); err != nil {
		_ = conn.Close()
		return fmt.Errorf("blemidi: power on adapter %s: %w", t.adapterName, err)
	}
	cancelAgent, err := registerAgent(conn, "NoInputNoOutput")
	if err != nil {
		_ = conn.Close()
		return err
	}

	t.conn = conn
	t.adapter = adapter
	t.cancelAgent = cancelAgent
	return nil
}

// discover scans for LE peripherals for up to t.discoverTimeout and returns the
// Device1 object paths that look like BLE-MIDI endpoints: anything advertising
// the BLE-MIDI service UUID, plus already-bonded devices (the CME WIDI dongles
// do not advertise the service UUID, so a UUID-only filter would hide them —
// once bonded they must still be listed).
func (t *Transport) discover(ctx context.Context) (map[dbus.ObjectPath]map[string]dbus.Variant, error) {
	// Filter on the LE transport only; do NOT constrain by service UUID, or the
	// WIDI-class dongles would be filtered out of the advertisement scan.
	filter := map[string]dbus.Variant{"Transport": dbus.MakeVariant("le")}
	if call := t.adapter.Call(adapterIface+".SetDiscoveryFilter", 0, filter); call.Err != nil {
		return nil, fmt.Errorf("blemidi: set discovery filter: %w", call.Err)
	}
	if call := t.adapter.Call(adapterIface+".StartDiscovery", 0); call.Err != nil {
		return nil, fmt.Errorf("blemidi: start discovery: %w", call.Err)
	}
	defer t.adapter.Call(adapterIface+".StopDiscovery", 0)

	deadline := time.After(t.discoverTimeout)
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()

	found := map[dbus.ObjectPath]map[string]dbus.Variant{}
	for {
		objects, err := managedObjects(t.conn)
		if err != nil {
			return nil, err
		}
		for path, ifaces := range objects {
			props, ok := ifaces[deviceIface]
			if !ok {
				continue
			}
			if isMIDIDevice(props) || hasMIDIChar(objects, path) {
				found[path] = props
			}
		}

		select {
		case <-ctx.Done():
			return found, ctx.Err()
		case <-deadline:
			return found, nil
		case <-ticker.C:
		}
	}
}

// resolveDevicePath finds the Device1 object path for a BLE address. If the
// device is not yet known to BlueZ it runs a short discovery and retries.
func (t *Transport) resolveDevicePath(ctx context.Context, address string) (dbus.ObjectPath, error) {
	addr := strings.ToUpper(address)
	if path, ok := t.findDevicePath(addr); ok {
		return path, nil
	}
	if _, err := t.discover(ctx); err != nil && ctx.Err() != nil {
		return "", err
	}
	if path, ok := t.findDevicePath(addr); ok {
		return path, nil
	}
	return "", fmt.Errorf("blemidi: no device with address %s (try discover/pair first)", address)
}

// findDevicePath scans the current managed objects for a Device1 whose Address
// matches addr (already uppercased).
func (t *Transport) findDevicePath(addr string) (dbus.ObjectPath, bool) {
	objects, err := managedObjects(t.conn)
	if err != nil {
		return "", false
	}
	for path, ifaces := range objects {
		props, ok := ifaces[deviceIface]
		if !ok {
			continue
		}
		if strings.EqualFold(strProp(props, "Address"), addr) {
			return path, true
		}
	}
	return "", false
}

// pairDevice bonds with the device (unless already bonded) and marks it Trusted
// so BlueZ — and PipeWire's bluez5 bridge — can re-establish the encrypted link
// automatically. It does not open a GATT connection; for the PipeWire data path
// the bridge connects on its own once the device is bonded+trusted.
func (t *Transport) pairDevice(ctx context.Context, path dbus.ObjectPath) error {
	dev := t.conn.Object(bluezBus, path)
	paired, _ := boolProp(dev, deviceIface, "Paired")
	if !paired {
		if call := dev.CallWithContext(ctx, deviceIface+".Pair", 0); call.Err != nil {
			// A bond may race in concurrently; treat AlreadyExists as success.
			if !strings.Contains(call.Err.Error(), "AlreadyExists") {
				return fmt.Errorf("blemidi: pair %s: %w", path, call.Err)
			}
		}
	}
	if err := setProp(dev, deviceIface, "Trusted", dbus.MakeVariant(true)); err != nil {
		log.Printf("blemidi: set Trusted on %s: %v (continuing)", path, err)
	}
	return nil
}

// gattConnect opens an encrypted GATT connection used by the raw-GATT fallback
// data plane. The BLE-MIDI characteristic rejects writes on an unencrypted
// link, so a stale unencrypted connection is dropped and re-established over the
// bond (the "Operation Not Authorized" fix discovered in Phase A).
func (t *Transport) gattConnect(ctx context.Context, path dbus.ObjectPath) error {
	dev := t.conn.Object(bluezBus, path)
	if wasConnected, _ := boolProp(dev, deviceIface, "Connected"); wasConnected {
		if call := dev.CallWithContext(ctx, deviceIface+".Disconnect", 0); call.Err != nil {
			log.Printf("blemidi: disconnect %s before re-pairing link: %v", path, call.Err)
		}
		sleep(ctx, 1500*time.Millisecond)
	}
	if call := dev.CallWithContext(ctx, deviceIface+".Connect", 0); call.Err != nil {
		return fmt.Errorf("blemidi: connect %s: %w", path, call.Err)
	}
	return nil
}

// disconnectDevice drops the BLE link (used only after a raw-GATT session; the
// PipeWire data path leaves the link to WirePlumber).
func disconnectDevice(ctx context.Context, conn *dbus.Conn, path dbus.ObjectPath) {
	dev := conn.Object(bluezBus, path)
	if call := dev.CallWithContext(ctx, deviceIface+".Disconnect", 0); call.Err != nil {
		log.Printf("blemidi: disconnect %s: %v", path, call.Err)
	}
}

// resolveIOChar waits for GATT services to resolve and returns the object path
// of the BLE-MIDI I/O characteristic under the device.
func (t *Transport) resolveIOChar(ctx context.Context, path dbus.ObjectPath) (dbus.ObjectPath, error) {
	dev := t.conn.Object(bluezBus, path)
	deadline := time.After(20 * time.Second)
	for {
		if resolved, _ := boolProp(dev, deviceIface, "ServicesResolved"); resolved {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "", fmt.Errorf("blemidi: GATT services on %s did not resolve in time", path)
		case <-time.After(300 * time.Millisecond):
		}
	}

	objects, err := managedObjects(t.conn)
	if err != nil {
		return "", err
	}
	prefix := string(path) + "/"
	for p, ifaces := range objects {
		if !strings.HasPrefix(string(p), prefix) {
			continue
		}
		props, ok := ifaces[gattCharIface]
		if !ok {
			continue
		}
		if strings.EqualFold(strProp(props, "UUID"), IOCharUUID) {
			return p, nil
		}
	}
	return "", fmt.Errorf("blemidi: BLE-MIDI I/O characteristic not found on %s", path)
}

// --- predicates & D-Bus helpers ---------------------------------------------

// isMIDIDevice reports whether a Device1 advertises the BLE-MIDI service UUID
// or is already bonded (bonded devices stay listed even without the advert).
func isMIDIDevice(props map[string]dbus.Variant) bool {
	for _, u := range uuidProp(props) {
		if strings.EqualFold(u, ServiceUUID) {
			return true
		}
	}
	if paired, _ := props["Paired"].Value().(bool); paired {
		return true
	}
	return false
}

// hasMIDIChar reports whether any GATT characteristic object under devicePath
// is the BLE-MIDI I/O characteristic (true once services have resolved).
func hasMIDIChar(objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant, devicePath dbus.ObjectPath) bool {
	prefix := string(devicePath) + "/"
	for p, ifaces := range objects {
		if !strings.HasPrefix(string(p), prefix) {
			continue
		}
		if props, ok := ifaces[gattCharIface]; ok {
			if strings.EqualFold(strProp(props, "UUID"), IOCharUUID) {
				return true
			}
		}
	}
	return false
}

func managedObjects(conn *dbus.Conn) (map[dbus.ObjectPath]map[string]map[string]dbus.Variant, error) {
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	root := conn.Object(bluezBus, dbus.ObjectPath("/"))
	call := root.Call(objMgrIface+".GetManagedObjects", 0)
	if call.Err != nil {
		return nil, fmt.Errorf("blemidi: get managed objects: %w", call.Err)
	}
	if err := call.Store(&objects); err != nil {
		return nil, fmt.Errorf("blemidi: decode managed objects: %w", err)
	}
	return objects, nil
}

func setProp(obj dbus.BusObject, iface, name string, val dbus.Variant) error {
	return obj.Call(propsIface+".Set", 0, iface, name, val).Err
}

func boolProp(obj dbus.BusObject, iface, name string) (bool, error) {
	v, err := obj.GetProperty(iface + "." + name)
	if err != nil {
		return false, err
	}
	b, _ := v.Value().(bool)
	return b, nil
}

func strProp(props map[string]dbus.Variant, name string) string {
	if v, ok := props[name]; ok {
		s, _ := v.Value().(string)
		return s
	}
	return ""
}

func boolPropMap(props map[string]dbus.Variant, name string) bool {
	if v, ok := props[name]; ok {
		b, _ := v.Value().(bool)
		return b
	}
	return false
}

func uuidProp(props map[string]dbus.Variant) []string {
	if v, ok := props["UUIDs"]; ok {
		u, _ := v.Value().([]string)
		return u
	}
	return nil
}

// deviceLabel prefers the Alias, falling back to the Name.
func deviceLabel(props map[string]dbus.Variant) string {
	if s := strProp(props, "Alias"); s != "" {
		return s
	}
	return strProp(props, "Name")
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
