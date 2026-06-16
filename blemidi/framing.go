package blemidi

import "time"

// BLE-MIDI packet framing (BLE-MIDI spec, "Packet Encoding").
//
// A characteristic payload is one or more MIDI messages prefixed with a 13-bit
// millisecond timestamp, split into a header byte and per-message timestamp
// bytes:
//
//	byte 0:        1 0 t t t t t t   header  (high 6 timestamp bits)
//	byte 1:        1 t t t t t t t   timestamp (low 7 bits), precedes a message
//	bytes 2..:     MIDI status + data
//	(repeat timestamp byte + message for further messages in the same packet)
//
// Status and timestamp bytes both have the high bit set; the positional rule
// "a timestamp byte precedes every status byte (and every running-status
// message)" disambiguates them on decode. Data bytes always have the high bit
// clear.

// FrameMessage wraps a single MIDI message in a minimal BLE-MIDI packet using
// the current wall-clock millisecond as the timestamp. The pedals ignore the
// timestamp value (it matters only for jitter-correction on ordered streams),
// so a coarse monotonic-ish counter is sufficient.
func FrameMessage(midi []byte) []byte {
	return frameMessageAt(midi, timestamp13())
}

// frameMessageAt is the deterministic core of FrameMessage, taking the 13-bit
// timestamp explicitly so tests can assert exact bytes.
func frameMessageAt(midi []byte, ts uint16) []byte {
	ts &= 0x1FFF
	header := byte(0x80) | byte(ts>>7)
	tsLow := byte(0x80) | byte(ts&0x7F)
	out := make([]byte, 0, len(midi)+2)
	out = append(out, header, tsLow)
	out = append(out, midi...)
	return out
}

// FrameMessageMTU frames a MIDI message into one or more BLE-MIDI packets, each
// at most mtu bytes. Channel-voice / system messages always fit in a single
// packet; a long SysEx is split across continuation packets per the BLE-MIDI
// spec (the first packet opens with 0xF0, intermediate packets carry raw data
// after the header byte, and the final packet ends with a timestamp + 0xF7). An
// mtu < minSysExPacket is treated as "no limit" and yields one packet.
func FrameMessageMTU(midi []byte, mtu int) [][]byte {
	ts := timestamp13()
	full := frameMessageAt(midi, ts)
	// minSysExPacket = header + timestamp + F0 + 1 data byte; below this we
	// cannot make progress, so fall back to a single (best-effort) packet.
	const minSysExPacket = 4
	if mtu < minSysExPacket || len(full) <= mtu {
		return [][]byte{full}
	}
	// Only a SysEx can be meaningfully chunked; anything else over the MTU is
	// emitted whole (it is at most a few bytes, so this should not happen).
	if len(midi) < 2 || midi[0] != 0xF0 || midi[len(midi)-1] != 0xF7 {
		return [][]byte{full}
	}
	return chunkSysEx(midi, ts, mtu)
}

// chunkSysEx splits a complete F0..F7 SysEx into MTU-bounded BLE-MIDI packets.
func chunkSysEx(midi []byte, ts uint16, mtu int) [][]byte {
	ts &= 0x1FFF
	header := byte(0x80) | byte(ts>>7)
	tsLow := byte(0x80) | byte(ts&0x7F)

	body := midi[1 : len(midi)-1] // data bytes between F0 and F7
	var packets [][]byte

	// First packet: header + timestamp + F0 + as much data as fits.
	first := []byte{header, tsLow, 0xF0}
	take := mtu - len(first)
	if take > len(body) {
		take = len(body)
	}
	first = append(first, body[:take]...)
	packets = append(packets, first)
	body = body[take:]

	// Continuation packets: header + data. The packet that can also hold the
	// terminator (timestamp + F7) closes the SysEx.
	for len(body) > 0 {
		pkt := []byte{header}
		room := mtu - 1
		if len(body)+2 <= room { // data + ts + F7 all fit: final packet
			pkt = append(pkt, body...)
			pkt = append(pkt, tsLow, 0xF7)
			return append(packets, pkt)
		}
		take := room
		if take > len(body) {
			take = len(body)
		}
		pkt = append(pkt, body[:take]...)
		packets = append(packets, pkt)
		body = body[take:]
	}
	// All data was sent but the terminator did not fit alongside it: emit it in
	// its own final packet.
	return append(packets, []byte{header, tsLow, 0xF7})
}

// timestamp13 returns the low 13 bits of the current Unix millisecond clock.
func timestamp13() uint16 {
	return uint16(time.Now().UnixMilli()) & 0x1FFF
}

