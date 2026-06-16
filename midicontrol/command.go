package midicontrol

// ControlSurfaceType is the frame type of the daemon→brain control-surface
// manifest. The brain silently drops frame types it does not know, so pushing
// it to an older brain is backward-compatible.
const ControlSurfaceType = "controlSurface"

// ControlSurface is the daemon→brain manifest frame describing the current
// session rig as a renderable control surface: one entry per session-derived
// device, each carrying the controls' widget kinds and the exact MIDI message
// (type/channel/number) the session mapping stores. The brain caches it in the
// AU fullState and renders it in its plugin UI, emitting the messages locally —
// so the surface keeps working when the daemon goes offline.
type ControlSurface struct {
	Type string `json:"type"` // always ControlSurfaceType

	// Session is the staged session id the surface was derived from; Title is
	// the session's human title ("" when untitled).
	Session string `json:"session"`
	Title   string `json:"title,omitempty"`

	Devices []SurfaceDevice `json:"devices"`

	// Sessions is the daemon's session-switch registry: one entry per staged
	// session pinned to a Program Change on the reserved session-switch
	// channel. The brain renders it as a switcher row; a tap emits the PC
	// locally (AUM's hand-mapped global "Session Load" action fires) and sends
	// a sessionSwitch frame upstream so the daemon re-syncs. Present on every
	// push, so the switcher is fullState-cached in every registered session.
	Sessions []SurfaceSession `json:"sessions,omitempty"`
}

// SurfaceSession is one registered cross-session switch: tapping it loads the
// named session via the pinned Program Change.
type SurfaceSession struct {
	Name    string `json:"name"`
	Program int    `json:"program"`
	Channel int    `json:"channel"` // 1-based, like SurfaceMsg.Channel
	// Current marks the daemon's current session (the one this manifest was
	// derived from / last switched to).
	Current bool `json:"current,omitempty"`
}

// SurfaceDevice is one session-derived device (the session device or one
// hosted AUv3 node) and its renderable controls.
type SurfaceDevice struct {
	Name     string           `json:"name"`
	Controls []SurfaceControl `json:"controls"`
}

// SurfaceControl is one renderable control. Widget picks the UI element:
//
//	fader    continuous value (Min/Max bound the wire value, default 0..127)
//	toggle   two named states (Values)
//	trigger  one-shot action (Values holds the single fire value)
//	enum     pick-one from Values
//	preset   preset recall (Msg is the pc to emit; Number is the program)
type SurfaceControl struct {
	Name   string     `json:"name"`
	Widget string     `json:"widget"` // fader | toggle | trigger | enum | preset
	Msg    SurfaceMsg `json:"msg"`

	// Values lists the named wire values for toggle/trigger/enum widgets,
	// ordered by wire value (then label) so rendering is deterministic.
	Values []SurfaceValue `json:"values,omitempty"`

	// Min/Max bound a fader's wire value when the control declares them
	// (omitted = the full 0..127 MIDI data range).
	Min *int `json:"min,omitempty"`
	Max *int `json:"max,omitempty"`
}

// SurfaceMsg is the MIDI message a control emits: the Command wire shape
// reduced to its address (the value byte comes from the widget interaction).
type SurfaceMsg struct {
	Type    string `json:"type"`    // "cc" | "noteOn" | "noteOff" | "pc"
	Channel int    `json:"channel"` // 1-based, like Command.Channel
	Number  int    `json:"number"`  // CC controller / note number / pc program
}

// SurfaceValue is one named wire value of a toggle/trigger/enum control.
type SurfaceValue struct {
	Label string `json:"label"`
	Value int    `json:"value"`
}

// CommandFromMIDI decodes a raw MIDI 1.0 channel/realtime message into the
// brain command frame the LAN channel speaks. ok is false for messages the
// brain protocol does not model (pitch bend, channel pressure, SysEx, clock),
// so callers skip them — the brain cannot re-emit them inside AUM.
//
// It lives with Command (its target type) so both the auv3midi transport (the
// real send path) and any other caller share one decode.
func CommandFromMIDI(data []byte) (Command, bool) {
	if len(data) == 0 {
		return Command{}, false
	}
	status := data[0]
	// System real-time transport (single-byte status).
	switch status {
	case 0xFA:
		return Command{Type: "transport", Action: "start"}, true
	case 0xFB:
		return Command{Type: "transport", Action: "continue"}, true
	case 0xFC:
		return Command{Type: "transport", Action: "stop"}, true
	}
	if status < 0x80 {
		return Command{}, false
	}
	ch := int(status&0x0F) + 1 // wire 0-based nibble -> brain 1-based channel
	d1 := func() int {
		if len(data) > 1 {
			return int(data[1] & 0x7F)
		}
		return 0
	}
	d2 := func() int {
		if len(data) > 2 {
			return int(data[2] & 0x7F)
		}
		return 0
	}
	switch status & 0xF0 {
	case 0x90: // note-on (velocity 0 is a note-off by convention)
		if d2() == 0 {
			return Command{Type: "noteOff", Channel: ch, Note: d1()}, true
		}
		return Command{Type: "noteOn", Channel: ch, Note: d1(), Velocity: d2()}, true
	case 0x80: // note-off
		return Command{Type: "noteOff", Channel: ch, Note: d1(), Velocity: d2()}, true
	case 0xB0: // control change
		return Command{Type: "cc", Channel: ch, Controller: d1(), Value: d2()}, true
	case 0xC0: // program change
		return Command{Type: "pc", Channel: ch, Program: d1()}, true
	default:
		return Command{}, false
	}
}
