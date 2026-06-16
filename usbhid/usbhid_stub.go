//go:build !linux

// Package usbhid's non-Linux stub. The real backend (usbhid.go) reads/writes
// Linux hidraw nodes (/sys/class/hidraw + /dev/hidrawN), which only exist on
// Linux; on other platforms the transport is present but inert so the daemon
// still starts (vendor-HID readback is a Linux-only path). Pair/Disconnect are
// inert no-ops as on the real backend; the data-path operations report that
// hidraw is unavailable.
package usbhid

import (
	"context"
	"errors"

	"github.com/teemow/midi-transport"
)

// errNoHidraw is returned by the operations that need Linux hidraw.
var errNoHidraw = errors.New("usbhid: requires Linux (hidraw is not available on this platform)")

// Transport is the inert vendor-HID backend used on non-Linux builds.
type Transport struct{}

// New returns the stub usbhid transport.
func New() (*Transport, error) { return &Transport{}, nil }

func (t *Transport) ID() string { return "usbhid" }

func (t *Transport) Discover(ctx context.Context) ([]transport.Endpoint, error) {
	return nil, errNoHidraw
}

func (t *Transport) Pair(ctx context.Context, endpointID string) error { return nil }

func (t *Transport) Connect(ctx context.Context, endpointID string) error { return errNoHidraw }

func (t *Transport) Disconnect(ctx context.Context, endpointID string) error { return nil }

func (t *Transport) Send(ctx context.Context, endpointID string, ev transport.Event) error {
	return errNoHidraw
}

func (t *Transport) Listen(ctx context.Context, endpointID string) (<-chan transport.Event, error) {
	return nil, errNoHidraw
}
