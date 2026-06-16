package midicontrol

import "testing"

// TestCommandFromMIDI checks the raw-MIDI -> brain command-frame decode used by
// the auv3midi transport: channel nibble -> 1-based channel, the
// note-on-velocity-0 -> note-off convention, and the supported/unsupported
// message split.
func TestCommandFromMIDI(t *testing.T) {
	cases := []struct {
		name    string
		data    []byte
		wantOK  bool
		wantTyp string
		wantCh  int
		wantD1  int // controller/note/program
		wantD2  int // value/velocity
	}{
		{"cc ch1", []byte{0xB0, 21, 127}, true, "cc", 1, 21, 127},
		{"cc ch3", []byte{0xB2, 102, 64}, true, "cc", 3, 102, 64},
		{"pc ch1", []byte{0xC0, 5}, true, "pc", 1, 5, 0},
		{"noteOn", []byte{0x90, 60, 100}, true, "noteOn", 1, 60, 100},
		{"noteOn vel0 -> noteOff", []byte{0x90, 60, 0}, true, "noteOff", 1, 60, 0},
		{"noteOff", []byte{0x80, 60, 0}, true, "noteOff", 1, 60, 0},
		{"transport start", []byte{0xFA}, true, "transport", 0, 0, 0},
		{"transport continue", []byte{0xFB}, true, "transport", 0, 0, 0},
		{"transport stop", []byte{0xFC}, true, "transport", 0, 0, 0},
		{"pitch bend unsupported", []byte{0xE0, 0, 64}, false, "", 0, 0, 0},
		{"channel pressure unsupported", []byte{0xD0, 64}, false, "", 0, 0, 0},
		{"sysex unsupported", []byte{0xF0, 0x7F, 0x7F, 0x06, 0x02, 0xF7}, false, "", 0, 0, 0},
		{"empty", nil, false, "", 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, ok := CommandFromMIDI(c.data)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if cmd.Type != c.wantTyp {
				t.Fatalf("type = %q, want %q", cmd.Type, c.wantTyp)
			}
			if cmd.Type == "transport" {
				return
			}
			if cmd.Channel != c.wantCh {
				t.Fatalf("channel = %d, want %d", cmd.Channel, c.wantCh)
			}
			switch cmd.Type {
			case "cc":
				if cmd.Controller != c.wantD1 || cmd.Value != c.wantD2 {
					t.Fatalf("cc = (%d,%d), want (%d,%d)", cmd.Controller, cmd.Value, c.wantD1, c.wantD2)
				}
			case "pc":
				if cmd.Program != c.wantD1 {
					t.Fatalf("pc program = %d, want %d", cmd.Program, c.wantD1)
				}
			case "noteOn", "noteOff":
				if cmd.Note != c.wantD1 || cmd.Velocity != c.wantD2 {
					t.Fatalf("note = (%d,%d), want (%d,%d)", cmd.Note, cmd.Velocity, c.wantD1, c.wantD2)
				}
			}
		})
	}
}
