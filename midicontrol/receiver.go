package midicontrol

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// maxMessageBytes bounds a single inbound WebSocket frame. The brain rarely
// sends anything back (it is a command sink); 64 KiB is a generous cap that
// stops a hostile LAN client from forcing a huge allocation per read.
const maxMessageBytes = 1 << 16

// SessionSwitchType is the frame type of the brain→daemon session-switch
// notification: the brain's switcher row was tapped (the brain already emitted
// the Program Change into AUM locally), and the daemon should follow — resolve
// the program via its session-switch registry, update the current session and
// re-import. The only inbound frame type currently decoded; everything else is
// drained and ignored.
const SessionSwitchType = "sessionSwitch"

// sessionSwitchFrame is the brain→daemon wire shape:
// {"type":"sessionSwitch","program":N}.
type sessionSwitchFrame struct {
	Type    string `json:"type"`
	Program int    `json:"program"`
}

// Callbacks are the daemon-side hooks the midi-control receiver dispatches.
// Every field may be nil; none of them may block (run real work in a
// goroutine).
type Callbacks struct {
	// OnConnect / OnDisconnect notify that the brain (the agent's "hands")
	// came or went, so the daemon can tell connected MCP clients.
	OnConnect    func(remote string)
	OnDisconnect func(remote string)
	// OnSessionSwitch receives the program of each inbound sessionSwitch
	// frame (the brain's switcher row was tapped).
	OnSessionSwitch func(program int)
}

// Register mounts the midi-control WebSocket endpoint on mux and dispatches
// the given callbacks.
//
// Route:
//
//	GET /midi-control   ProbeMidiBrain control channel (daemon -> brain commands)
func Register(mux *http.ServeMux, hub *Hub, cb Callbacks) {
	mux.HandleFunc("/midi-control", handleControl(hub, cb))
}

// handleControl upgrades to a WebSocket, registers the connection with the hub
// (so MCP tools can push command frames to it), and then blocks reading until
// the brain disconnects or the daemon shuts down. The brain is mostly a command
// sink: inbound TEXT/BINARY frames are drained (the read is what keeps the
// connection alive and detects loss), with one exception — sessionSwitch frames
// are decoded and dispatched to cb.OnSessionSwitch so a brain-side session
// switch keeps the daemon in sync. Unknown frame types stay ignored (reserved
// for future acks).
func handleControl(hub *Hub, cb Callbacks) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Clear the shared lanhttp absolute deadlines before upgrading, exactly
		// like internal/audiotap: a long-lived control channel must not be
		// dropped after the receiver's 60s Read/WriteTimeout. The loop is bound
		// by r.Context() (cancelled on disconnect / daemon shutdown) instead.
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})

		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// The peer is the native iPad app (no browser Origin), so
			// cross-origin checks do not apply on this LAN-only listener.
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			log.Printf("midi-control receiver: accept: %v", err)
			return
		}
		c.SetReadLimit(maxMessageBytes)

		remote := r.RemoteAddr
		hub.Connect(remote, c)
		log.Printf("midi-control connected from %s", remote)
		if cb.OnConnect != nil {
			cb.OnConnect(remote)
		}
		defer func() {
			hub.Disconnect(c)
			log.Printf("midi-control disconnected from %s", remote)
			if cb.OnDisconnect != nil {
				cb.OnDisconnect(remote)
			}
			_ = c.CloseNow()
		}()

		ctx := r.Context()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				if !errors.Is(err, context.Canceled) &&
					websocket.CloseStatus(err) == -1 {
					log.Printf("midi-control receiver: read from %s: %v", remote, err)
				}
				return
			}
			// sessionSwitch is the one decoded inbound frame; anything else is
			// drained and ignored (reserved for future acks).
			var frame sessionSwitchFrame
			if json.Unmarshal(data, &frame) == nil && frame.Type == SessionSwitchType {
				if cb.OnSessionSwitch != nil {
					cb.OnSessionSwitch(frame.Program)
				}
			}
		}
	}
}
