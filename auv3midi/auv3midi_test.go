package auv3midi

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

	"github.com/teemow/midi-transport"
	"github.com/teemow/midi-transport/midicontrol"
)

func TestID(t *testing.T) {
	if got := New(midicontrol.NewHub()).ID(); got != "auv3midi" {
		t.Fatalf("ID() = %q, want auv3midi", got)
	}
}

// TestSendNoBrain: with no brain connected the hub reports ErrNoBrain, which
// Send surfaces so the caller knows the channel is down.
func TestSendNoBrain(t *testing.T) {
	tr := New(midicontrol.NewHub())
	err := tr.Send(context.Background(), "brain", transport.Event{
		Kind: transport.MIDIEvent,
		Data: []byte{0xB0, 21, 64},
	})
	if err != midicontrol.ErrNoBrain {
		t.Fatalf("Send with no brain = %v, want ErrNoBrain", err)
	}
}

// TestSendSkipsUnsendable: non-MIDI events and MIDI the brain protocol cannot
// carry are dropped silently (no error), even with no brain connected, so they
// never surface a spurious ErrNoBrain.
func TestSendSkipsUnsendable(t *testing.T) {
	tr := New(midicontrol.NewHub())
	cases := []transport.Event{
		{Kind: transport.OSCEvent, OSCAddr: "/foo"},
		{Kind: transport.MIDIEvent, Data: []byte{0xE0, 0, 64}}, // pitch bend
		{Kind: transport.MIDIEvent, Data: nil},                 // empty
	}
	for _, ev := range cases {
		if err := tr.Send(context.Background(), "brain", ev); err != nil {
			t.Fatalf("Send(%v) = %v, want nil (skipped)", ev, err)
		}
	}
}

// TestListenClosesOnCtx: the brain channel is outbound-only, so Listen yields no
// events and closes when the context is cancelled.
func TestListenClosesOnCtx(t *testing.T) {
	tr := New(midicontrol.NewHub())
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := tr.Listen(ctx, "brain")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected no events from the brain channel")
		}
	case <-time.After(time.Second):
		t.Fatal("Listen channel did not close on ctx cancel")
	}
}

// TestSendRoutesToBrain dials the receiver as a real WebSocket client (standing
// in for ProbeMidiBrain) and asserts a rendered MIDI event is decoded into a
// command frame and pushed through the hub to the brain.
func TestSendRoutesToBrain(t *testing.T) {
	hub := midicontrol.NewHub()
	var connects atomic.Int32

	mux := http.NewServeMux()
	midicontrol.Register(mux, hub, midicontrol.Callbacks{
		OnConnect: func(string) { connects.Add(1) },
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
	defer func() { _ = c.Close(websocket.StatusNormalClosure, "done") }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && connects.Load() != 1 {
		time.Sleep(10 * time.Millisecond)
	}
	if !hub.Connected() {
		t.Fatal("expected hub to report connected")
	}

	if !discovered(t, ctx, New(hub)) {
		t.Fatal("Discover did not surface the connected brain")
	}

	tr := New(hub)
	// A control change on channel 3 (status 0xB2).
	if err := tr.Send(ctx, "brain", transport.Event{
		Kind: transport.MIDIEvent,
		Data: []byte{0xB2, 102, 64},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("frame type = %v, want text", typ)
	}
	var got midicontrol.Command
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "cc" || got.Channel != 3 || got.Controller != 102 || got.Value != 64 {
		t.Fatalf("command not delivered as sent: %+v", got)
	}
}

func discovered(t *testing.T, ctx context.Context, tr *Transport) bool {
	t.Helper()
	eps, err := tr.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, ep := range eps {
		if ep.Transport == "auv3midi" && ep.Connected {
			return true
		}
	}
	return false
}
