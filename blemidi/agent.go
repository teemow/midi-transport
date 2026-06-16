package blemidi

import (
	"fmt"
	"log"

	"github.com/godbus/dbus/v5"
)

// agentPath is where we export our Agent1 on the connection's object tree.
const agentPath = dbus.ObjectPath("/midi/mcp/agent")

// pairingAgent implements org.bluez.Agent1 with a NoInputNoOutput capability:
// it accepts every pairing request without prompting. Phase A confirmed the
// CME WIDI BLE-MIDI dongles pair "Just Works" (no PIN/passkey callback fires),
// so the agent mostly exists to satisfy BlueZ's requirement that a registered
// agent be present and to gracefully accept the edge cases (legacy PIN,
// passkey, service authorization) without a human in the loop.
//
// The daemon is a local, single-user process; auto-accepting is the intended
// behaviour (the user explicitly asked to pair a known dongle).
type pairingAgent struct{}

func (pairingAgent) Release() *dbus.Error { return nil }

func (pairingAgent) RequestPinCode(dbus.ObjectPath) (string, *dbus.Error) {
	log.Printf("blemidi: agent RequestPinCode -> 0000")
	return "0000", nil
}

func (pairingAgent) DisplayPinCode(_ dbus.ObjectPath, pin string) *dbus.Error {
	log.Printf("blemidi: agent DisplayPinCode %s", pin)
	return nil
}

func (pairingAgent) RequestPasskey(dbus.ObjectPath) (uint32, *dbus.Error) {
	log.Printf("blemidi: agent RequestPasskey -> 0")
	return 0, nil
}

func (pairingAgent) DisplayPasskey(_ dbus.ObjectPath, passkey uint32, entered uint16) *dbus.Error {
	log.Printf("blemidi: agent DisplayPasskey %06d (entered %d)", passkey, entered)
	return nil
}

func (pairingAgent) RequestConfirmation(_ dbus.ObjectPath, passkey uint32) *dbus.Error {
	log.Printf("blemidi: agent RequestConfirmation %06d -> accept", passkey)
	return nil
}

func (pairingAgent) RequestAuthorization(dbus.ObjectPath) *dbus.Error {
	log.Printf("blemidi: agent RequestAuthorization -> accept")
	return nil
}

func (pairingAgent) AuthorizeService(_ dbus.ObjectPath, uuid string) *dbus.Error {
	log.Printf("blemidi: agent AuthorizeService %s -> accept", uuid)
	return nil
}

func (pairingAgent) Cancel() *dbus.Error {
	log.Printf("blemidi: agent Cancel")
	return nil
}

// registerAgent exports the pairing agent on conn and registers it as the
// default BlueZ agent. The returned function unregisters and unexports it.
func registerAgent(conn *dbus.Conn, capability string) (func(), error) {
	if err := conn.Export(pairingAgent{}, agentPath, agentIface); err != nil {
		return nil, fmt.Errorf("blemidi: export agent: %w", err)
	}
	mgr := conn.Object(bluezBus, dbus.ObjectPath("/org/bluez"))
	if call := mgr.Call(agentManagerIface+".RegisterAgent", 0, agentPath, capability); call.Err != nil {
		_ = conn.Export(nil, agentPath, agentIface)
		return nil, fmt.Errorf("blemidi: register agent: %w", call.Err)
	}
	// Best-effort: pairing can still succeed with a registered (non-default)
	// agent if another agent already owns the default slot.
	if call := mgr.Call(agentManagerIface+".RequestDefaultAgent", 0, agentPath); call.Err != nil {
		log.Printf("blemidi: RequestDefaultAgent: %v (continuing)", call.Err)
	}
	return func() {
		mgr.Call(agentManagerIface+".UnregisterAgent", 0, agentPath)
		_ = conn.Export(nil, agentPath, agentIface)
	}, nil
}
