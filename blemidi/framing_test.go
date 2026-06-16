package blemidi

import (
	"bytes"
	"reflect"
	"testing"
)

func TestFrameMessageAt(t *testing.T) {
	tests := []struct {
		name string
		midi []byte
		ts   uint16
		want []byte
	}{
		{
			name: "cc with zero timestamp",
			midi: []byte{0xB0, 0x11, 0x40},
			ts:   0,
			want: []byte{0x80, 0x80, 0xB0, 0x11, 0x40},
		},
		{
			name: "program change splits 13-bit timestamp",
			midi: []byte{0xC4, 0x05},
			ts:   0x1FFF, // all 13 bits set
			want: []byte{0x80 | 0x3F, 0x80 | 0x7F, 0xC4, 0x05},
		},
		{
			name: "timestamp masked to 13 bits",
			midi: []byte{0xF8},
			ts:   0xFFFF, // upper bits must be discarded
			want: []byte{0x80 | 0x3F, 0x80 | 0x7F, 0xF8},
		},
		{
			name: "low and high halves placed correctly",
			midi: []byte{0x90, 0x40, 0x7F},
			ts:   0x0081, // high=1, low=1
			want: []byte{0x81, 0x81, 0x90, 0x40, 0x7F},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := frameMessageAt(tc.midi, tc.ts)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("frameMessageAt(% X, %#x) = % X, want % X", tc.midi, tc.ts, got, tc.want)
			}
			if got[0]&0x80 == 0 || got[1]&0x80 == 0 {
				t.Fatalf("header/timestamp bytes must have the high bit set: % X", got[:2])
			}
		})
	}
}

func TestFrameMessageUsesLiveTimestamp(t *testing.T) {
	got := FrameMessage([]byte{0xB0, 0x07, 0x64})
	if len(got) != 5 {
		t.Fatalf("len = %d, want 5", len(got))
	}
	if got[0]&0xC0 != 0x80 {
		t.Fatalf("header byte %#x: top two bits must be 10", got[0])
	}
	if got[1]&0x80 == 0 {
		t.Fatalf("timestamp byte %#x must have the high bit set", got[1])
	}
	if !bytes.Equal(got[2:], []byte{0xB0, 0x07, 0x64}) {
		t.Fatalf("payload = % X, want B0 07 64", got[2:])
	}
}

func TestDecodePacket(t *testing.T) {
	tests := []struct {
		name string
		pkt  []byte
		want [][]byte
	}{
		{
			name: "too short",
			pkt:  []byte{0x80},
			want: nil,
		},
		{
			name: "single control change",
			pkt:  []byte{0x80, 0x81, 0xB0, 0x11, 0x40},
			want: [][]byte{{0xB0, 0x11, 0x40}},
		},
		{
			name: "program change (one data byte)",
			pkt:  []byte{0x80, 0x81, 0xC4, 0x05},
			want: [][]byte{{0xC4, 0x05}},
		},
		{
			name: "two messages each with its own timestamp",
			pkt:  []byte{0x80, 0x81, 0xB0, 0x11, 0x40, 0x82, 0xC4, 0x05},
			want: [][]byte{{0xB0, 0x11, 0x40}, {0xC4, 0x05}},
		},
		{
			name: "running status: second message omits status byte",
			pkt:  []byte{0x80, 0x81, 0xB0, 0x11, 0x40, 0x82, 0x11, 0x60},
			want: [][]byte{{0xB0, 0x11, 0x40}, {0xB0, 0x11, 0x60}},
		},
		{
			name: "system real-time clock (no data bytes)",
			pkt:  []byte{0x80, 0x81, 0xF8},
			want: [][]byte{{0xF8}},
		},
		{
			name: "real-time interleaved between channel messages",
			pkt:  []byte{0x80, 0x81, 0xF8, 0x82, 0xB0, 0x11, 0x40},
			want: [][]byte{{0xF8}, {0xB0, 0x11, 0x40}},
		},
		{
			name: "sysex blob",
			pkt:  []byte{0x80, 0x81, 0xF0, 0x7D, 0x01, 0x02, 0x82, 0xF7},
			want: [][]byte{{0xF0, 0x7D, 0x01, 0x02, 0xF7}},
		},
		{
			name: "truncated trailing message is dropped",
			pkt:  []byte{0x80, 0x81, 0xB0, 0x11},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodePacket(tc.pkt)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DecodePacket(% X) = %v, want %v", tc.pkt, got, tc.want)
			}
		})
	}
}

