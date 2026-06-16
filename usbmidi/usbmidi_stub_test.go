//go:build !cgo

package usbmidi

import (
	"context"
	"testing"

	"github.com/teemow/midi-transport"
)

func TestStubReportsNoCGO(t *testing.T) {
	tr, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if tr.ID() != "usbmidi" {
		t.Fatalf("id = %q", tr.ID())
	}
	ctx := context.Background()
	if _, err := tr.Discover(ctx); err == nil {
		t.Fatalf("Discover should report the CGO requirement")
	}
	if err := tr.Connect(ctx, "x"); err == nil {
		t.Fatalf("Connect should report the CGO requirement")
	}
	if err := tr.Send(ctx, "x", transport.Event{Kind: transport.MIDIEvent}); err == nil {
		t.Fatalf("Send should report the CGO requirement")
	}
	if _, err := tr.Listen(ctx, "x"); err == nil {
		t.Fatalf("Listen should report the CGO requirement")
	}
	// Pair / Disconnect are inert no-ops even in the stub.
	if err := tr.Pair(ctx, "x"); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if err := tr.Disconnect(ctx, "x"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}
