//go:build linux

// Package usbhid is the vendor-HID transport: it moves raw HID reports to and
// from a Linux hidraw node (/dev/hidrawN), with no MIDI or OSC framing. It is
// the read/write data path for pedals whose editor protocol rides a vendor HID
// interface rather than USB-MIDI — e.g. the Source Audio Neuro channel (EQ2,
// VID:PID 29A4:0400) and the Two Notes Torpedo Remote pipe (Opus, 0483:A334).
// The codec layer (package usbcodec) builds/decodes the report bytes; this
// transport only carries them.
//
// Endpoints are keyed by "VID:PID" (uppercase 4-hex each, e.g. "29A4:0400");
// Connect also accepts a literal /dev/hidrawN path. There is no pairing.
//
// It needs no cgo: it talks to hidraw directly via golang.org/x/sys/unix
// (open/read/write/poll on the device fd), the same approach the cmd/usb-probe
// spike proved. Non-Linux builds compile the inert stub in usbhid_stub.go.
package usbhid

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/teemow/midi-transport"
	"golang.org/x/sys/unix"
)

// reportLen is the read buffer size for an input report. 64 covers every
// vendor pipe we target (the EQ2's 38-byte Neuro report and the Opus' 64-byte
// Torpedo pipe both fit); the actual report length is whatever read returns.
const reportLen = 64

// pollInterval bounds how long a Listen read blocks before re-checking ctx, so
// a cancelled context tears the listener down promptly.
const pollInterval = 200 * time.Millisecond

// Transport is the vendor-HID backend over Linux hidraw.
type Transport struct {
	mu    sync.Mutex
	ports map[string]*hidPort // endpointID -> open node
}

// hidPort is one opened hidraw fd plus the node path it resolved to.
type hidPort struct {
	fd   int
	path string
}

// New returns a usbhid transport.
func New() (*Transport, error) {
	return &Transport{ports: map[string]*hidPort{}}, nil
}

func (t *Transport) ID() string { return "usbhid" }

// Discover enumerates /sys/class/hidraw/* and reports one endpoint per node,
// keyed by its "VID:PID". Paired is always true (HID needs no bonding) and
// Connected reflects whether we currently hold the node open.
func (t *Transport) Discover(ctx context.Context) ([]transport.Endpoint, error) {
	nodes, _ := filepath.Glob("/sys/class/hidraw/hidraw*")
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []transport.Endpoint
	for _, n := range nodes {
		vid, pid, name, ok := hidInfo(n)
		if !ok {
			continue
		}
		id := fmt.Sprintf("%04X:%04X", vid, pid)
		if name == "" {
			name = id
		}
		out = append(out, transport.Endpoint{
			ID:        id,
			Name:      name,
			Transport: t.ID(),
			Paired:    true,
			Connected: t.ports[id] != nil,
		})
	}
	return out, nil
}

// Pair is a no-op for HID.
func (t *Transport) Pair(ctx context.Context, endpointID string) error { return nil }

// Connect opens the hidraw node for the endpoint so it is ready for
// Send/Listen. endpointID is either a "VID:PID" (resolved against
// /sys/class/hidraw) or a literal /dev/hidrawN path. It is idempotent.
func (t *Transport) Connect(ctx context.Context, endpointID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ports[endpointID] != nil {
		return nil
	}
	path, err := resolveNode(endpointID)
	if err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("usbhid: open %s: %w (hidraw nodes are root-only; grant access with `sudo setfacl -m u:$USER:rw %s`)", path, err, path)
	}
	t.ports[endpointID] = &hidPort{fd: fd, path: path}
	return nil
}

// Disconnect closes the endpoint's hidraw node.
func (t *Transport) Disconnect(ctx context.Context, endpointID string) error {
	t.mu.Lock()
	p := t.ports[endpointID]
	delete(t.ports, endpointID)
	t.mu.Unlock()
	if p == nil {
		return nil
	}
	return unix.Close(p.fd)
}

// Send writes one raw HID output report. hidraw expects byte 0 to be the
// report id (0x00 for the unnumbered reports these devices use), so the payload
// is prefixed with 0x00. Only RawEvent payloads are accepted.
func (t *Transport) Send(ctx context.Context, endpointID string, ev transport.Event) error {
	if ev.Kind != transport.RawEvent {
		return fmt.Errorf("usbhid: cannot send %v event (raw HID only)", ev.Kind)
	}
	t.mu.Lock()
	p := t.ports[endpointID]
	t.mu.Unlock()
	if p == nil {
		return fmt.Errorf("usbhid: endpoint %s not connected", endpointID)
	}
	buf := append([]byte{0x00}, ev.Data...)
	if _, err := unix.Write(p.fd, buf); err != nil {
		return fmt.Errorf("usbhid: write to %s: %w", endpointID, err)
	}
	return nil
}