func TestFrameDecodeRoundTrip(t *testing.T) {
	messages := [][]byte{
		{0xB0, 0x11, 0x40}, // CC
		{0xC4, 0x05},       // program change
		{0x90, 0x40, 0x7F}, // note on
		{0x80, 0x40, 0x00}, // note off
		{0xF8},             // clock
		{0xE0, 0x00, 0x40}, // pitch bend
	}
	for _, m := range messages {
		pkt := FrameMessage(m)
		got := DecodePacket(pkt)
		if len(got) != 1 || !bytes.Equal(got[0], m) {
			t.Fatalf("round-trip of % X: framed % X decoded to %v", m, pkt, got)
		}
	}
}

func TestDecoderMultiPacketSysEx(t *testing.T) {
	// A SysEx split across three packets: open (F0 + data, no terminator),
	// continuation (raw data), final (data + timestamp + F7).
	hdr := byte(0x80)
	ts := byte(0x80)
	p1 := []byte{hdr, ts, 0xF0, 0x7D, 0x01}
	p2 := []byte{hdr, 0x02, 0x03}
	p3 := []byte{hdr, 0x04, ts, 0xF7}

	dec := &Decoder{}
	var got [][]byte
	got = append(got, dec.Decode(p1)...)
	got = append(got, dec.Decode(p2)...)
	got = append(got, dec.Decode(p3)...)

	want := [][]byte{{0xF0, 0x7D, 0x01, 0x02, 0x03, 0x04, 0xF7}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multi-packet sysex = %v, want %v", got, want)
	}
}

func TestFrameMessageMTUChunksSysEx(t *testing.T) {
	// A SysEx longer than the MTU must split into several packets that, when
	// decoded by a single stateful Decoder, reassemble to the original message.
	body := make([]byte, 0, 40)
	for i := 0; i < 40; i++ {
		body = append(body, byte(i))
	}
	midi := append(append([]byte{0xF0}, body...), 0xF7)

	const mtu = 12
	packets := FrameMessageMTU(midi, mtu)
	if len(packets) < 2 {
		t.Fatalf("expected the long sysex to be chunked, got %d packet(s)", len(packets))
	}
	for i, p := range packets {
		if len(p) > mtu {
			t.Fatalf("packet %d len %d exceeds mtu %d", i, len(p), mtu)
		}
	}

	dec := &Decoder{}
	var got [][]byte
	for _, p := range packets {
		got = append(got, dec.Decode(p)...)
	}
	want := [][]byte{midi}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reassembled = %v, want %v", got, want)
	}
}

func TestChannelOf(t *testing.T) {
	tests := []struct {
		midi   []byte
		wantCh int
		wantOK bool
	}{
		{[]byte{0xB0, 0x11, 0x40}, 0, true},
		{[]byte{0xB5, 0x11, 0x40}, 5, true},
		{[]byte{0xCF, 0x05}, 15, true},
		{[]byte{0xF8}, 0, false},
		{[]byte{0xF0, 0x7D, 0xF7}, 0, false},
		{nil, 0, false},
	}
	for _, tc := range tests {
		gotCh, gotOK := channelOf(tc.midi)
		if gotCh != tc.wantCh || gotOK != tc.wantOK {
			t.Fatalf("channelOf(% X) = (%d,%v), want (%d,%v)", tc.midi, gotCh, gotOK, tc.wantCh, tc.wantOK)
		}
	}
}

func TestDataLen(t *testing.T) {
	tests := []struct {
		status byte
		want   int
	}{
		{0x80, 2}, {0x90, 2}, {0xA0, 2}, {0xB0, 2}, {0xE0, 2},
		{0xC0, 1}, {0xD0, 1},
		{0xF1, 1}, {0xF3, 1}, {0xF2, 2}, {0xF6, 0}, {0xF8, 0}, {0xFE, 0},
	}
	for _, tc := range tests {
		if got := dataLen(tc.status); got != tc.want {
			t.Fatalf("dataLen(%#x) = %d, want %d", tc.status, got, tc.want)
		}
	}
}
