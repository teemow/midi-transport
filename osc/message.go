package osc

import (
	"encoding/binary"
	"fmt"
	"math"
)

// OSC 1.0 wire codec, hand-rolled (the format is trivial and avoids a
// dependency). A packet is either a Message or a Bundle:
//
//	Message: <address string> <typetag string> <args…>
//	Bundle:  "#bundle\0" <timetag:8> ( <int32 size> <element bytes> )*
//
// Strings are null-terminated and padded with nulls to a 4-byte boundary;
// int32/float32 are big-endian 4-byte; blobs are an int32 size + bytes padded
// to 4. Type tags handled: i (int32), f (float32), s (string), b (blob), and
// the no-argument tags T (true), F (false), N (nil), I (impulse).
//
// The X32 speaks this dialect over UDP/10023 (verified live; see
// docs/research/x32.md): a bare address query echoes the wire value, and
// /xremote makes the console mirror every parameter change back to us.

// encodeMessage marshals an OSC address + argument list into one packet.
func encodeMessage(addr string, args []any) ([]byte, error) {
	if addr == "" || addr[0] != '/' {
		return nil, fmt.Errorf("osc: address %q must start with '/'", addr)
	}
	out := encodeString(addr)

	tags := []byte{','}
	var body []byte
	for i, a := range args {
		switch v := a.(type) {
		case int32:
			tags = append(tags, 'i')
			body = appendInt32(body, v)
		case int:
			if v < math.MinInt32 || v > math.MaxInt32 {
				return nil, fmt.Errorf("osc: int arg %d of %d out of int32 range", i, v)
			}
			tags = append(tags, 'i')
			body = appendInt32(body, int32(v))
		case int64:
			if v < math.MinInt32 || v > math.MaxInt32 {
				return nil, fmt.Errorf("osc: int arg %d of %d out of int32 range", i, v)
			}
			tags = append(tags, 'i')
			body = appendInt32(body, int32(v))
		case float32:
			tags = append(tags, 'f')
			body = appendFloat32(body, v)
		case float64:
			tags = append(tags, 'f')
			body = appendFloat32(body, float32(v))
		case string:
			tags = append(tags, 's')
			body = append(body, encodeString(v)...)
		case []byte:
			tags = append(tags, 'b')
			body = appendBlob(body, v)
		case bool:
			if v {
				tags = append(tags, 'T')
			} else {
				tags = append(tags, 'F')
			}
		case nil:
			tags = append(tags, 'N')
		default:
			return nil, fmt.Errorf("osc: unsupported argument %d of type %T", i, a)
		}
	}
	out = append(out, encodeString(string(tags))...)
	out = append(out, body...)
	return out, nil
}

// decodePacket parses one received UDP packet into zero or more messages,
// transparently flattening bundles. Each returned message is (address, args).
func decodePacket(b []byte) ([]decodedMessage, error) {
	if len(b) >= 8 && string(b[:8]) == "#bundle\x00" {
		return decodeBundle(b)
	}
	m, err := decodeMessage(b)
	if err != nil {
		return nil, err
	}
	return []decodedMessage{m}, nil
}

type decodedMessage struct {
	addr string
	args []any
}

func decodeBundle(b []byte) ([]decodedMessage, error) {
	// Skip "#bundle\0" (8) + timetag (8).
	if len(b) < 16 {
		return nil, fmt.Errorf("osc: bundle too short")
	}
	rest := b[16:]
	var out []decodedMessage
	for len(rest) >= 4 {
		size := int(binary.BigEndian.Uint32(rest))
		rest = rest[4:]
		if size < 0 || size > len(rest) {
			return nil, fmt.Errorf("osc: bundle element size %d exceeds %d remaining", size, len(rest))
		}
		elems, err := decodePacket(rest[:size])
		if err != nil {
			return nil, err
		}
		out = append(out, elems...)
		rest = rest[size:]
	}
	return out, nil
}

