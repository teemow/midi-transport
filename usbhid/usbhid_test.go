//go:build linux

package usbhid

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/teemow/midi-transport"
)

func TestParseVIDPID(t *testing.T) {
	vid, pid, err := parseVIDPID("29A4:0400")
	if err != nil {
		t.Fatalf("parseVIDPID: %v", err)
	}
	if vid != 0x29A4 || pid != 0x0400 {
		t.Fatalf("got %04X:%04X, want 29A4:0400", vid, pid)
	}
	if _, _, err := parseVIDPID("nope"); err == nil {
		t.Fatalf("expected error for malformed id")
	}
	if _, _, err := parseVIDPID("ZZZZ:0400"); err == nil {
		t.Fatalf("expected error for non-hex VID")
	}
}

func TestHidInfo(t *testing.T) {
	// Build a fake /sys/class/hidraw/hidrawN/device/uevent layout.
	dir := t.TempDir()
	devDir := filepath.Join(dir, "device")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	uevent := "DRIVER=hidraw\nHID_ID=0003:000029A4:00000400\nHID_NAME=Source Audio EQ2\n"
	if err := os.WriteFile(filepath.Join(devDir, "uevent"), []byte(uevent), 0o644); err != nil {
		t.Fatal(err)
	}
	vid, pid, name, ok := hidInfo(dir)
	if !ok {
		t.Fatalf("hidInfo: expected ok")
	}
	if vid != 0x29A4 || pid != 0x0400 {
		t.Fatalf("got %04X:%04X, want 29A4:0400", vid, pid)
	}
	if name != "Source Audio EQ2" {
		t.Fatalf("name = %q", name)
	}

	// A node with no readable uevent is skipped (ok=false).
	if _, _, _, ok := hidInfo(t.TempDir()); ok {
		t.Fatalf("expected ok=false for a node without uevent")
	}
}

func TestResolveNodeDevPath(t *testing.T) {
	// A literal /dev/hidraw path is returned verbatim (no sysfs lookup).
	got, err := resolveNode("/dev/hidraw7")
	if err != nil {
		t.Fatalf("resolveNode: %v", err)
	}
	if got != "/dev/hidraw7" {
		t.Fatalf("got %q, want /dev/hidraw7", got)
	}
}

func TestUnconnectedEndpointErrors(t *testing.T) {
	tr, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if tr.ID() != "usbhid" {
		t.Fatalf("id = %q", tr.ID())
	}
	ctx := context.Background()
	if err := tr.Send(ctx, "29A4:0400", transport.Event{Kind: transport.RawEvent, Data: []byte{0x36}}); err == nil {
		t.Fatalf("Send to unconnected endpoint should error")
	}
	if _, err := tr.Listen(ctx, "29A4:0400"); err == nil {
		t.Fatalf("Listen on unconnected endpoint should error")
	}
	// Wrong event kind is rejected even when (hypothetically) connected.
	tr.ports["x"] = &hidPort{fd: -1, path: "/dev/null"}
	if err := tr.Send(ctx, "x", transport.Event{Kind: transport.MIDIEvent}); err == nil {
		t.Fatalf("Send should reject a non-raw event")
	}
	// Pair / Disconnect are inert no-ops.
	if err := tr.Pair(ctx, "29A4:0400"); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if err := tr.Disconnect(ctx, "29A4:0400"); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
}
