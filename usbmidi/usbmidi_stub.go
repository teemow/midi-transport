//go:build !cgo

// Package usbmidi's pure-Go (CGO_ENABLED=0) stub. The real backend (usbmidi.go)
// links libasound via the gomidi rtmidi driver and therefore requires CGO; in a
// CGO-less build the transport is present but inert so the daemon still starts
// (USB is the bonus/verification path, not the primary one). Production release
// builds set CGO_ENABLED=1, so the real backend is the one normally compiled.
package usbmidi

import (
	"context"
	"errors"

	"github.com/teemow/midi-transport"
)

// errNoCGO is returned by the operations that need the ALSA driver.
var errNoCGO = errors.New("usbmidi: requires a CGO build (CGO_ENABLED=1) with ALSA")

// Transport is the inert USB-MIDI backend used in pure-Go builds.
type Transport struct{}

// New returns the stub USB-MIDI transport.
func New() (*Transport, error) { return &Transport{}, nil }

func (t *Transport) ID() string { return "usbmidi" }

func (t *Transport) Discover(ctx context.Context) ([]transport.Endpoint, error) {
	return nil, errNoCGO
}

func (t *Transport) Pair(ctx context.Context, endpointID string) error { return nil }

func (t *Transport) Connect(ctx context.Context, endpointID string) error { return errNoCGO }

func (t *Transport) Disconnect(ctx context.Context, endpointID string) error { return nil }

func (t *Transport) Send(ctx context.Context, endpointID string, ev transport.Event) error {
	return errNoCGO
}

func (t *Transport) Listen(ctx context.Context, endpointID string) (<-chan transport.Event, error) {
	return nil, errNoCGO
}
