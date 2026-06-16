// Package osc implements the OSC/UDP transport, used for the Behringer X32.
// Endpoints are host:port targets (port defaults to 10023, the X32's OSC
// server); there is no pairing. Send marshals a transport.Event's OSCAddr +
// OSCArgs into an OSC packet; Listen reuses the same UDP socket to receive the
// console's replies — and periodically (re)sends /xremote so the X32 mirrors
// every parameter change back to us, which is the cheap feedback/reconcile
// path (see docs/research/x32.md).
package osc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/teemow/midi-transport"
)

// defaultPort is the X32's OSC/UDP server port (X-Air/XR uses 10024 — a
// different surface). Appended to an endpoint id that omits a port.
const defaultPort = "10023"

// defaultXRemoteInterval is how often Listen re-sends /xremote. The console
// stops mirroring changes if it does not hear from us within ~10s, so we renew
// comfortably inside that window.
const defaultXRemoteInterval = 9 * time.Second

// Transport is the OSC/UDP backend.
type Transport struct {
	xremoteEvery time.Duration

	mu    sync.Mutex
	conns map[string]*net.UDPConn // endpoint id -> connected UDP socket
}

// Option configures the transport.
type Option func(*Transport)

// WithXRemoteInterval overrides how often Listen renews /xremote.
func WithXRemoteInterval(d time.Duration) Option {
	return func(t *Transport) {
		if d > 0 {
			t.xremoteEvery = d
		}
	}
}

// New returns an OSC transport.
func New(opts ...Option) (*Transport, error) {
	t := &Transport{
		xremoteEvery: defaultXRemoteInterval,
		conns:        map[string]*net.UDPConn{},
	}
	for _, o := range opts {
		o(t)
	}
	return t, nil
}

func (t *Transport) ID() string { return "osc" }

// Discover is a no-op for OSC: endpoints are configured as host:port via
// bindings, not auto-enumerated. (A LAN /info broadcast probe would need raw
// SO_BROADCAST handling and a known subnet; reachability is instead validated
// lazily — Connect resolves the address and Listen's /xremote elicits replies.)
func (t *Transport) Discover(ctx context.Context) ([]transport.Endpoint, error) {
	return nil, nil
}

// Pair is a no-op for OSC.
func (t *Transport) Pair(ctx context.Context, endpointID string) error { return nil }

// Connect resolves endpointID (host[:port]) and opens a connected UDP socket.
// UDP is connectionless, so this never round-trips the console; it only fails
// if the address cannot be resolved.
func (t *Transport) Connect(ctx context.Context, endpointID string) error {
	addr, err := resolve(endpointID)
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("osc: dial %s: %w", addr, err)
	}
	t.mu.Lock()
	if old := t.conns[endpointID]; old != nil {
		_ = old.Close()
	}
	t.conns[endpointID] = conn
	t.mu.Unlock()
	return nil
}

// Disconnect closes the endpoint's UDP socket, which also unblocks any Listen
// read loop bound to it.
func (t *Transport) Disconnect(ctx context.Context, endpointID string) error {
	t.mu.Lock()
	conn := t.conns[endpointID]
	delete(t.conns, endpointID)
	t.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// Send marshals ev.OSCAddr + ev.OSCArgs into an OSC packet and writes it.
func (t *Transport) Send(ctx context.Context, endpointID string, ev transport.Event) error {
	if ev.Kind != transport.OSCEvent {
		return fmt.Errorf("osc: cannot send %v event (OSC only)", ev.Kind)
	}
	conn, err := t.connFor(endpointID)
	if err != nil {
		return err
	}
	pkt, err := encodeMessage(ev.OSCAddr, ev.OSCArgs)
	if err != nil {
		return err
	}
	if _, err := conn.Write(pkt); err != nil {
		return fmt.Errorf("osc: send to %s: %w", endpointID, err)
	}
	return nil
}

// Listen receives OSC replies from the console on the same socket it sends
// from, parses each into a transport.Event, and periodically (re)sends
// /xremote so the X32 mirrors parameter changes back to us. The returned
// channel closes when ctx is done.
func (t *Transport) Listen(ctx context.Context, endpointID string) (<-chan transport.Event, error) {
	conn, err := t.connFor(endpointID)
	if err != nil {
		return nil, err
	}
	out := make(chan transport.Event, 64)

	go t.xremoteLoop(ctx, conn)
	go func() {
		defer close(out)
		buf := make([]byte, 64*1024)
		for {
			if ctx.Err() != nil {
				return
			}
			// Bounded read deadline so we can notice ctx cancellation without
			// closing the shared socket (Disconnect owns that).
			_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
			n, err := conn.Read(buf)
			if err != nil {
				var nerr net.Error
				if errors.As(err, &nerr) && nerr.Timeout() {
					continue
				}
				return // socket closed (Disconnect) or fatal error
			}
			msgs, derr := decodePacket(buf[:n])
			if derr != nil {
				continue // ignore malformed packets
			}
			for _, m := range msgs {
				ev := transport.Event{Kind: transport.OSCEvent, OSCAddr: m.addr, OSCArgs: m.args}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// xremoteLoop sends /xremote immediately and then every xremoteEvery until ctx
// is done, keeping the console's change-mirroring subscription alive.
func (t *Transport) xremoteLoop(ctx context.Context, conn *net.UDPConn) {
	pkt, err := encodeMessage("/xremote", nil)
	if err != nil {
		return
	}
	ticker := time.NewTicker(t.xremoteEvery)
	defer ticker.Stop()
	for {
		// Stop promptly when the socket is closed (Disconnect) instead of
		// spinning writes at a dead fd until ctx is cancelled.
		if _, err := conn.Write(pkt); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (t *Transport) connFor(endpointID string) (*net.UDPConn, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	conn := t.conns[endpointID]
	if conn == nil {
		return nil, fmt.Errorf("osc: endpoint %s not connected", endpointID)
	}
	return conn, nil
}

// resolve turns a host[:port] endpoint id into a UDP address, defaulting the
// port to the X32's 10023 when omitted.
func resolve(endpointID string) (*net.UDPAddr, error) {
	if endpointID == "" {
		return nil, fmt.Errorf("osc: empty endpoint (want host[:port])")
	}
	hostport := endpointID
	if _, _, err := net.SplitHostPort(endpointID); err != nil {
		// No port (or a bare IPv6) — append the default and retry.
		hostport = net.JoinHostPort(strings.Trim(endpointID, "[]"), defaultPort)
	}
	addr, err := net.ResolveUDPAddr("udp", hostport)
	if err != nil {
		return nil, fmt.Errorf("osc: resolve %q: %w", endpointID, err)
	}
	return addr, nil
}