func decodeMessage(b []byte) (decodedMessage, error) {
	addr, rest, ok := readString(b)
	if !ok {
		return decodedMessage{}, fmt.Errorf("osc: truncated address")
	}
	m := decodedMessage{addr: addr}
	// A type-tag string is conventional but some senders omit it for a bare
	// address (no args). Tolerate that.
	if len(rest) == 0 {
		return m, nil
	}
	tags, rest, ok := readString(rest)
	if !ok || len(tags) == 0 || tags[0] != ',' {
		return decodedMessage{}, fmt.Errorf("osc: missing type-tag string for %q", addr)
	}
	for _, tag := range tags[1:] {
		switch tag {
		case 'i':
			if len(rest) < 4 {
				return decodedMessage{}, fmt.Errorf("osc: truncated int32 arg in %q", addr)
			}
			m.args = append(m.args, int32(binary.BigEndian.Uint32(rest)))
			rest = rest[4:]
		case 'f':
			if len(rest) < 4 {
				return decodedMessage{}, fmt.Errorf("osc: truncated float32 arg in %q", addr)
			}
			m.args = append(m.args, math.Float32frombits(binary.BigEndian.Uint32(rest)))
			rest = rest[4:]
		case 's':
			s, r, ok := readString(rest)
			if !ok {
				return decodedMessage{}, fmt.Errorf("osc: truncated string arg in %q", addr)
			}
			m.args = append(m.args, s)
			rest = r
		case 'b':
			if len(rest) < 4 {
				return decodedMessage{}, fmt.Errorf("osc: truncated blob size in %q", addr)
			}
			n := int(binary.BigEndian.Uint32(rest))
			rest = rest[4:]
			if n < 0 || n > len(rest) {
				return decodedMessage{}, fmt.Errorf("osc: blob size %d exceeds payload in %q", n, addr)
			}
			blob := append([]byte(nil), rest[:n]...)
			m.args = append(m.args, blob)
			// Advance past the padded blob, but never beyond the buffer: a
			// blob that ends the packet without 4-byte padding makes pad4(n) >
			// len(rest), which would panic on the slice.
			adv := pad4(n)
			if adv > len(rest) {
				adv = len(rest)
			}
			rest = rest[adv:]
		case 'T':
			m.args = append(m.args, true)
		case 'F':
			m.args = append(m.args, false)
		case 'N':
			m.args = append(m.args, nil)
		case 'I':
			m.args = append(m.args, float32(math.Inf(1)))
		default:
			// Unknown tag: its data length is unknown, so stop here rather
			// than risk misaligning the rest of the arguments.
			return m, nil
		}
	}
	return m, nil
}

// encodeString returns an OSC string: the bytes, a terminating null, and null
// padding up to the next 4-byte boundary.
func encodeString(s string) []byte {
	n := pad4(len(s) + 1)
	out := make([]byte, n)
	copy(out, s)
	return out
}

// readString reads a null-terminated, 4-byte-padded OSC string from b, returning
// the string, the remaining bytes, and ok=false if no terminator was found.
func readString(b []byte) (string, []byte, bool) {
	for i := 0; i < len(b); i++ {
		if b[i] == 0 {
			s := string(b[:i])
			end := pad4(i + 1)
			if end > len(b) {
				end = len(b)
			}
			return s, b[end:], true
		}
	}
	return "", nil, false
}

func appendInt32(b []byte, v int32) []byte {
	return binary.BigEndian.AppendUint32(b, uint32(v))
}

func appendFloat32(b []byte, v float32) []byte {
	return binary.BigEndian.AppendUint32(b, math.Float32bits(v))
}

func appendBlob(b, blob []byte) []byte {
	b = appendInt32(b, int32(len(blob)))
	b = append(b, blob...)
	for i := len(blob); i < pad4(len(blob)); i++ {
		b = append(b, 0)
	}
	return b
}

// pad4 rounds n up to the next multiple of 4.
func pad4(n int) int { return (n + 3) &^ 3 }