// Listen streams inbound HID input reports from the endpoint as RawEvents. Each
// report is emitted verbatim (no report-id stripping). The channel closes when
// ctx is done.
func (t *Transport) Listen(ctx context.Context, endpointID string) (<-chan transport.Event, error) {
	t.mu.Lock()
	p := t.ports[endpointID]
	t.mu.Unlock()
	if p == nil {
		return nil, fmt.Errorf("usbhid: endpoint %s not connected", endpointID)
	}
	out := make(chan transport.Event, 64)
	go func() {
		defer close(out)
		for {
			if ctx.Err() != nil {
				return
			}
			rep, err := readReport(p.fd, pollInterval)
			if err != nil {
				return // node closed (e.g. Disconnect) or a fatal read error
			}
			if len(rep) == 0 {
				continue // poll timed out; re-check ctx
			}
			select {
			case out <- transport.Event{Kind: transport.RawEvent, Data: rep}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// readReport waits up to timeout for one input report on fd; returns a nil
// slice (and nil error) on timeout. Retries across signal interruptions.
func readReport(fd int, timeout time.Duration) ([]byte, error) {
	pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	var n int
	var err error
	for {
		n, err = unix.Poll(pfd, int(timeout.Milliseconds()))
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	// Surface device-gone conditions instead of attempting a read that would
	// fail (or block): a disconnected hidraw node wakes poll with POLLHUP/
	// POLLERR/POLLNVAL rather than POLLIN.
	if rev := pfd[0].Revents; rev&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
		return nil, fmt.Errorf("usbhid: poll error on fd (revents=0x%x; device disconnected?)", rev)
	} else if rev&unix.POLLIN == 0 {
		return nil, nil // woke without readable data; treat as a timeout
	}
	buf := make([]byte, reportLen)
	m, err := unix.Read(fd, buf)
	if err != nil {
		return nil, err
	}
	return buf[:m], nil
}

// resolveNode turns an endpoint id into a /dev/hidrawN path: a literal
// /dev/hidraw* path is used as-is; otherwise it is parsed as "VID:PID" and
// matched against /sys/class/hidraw.
func resolveNode(endpointID string) (string, error) {
	if strings.HasPrefix(endpointID, "/dev/hidraw") {
		return endpointID, nil
	}
	vid, pid, err := parseVIDPID(endpointID)
	if err != nil {
		return "", err
	}
	nodes, _ := filepath.Glob("/sys/class/hidraw/hidraw*")
	for _, n := range nodes {
		v, p, _, ok := hidInfo(n)
		if ok && v == vid && p == pid {
			return "/dev/" + filepath.Base(n), nil
		}
	}
	return "", fmt.Errorf("usbhid: no hidraw node for %04X:%04X (is the device connected?)", vid, pid)
}

// parseVIDPID parses "29A4:0400" into its vendor and product ids.
func parseVIDPID(s string) (vid, pid uint32, err error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("usbhid: bad endpoint id %q (want \"VID:PID\" or /dev/hidrawN)", s)
	}
	if _, err = fmt.Sscanf(parts[0], "%x", &vid); err != nil {
		return 0, 0, fmt.Errorf("usbhid: bad VID in %q: %w", s, err)
	}
	if _, err = fmt.Sscanf(parts[1], "%x", &pid); err != nil {
		return 0, 0, fmt.Errorf("usbhid: bad PID in %q: %w", s, err)
	}
	return vid, pid, nil
}

// hidInfo reads a /sys/class/hidraw/hidrawN node's uevent for its VID, PID and
// HID name. The uevent's HID_ID line is "bus:vendor:product" with 8-hex vendor
// and product fields (e.g. HID_ID=0003:000029A4:00000400).
func hidInfo(node string) (vid, pid uint32, name string, ok bool) {
	ue, err := os.ReadFile(filepath.Join(node, "device", "uevent"))
	if err != nil {
		return 0, 0, "", false
	}
	for _, line := range strings.Split(string(ue), "\n") {
		switch {
		case strings.HasPrefix(line, "HID_ID="):
			fields := strings.Split(strings.TrimPrefix(line, "HID_ID="), ":")
			if len(fields) == 3 {
				var v, p uint64
				if _, e1 := fmt.Sscanf(fields[1], "%x", &v); e1 == nil {
					if _, e2 := fmt.Sscanf(fields[2], "%x", &p); e2 == nil {
						vid, pid, ok = uint32(v), uint32(p), true
					}
				}
			}
		case strings.HasPrefix(line, "HID_NAME="):
			name = strings.TrimPrefix(line, "HID_NAME=")
		}
	}
	return vid, pid, name, ok
}
