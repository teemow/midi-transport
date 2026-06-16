package midicontrol

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestHubPushesCommands dials the receiver as a real WebSocket client (standing
// in for ProbeMidiBrain), then asserts that hub.Send delivers command frames to
// it and that the connect/disconnect callbacks fire.
func TestHubPushesCommands(t *testing.T) {
	hub := NewHub()
	var connects, disconnects atomic.Int32

	mux := http.NewServeMux()
	Register(mux, hub, Callbacks{
		OnConnect:    func(string) { connects.Add(1) },
		OnDisconnect: func(string) { disconnects.Add(1) },
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/midi-control"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Wait for the server to register the connection. The handler calls
	// hub.Connect before the onConnect callback, so gate on the connect counter
	// (which implies Connected) to avoid racing the callback.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if connects.Load() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if connects.Load() != 1 {
		t.Fatalf("connects = %d, want 1", connects.Load())
	}
	if !hub.Connected() {
		t.Fatal("expected hub to report connected")
	}

	// Push a note-on and read it back on the client side.
	if err := hub.Send(ctx, Command{Type: "noteOn", Channel: 1, Note: 60, Velocity: 100}); err != nil {
		t.Fatalf("send: %v", err)
	}
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("frame type = %v, want text", typ)
	}
	var got Command
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "noteOn" || got.Note != 60 || got.Velocity != 100 || got.Channel != 1 {
		t.Fatalf("command not delivered as sent: %+v", got)
	}

	if st := hub.Status(); st.Sent != 1 {
		t.Fatalf("Status.Sent = %d, want 1", st.Sent)
	}

	_ = c.Close(websocket.StatusNormalClosure, "done")

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if disconnects.Load() == 1 && !hub.Connected() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if disconnects.Load() != 1 {
		t.Fatalf("disconnects = %d, want 1", disconnects.Load())
	}
	if hub.Connected() {
		t.Fatal("expected hub to report disconnected after close")
	}

	// With no brain connected, Send reports ErrNoBrain so callers can fall back.
	if err := hub.Send(ctx, Command{Type: "transport", Action: "stop"}); err != ErrNoBrain {
		t.Fatalf("Send after disconnect = %v, want ErrNoBrain", err)
	}
}

// TestHubSendJSONControlSurface pushes a controlSurface manifest frame through
// the hub and asserts the brain-side client receives it verbatim — the
// daemon→brain manifest path used after every session-rig (auto-)import.
func TestHubSendJSONControlSurface(t *testing.T) {
	hub := NewHub()
	mux := http.NewServeMux()
	Register(mux, hub, Callbacks{})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/midi-control", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !hub.Connected() {
		time.Sleep(10 * time.Millisecond)
	}
	if !hub.Connected() {
		t.Fatal("hub never registered the connection")
	}

	min, max := 0, 127
	frame := ControlSurface{
		Type:    ControlSurfaceType,
		Session: "rig",
		Title:   "Rig",
		Devices: []SurfaceDevice{{
			Name: "aum",
			Controls: []SurfaceControl{
				{Name: "gitarre_level", Widget: "fader", Msg: SurfaceMsg{Type: "cc", Channel: 3, Number: 7}, Min: &min, Max: &max},
				{Name: "gitarre_mute", Widget: "toggle", Msg: SurfaceMsg{Type: "cc", Channel: 3, Number: 8},
					Values: []SurfaceValue{{Label: "unmute", Value: 0}, {Label: "mute", Value: 127}}},
				{Name: "preset_lead", Widget: "preset", Msg: SurfaceMsg{Type: "pc", Channel: 2, Number: 5}},
			},
		}},
	}
	if err := hub.SendJSON(ctx, frame); err != nil {
		t.Fatalf("SendJSON: %v", err)
	}

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got ControlSurface
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != ControlSurfaceType || got.Session != "rig" || len(got.Devices) != 1 {
		t.Fatalf("frame not delivered as sent: %+v", got)
	}
	ctrls := got.Devices[0].Controls
	if len(ctrls) != 3 || ctrls[0].Widget != "fader" || ctrls[1].Widget != "toggle" || ctrls[2].Widget != "preset" {
		t.Fatalf("controls not delivered as sent: %+v", ctrls)
	}
	if ctrls[0].Msg.Channel != 3 || ctrls[0].Min == nil || *ctrls[0].Max != 127 {
		t.Fatalf("fader control mangled: %+v", ctrls[0])
	}
	if st := hub.Status(); st.Sent != 1 {
		t.Fatalf("Status.Sent = %d, want 1", st.Sent)
	}

	// With no brain connected, SendJSON reports ErrNoBrain like Send.
	_ = c.Close(websocket.StatusNormalClosure, "done")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hub.Connected() {
		time.Sleep(10 * time.Millisecond)
	}
	if err := hub.SendJSON(ctx, frame); err != ErrNoBrain {
		t.Fatalf("SendJSON after disconnect = %v, want ErrNoBrain", err)
	}
}

// TestReceiverDispatchesSessionSwitchFrames sends brain→daemon frames and
// asserts only well-formed sessionSwitch frames reach the callback — the
// upstream half of the cross-session switcher (the brain emitted the PC into
// AUM locally and tells the daemon to follow).
func TestReceiverDispatchesSessionSwitchFrames(t *testing.T) {
	hub := NewHub()
	programs := make(chan int, 4)
	mux := http.NewServeMux()
	Register(mux, hub, Callbacks{OnSessionSwitch: func(program int) { programs <- program }})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/midi-control", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()

	// Noise first: a non-JSON frame and an unknown type are drained silently.
	if err := c.Write(ctx, websocket.MessageText, []byte("not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"ack","program":9}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The real thing.
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"sessionSwitch","program":3}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case got := <-programs:
		if got != 3 {
			t.Fatalf("dispatched program = %d, want 3", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sessionSwitch frame never dispatched")
	}
	select {
	case got := <-programs:
		t.Fatalf("unexpected extra dispatch (program %d) — noise frames must be ignored", got)
	case <-time.After(100 * time.Millisecond):
	}
}
