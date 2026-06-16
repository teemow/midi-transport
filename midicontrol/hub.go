// Package midicontrol is the off-MCP LAN listener that terminates the
// ProbeMidiBrain control channel (github.com/teemow/auv3-probe) and lets an
// agent push MIDI into the rig — notes, CC, program change and transport — by
// having the brain AUv3 emit them on its host MIDI-out at render time.
//
// It is the symmetric counterpart of internal/audiotap: where the audio tap is
// the agent's "ears" (an inbound stream the daemon receives), the MIDI control
// channel is the agent's "hands" (an outbound stream the daemon pushes). Both
// are SEPARATE listeners from the loopback-only MCP endpoint (the iPad cannot
// reach loopback) and both reuse the iPad app's DaemonClient/webSocketURL.
//
// The brain is the WebSocket *client* (it dials the daemon, like the tap), so
// the daemon holds at most one live connection and writes command frames to it.
// Nothing is written to disk; the channel is volatile rig control.
package midicontrol

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Command is a single control frame pushed to the connected ProbeMidiBrain. The
// brain decodes it off its networking thread, enqueues it on a lock-free ring,
// and emits the corresponding MIDI 1.0 bytes from its realtime render block.
//
// Type is one of: "noteOn", "noteOff", "cc", "pc", "transport". Channel is
// 1-16 (ignored for transport). The remaining fields are interpreted per Type.
type Command struct {
	Type string `json:"type"`

	Channel int `json:"channel,omitempty"`

	// noteOn / noteOff. Not omitempty: note 0 and velocity 0 are meaningful
	// (note 0 = C-1; velocity 0 = note-off), so the field must stay on the wire
	// rather than relying on the brain's zero-default.
	Note     int `json:"note"`
	Velocity int `json:"velocity"`

	// cc. Not omitempty: controller 0 (Bank Select MSB) and value 0 (knob to
	// minimum) are common and meaningful.
	Controller int `json:"controller"`
	Value      int `json:"value"`

	// pc. Not omitempty: program 0 is a valid program.
	Program int `json:"program"`

	// transport: "start" | "stop" | "continue"
	Action string `json:"action,omitempty"`
}

// Hub tracks the single connected ProbeMidiBrain and serializes command frames
// to it. A second connection simply replaces the first (one brain per rig). All
// state access is mutex-guarded; the WebSocket read loop registers/unregisters
// the connection, MCP tool calls push commands, on different goroutines.
type Hub struct {
	mu          sync.Mutex
	conn        *websocket.Conn
	remote      string
	connectedAt time.Time
	lastSendAt  time.Time
	sent        int64
}

// NewHub returns an empty hub with no brain connected.
func NewHub() *Hub {
	return &Hub{}
}

// Connect registers the live brain connection from remote, replacing any
// previous one (which is closed by its own read loop on the way out).
func (h *Hub) Connect(remote string, conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conn = conn
	h.remote = remote
	h.connectedAt = time.Now()
}

// Disconnect clears the live connection if it is still the one passed in. The
// guard avoids a late read loop tearing down a newer connection that replaced
// it.
func (h *Hub) Disconnect(conn *websocket.Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == conn {
		h.conn = nil
		h.remote = ""
	}
}

// Connected reports whether a brain is currently connected.
func (h *Hub) Connected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn != nil
}

// Remote returns the address of the connected brain (empty when none).
func (h *Hub) Remote() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.remote
}

// ErrNoBrain is returned by Send when no ProbeMidiBrain is connected.
var ErrNoBrain = errNoBrain{}

type errNoBrain struct{}

func (errNoBrain) Error() string {
	return "no ProbeMidiBrain connected to the /midi-control channel"
}

// Send marshals cmd and writes it to the connected brain as a TEXT frame. It
// returns ErrNoBrain when nothing is connected so callers can fall back to a
// hardware transport. Writes are serialized under the hub mutex; command rate
// is agent-driven (low), so holding the lock across the write is acceptable.
func (h *Hub) Send(ctx context.Context, cmd Command) error {
	return h.SendJSON(ctx, cmd)
}

// SendJSON marshals an arbitrary frame (e.g. a ControlSurface manifest) and
// writes it to the connected brain as a TEXT frame, with the same ErrNoBrain
// and serialization semantics as Send. The brain drops unknown frame types, so
// pushing newer frames to an older brain is safe.
func (h *Hub) SendJSON(ctx context.Context, frame any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.conn == nil {
		return ErrNoBrain
	}
	if err := h.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	h.lastSendAt = time.Now()
	h.sent++
	return nil
}

// Status is the read-only view of the control channel.
type Status struct {
	Connected      bool   `json:"connected"`
	Remote         string `json:"remote,omitempty"`
	ConnectedForMS int64  `json:"connected_for_ms,omitempty"`
	LastSendAgeMS  int64  `json:"last_send_age_ms,omitempty"`
	Sent           int64  `json:"sent"`
}

// Status returns a snapshot of the channel state for reporting.
func (h *Hub) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := Status{
		Connected: h.conn != nil,
		Remote:    h.remote,
		Sent:      h.sent,
	}
	now := time.Now()
	if h.conn != nil && !h.connectedAt.IsZero() {
		st.ConnectedForMS = now.Sub(h.connectedAt).Milliseconds()
	}
	if !h.lastSendAt.IsZero() {
		st.LastSendAgeMS = now.Sub(h.lastSendAt).Milliseconds()
	}
	return st
}
