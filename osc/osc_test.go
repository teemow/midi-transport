package osc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/teemow/midi-transport"
)

func TestResolveDefaultsPort(t *testing.T) {
	addr, err := resolve("127.0.0.1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if addr.Port != 10023 {
		t.Fatalf("port = %d, want 10023 (default)", addr.Port)
	}
	addr, err = resolve("127.0.0.1:9000")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if addr.Port != 9000 {
		t.Fatalf("port = %d, want 9000 (explicit)", addr.Port)
	}
}

func TestSendNotConnected(t *testing.T) {
	tr, _ := New()
	err := tr.Send(context.Background(), "127.0.0.1:10023",
		transport.Event{Kind: transport.OSCEvent, OSCAddr: "/x"})
	if err == nil {
		t.Fatalf("expected error sending to an unconnected endpoint")
	}
}

func TestSendRejectsMIDI(t *testing.T) {
	tr, _ := New()
	pc := fakeConsole(t, nil)
	defer func() { _ = pc.Close() }()
	ep := pc.LocalAddr().String()
	if err := tr.Connect(context.Background(), ep); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := tr.Send(context.Background(), ep, transport.Event{Kind: transport.MIDIEvent, Data: []byte{0xB0, 1, 2}}); err == nil {
		t.Fatalf("expected error sending a MIDI event over OSC")
	}
}

// fakeConsole starts a UDP server on loopback. For every received packet it
// invokes reply (if non-nil) with the parsed messages and the sender address,
// so a test can mimic the X32 echoing values / mirroring /xremote.
func fakeConsole(t *testing.T, reply func(pc *net.UDPConn, src *net.UDPAddr, msgs []decodedMessage)) *net.UDPConn {
	t.Helper()
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, src, err := pc.ReadFromUDP(buf)
			if err != nil {
				return
			}
			msgs, derr := decodePacket(buf[:n])
			if derr != nil {
				continue
			}
			if reply != nil {
				reply(pc, src, msgs)
			}
		}
	}()
	return pc
}

func TestListenReceivesXRemoteMirror(t *testing.T) {
	// The fake console mirrors a fader change back whenever it sees /xremote —
	// exactly how the X32 feedback path works.
	pc := fakeConsole(t, func(pc *net.UDPConn, src *net.UDPAddr, msgs []decodedMessage) {
		for _, m := range msgs {
			if m.addr == "/xremote" {
				reply, _ := encodeMessage("/ch/01/mix/fader", []any{float32(0.42)})
				_, _ = pc.WriteToUDP(reply, src)
			}
		}
	})
	defer func() { _ = pc.Close() }()

	tr, _ := New(WithXRemoteInterval(50 * time.Millisecond))
	ep := pc.LocalAddr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tr.Connect(ctx, ep); err != nil {
		t.Fatalf("connect: %v", err)
	}
	ch, err := tr.Listen(ctx, ep)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	select {
	case ev := <-ch:
		if ev.Kind != transport.OSCEvent || ev.OSCAddr != "/ch/01/mix/fader" {
			t.Fatalf("event = %+v", ev)
		}
		if len(ev.OSCArgs) != 1 {
			t.Fatalf("args = %#v", ev.OSCArgs)
		}
		if f, ok := ev.OSCArgs[0].(float32); !ok || f != 0.42 {
			t.Fatalf("arg = %#v, want float32 0.42", ev.OSCArgs[0])
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for an /xremote-mirrored event")
	}
}

func TestDisconnectStopsListen(t *testing.T) {
	pc := fakeConsole(t, nil)
	defer func() { _ = pc.Close() }()
	tr, _ := New(WithXRemoteInterval(time.Hour))
	ep := pc.LocalAddr().String()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tr.Connect(ctx, ep); err != nil {
		t.Fatalf("connect: %v", err)
	}
	ch, err := tr.Listen(ctx, ep)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := tr.Disconnect(ctx, ep); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	select {
	case _, open := <-ch:
		if open {
			// Draining a buffered event is fine; keep reading until closed.
			for range ch {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Listen channel did not close after Disconnect")
	}
}
