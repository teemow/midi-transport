package osc

import (
	"reflect"
	"testing"
)

func TestEncodeMessageKnownBytes(t *testing.T) {
	// /ch/05/mix/fader (float 0.5). Address is 16 chars -> 17 with null ->
	// padded to 20; type tag ",f" -> 3 with null -> padded to 4; arg 0.5 =
	// 0x3F000000.
	got, err := encodeMessage("/ch/05/mix/fader", []any{float32(0.5)})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	want := append([]byte("/ch/05/mix/fader"), 0, 0, 0, 0) // 16 + 4 null pad = 20
	want = append(want, ',', 'f', 0, 0)
	want = append(want, 0x3F, 0x00, 0x00, 0x00)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("encode = % X\nwant     % X", got, want)
	}
}

func TestEncodeNoArgs(t *testing.T) {
	got, err := encodeMessage("/xremote", nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// "/xremote" is 8 bytes -> +null padded to 12; ",\0\0\0" = 4.
	want := append([]byte("/xremote"), 0, 0, 0, 0)
	want = append(want, ',', 0, 0, 0)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("encode = % X, want % X", got, want)
	}
}

func TestEncodeRejectsBadAddress(t *testing.T) {
	if _, err := encodeMessage("noslash", nil); err == nil {
		t.Fatalf("expected error for address without leading slash")
	}
}

func TestRoundTrip(t *testing.T) {
	cases := []struct {
		addr string
		args []any
	}{
		{"/ch/01/mix/fader", []any{float32(0.7175)}},
		{"/ch/01/config/color", []any{int32(2)}},
		{"/ch/01/config/name", []any{"vox"}},
		{"/-action/goscene", []any{int32(7)}},
		{"/info", nil},
		{"/mix/on", []any{true, false}},
		{"/blob", []any{[]byte{1, 2, 3}}},
		{"/multi", []any{int32(1), float32(2.5), "three"}},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			pkt, err := encodeMessage(tc.addr, tc.args)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			msgs, err := decodePacket(pkt)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(msgs) != 1 {
				t.Fatalf("got %d messages, want 1", len(msgs))
			}
			if msgs[0].addr != tc.addr {
				t.Fatalf("addr = %q, want %q", msgs[0].addr, tc.addr)
			}
			if !reflect.DeepEqual(msgs[0].args, tc.args) {
				t.Fatalf("args = %#v, want %#v", msgs[0].args, tc.args)
			}
		})
	}
}

func TestDecodeBundle(t *testing.T) {
	m1, _ := encodeMessage("/ch/01/mix/fader", []any{float32(0.5)})
	m2, _ := encodeMessage("/ch/02/mix/fader", []any{float32(0.25)})

	bundle := []byte("#bundle\x00")
	bundle = append(bundle, make([]byte, 8)...) // timetag
	bundle = appendInt32(bundle, int32(len(m1)))
	bundle = append(bundle, m1...)
	bundle = appendInt32(bundle, int32(len(m2)))
	bundle = append(bundle, m2...)

	msgs, err := decodePacket(bundle)
	if err != nil {
		t.Fatalf("decode bundle: %v", err)
	}
	if len(msgs) != 2 || msgs[0].addr != "/ch/01/mix/fader" || msgs[1].addr != "/ch/02/mix/fader" {
		t.Fatalf("bundle msgs = %+v", msgs)
	}
}

func TestPad4(t *testing.T) {
	for in, want := range map[int]int{0: 0, 1: 4, 4: 4, 5: 8, 7: 8, 8: 8} {
		if got := pad4(in); got != want {
			t.Fatalf("pad4(%d) = %d, want %d", in, got, want)
		}
	}
}