// Decoder decodes a stream of BLE-MIDI packets, carrying state (running status
// and an in-progress SysEx) across packet boundaries. A SysEx blob frequently
// spans several characteristic notifications: the first packet opens it with
// 0xF0 and no terminator, continuation packets carry raw data bytes (after the
// mandatory header byte), and the final packet ends with a timestamp + 0xF7.
// Use one Decoder per Listen session; the stateless DecodePacket is for
// self-contained single packets (and tests).
type Decoder struct {
	running byte   // last channel-voice status (for running status)
	sysex   []byte // accumulating SysEx blob (non-nil while inSysex)
	inSysex bool   // true between an opening 0xF0 and its 0xF7 terminator
}

// Decode strips BLE-MIDI timestamp framing from one received packet and returns
// the complete MIDI messages it yields. A SysEx that does not terminate in this
// packet is buffered and continued on the next Decode call. A truncated
// non-SysEx trailing message is dropped.
func (d *Decoder) Decode(p []byte) [][]byte {
	if len(p) < 2 {
		return nil
	}
	var msgs [][]byte
	i := 1 // skip the header byte

	// A SysEx opened in a previous packet continues right after the header:
	// the continuation bytes are raw SysEx data (no timestamp/status prefix).
	if d.inSysex {
		i = d.consumeSysex(p, i, &msgs)
	}

	for i < len(p) {
		// A timestamp byte (high bit set) precedes every status byte and every
		// running-status message. Consume it when present.
		if p[i]&0x80 != 0 {
			i++
			if i >= len(p) {
				break
			}
		}

		var status byte
		if p[i]&0x80 != 0 {
			status = p[i]
			i++
		} else {
			status = d.running // running status: reuse the previous status
		}
		if status == 0 {
			break // data byte with no established running status
		}

		if status == 0xF0 { // SysEx: collect data until the 0xF7 terminator
			d.inSysex = true
			d.sysex = []byte{0xF0}
			i = d.consumeSysex(p, i, &msgs)
			continue
		}

		n := dataLen(status)
		if i+n > len(p) {
			break // truncated message
		}
		msg := make([]byte, 0, n+1)
		msg = append(msg, status)
		msg = append(msg, p[i:i+n]...)
		msgs = append(msgs, msg)
		i += n

		if status < 0xF0 {
			d.running = status // running status applies only to channel-voice
		} else if status < 0xF8 {
			d.running = 0 // system common cancels running status…
		}
		// …but system real-time (>= 0xF8) may be interleaved anywhere and does
		// NOT cancel running status, so leave running untouched for those.
	}
	return msgs
}

// consumeSysex appends SysEx data bytes from p[i:] onto the in-progress blob,
// terminating it at the 0xF7 (which is preceded by a timestamp byte). It
// returns the index just past the bytes it consumed. If the packet ends before
// the terminator the blob stays open (inSysex remains true) so the next Decode
// continues it. Mirroring the single-packet rule, any other high-bit byte
// inside the blob is treated as a timestamp byte and skipped.
func (d *Decoder) consumeSysex(p []byte, i int, msgs *[][]byte) int {
	for i < len(p) {
		b := p[i]
		if b == 0xF7 {
			d.sysex = append(d.sysex, b)
			i++
			*msgs = append(*msgs, d.sysex)
			d.sysex = nil
			d.inSysex = false
			d.running = 0
			return i
		}
		if b&0x80 != 0 { // timestamp byte preceding the terminator
			i++
			continue
		}
		d.sysex = append(d.sysex, b)
		i++
	}
	return i
}

// DecodePacket decodes a single self-contained BLE-MIDI packet. It is the
// stateless form of Decoder.Decode: an unterminated SysEx in the packet is
// dropped rather than buffered (use a Decoder to reassemble across packets).
func DecodePacket(p []byte) [][]byte {
	d := &Decoder{}
	return d.Decode(p)
}

// dataLen returns the number of data bytes that follow a given MIDI status
// byte (excluding the status byte itself).
func dataLen(status byte) int {
	switch {
	case status >= 0xF8: // system real-time (clock/start/stop/...)
		return 0
	case status >= 0xF0: // system common
		switch status {
		case 0xF1, 0xF3: // MTC quarter-frame, song select
			return 1
		case 0xF2: // song position pointer
			return 2
		default: // 0xF4,0xF5,0xF6 tune request, 0xF7 (unpaired)
			return 0
		}
	case status >= 0xC0 && status <= 0xDF: // program change, channel pressure
		return 1
	default: // 0x80-0xBF (note/poly/cc), 0xE0-0xEF (pitch bend)
		return 2
	}
}

// channelOf extracts the 0-based MIDI channel from a channel-voice message. It
// returns ok=false for system messages (real-time, common, SysEx) which carry
// no channel.
func channelOf(midi []byte) (int, bool) {
	if len(midi) == 0 {
		return 0, false
	}
	s := midi[0]
	if s >= 0x80 && s < 0xF0 {
		return int(s & 0x0F), true
	}
	return 0, false
}
