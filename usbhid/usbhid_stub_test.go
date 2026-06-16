//go:build !linux

package usbhid

import (
	"context"
	"testing"

	"github.com/teemow/midi-transport"
)

func TestStubReportsNoHidraw(t *testing.T) {
	tr, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if tr.ID() != "usbhid" {
		t.Fatalf("id = %q", tr.ID())
	}
	ctx := context.Background()
	if _, err := tr.Discover(ctx); err == nil {
		t.Fatalf("Discover should report hidraw is unavailable")
	}
	if err := tr.Connect(ctx, "29A4:0400"); err == nil {
		t.Fatalf("Connect should report hidraw is unavailable")
	}
	if err := tr.Send(ctx, "29A4:0400", transport.Event{Kind: transport.RawEvent}); err == nil {
		t.Fatalf("Send should report hidraw is unavailable")
	}
	if _, err := tr.Listen(ctx, "29A4:0400"); err == nil {
		t.Fatalf("Listen should report hidraw is unavailable")
	}
	if err := tr.Pair(ctx, "29A4:0400"); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if err := tr.Disconnect(ctx, "29A4:0400"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}
