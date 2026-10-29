package cursoride

import (
	"encoding/binary"
	"fmt"
	"math"
)

// field is one protobuf field. b is set for length-delimited values.
type field struct {
	num int
	wt  int
	v   uint64
	b   []byte
}

type wbuf []byte

func (b *wbuf) varint(v uint64) {
	for v >= 0x80 {
		*b = append(*b, byte(v)|0x80)
		v >>= 7
	}
	*b = append(*b, byte(v))
}

func (b *wbuf) key(field, wt int) { b.varint(uint64(field)<<3 | uint64(wt)) }

func (b *wbuf) str(field int, s string) {
	b.key(field, 2)
	b.varint(uint64(len(s)))
	*b = append(*b, s...)
}

func (b *wbuf) msg(field int, p []byte) {
	b.key(field, 2)
	b.varint(uint64(len(p)))
	*b = append(*b, p...)
}

func (b *wbuf) bool(field int, v bool) {
	b.key(field, 0)
	if v {
		*b = append(*b, 1)
		return
	}
	*b = append(*b, 0)
}

func (b *wbuf) u32(field int, v uint32) {
	b.key(field, 0)
	b.varint(uint64(v))
}

func (b *wbuf) fixed64(field int, v uint64) {
	b.key(field, 1)
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], v)
	*b = append(*b, raw[:]...)
}

func takeVarint(b []byte) (uint64, int) {
	var x uint64
	for i := 0; i < len(b) && i < 10; i++ {
		x |= uint64(b[i]&0x7f) << (7 * uint(i))
		if b[i] < 0x80 {
			return x, i + 1
		}
	}
	return 0, 0
}

// parseFields reads a protobuf message. A short buffer is an error: a
// failed read is unknown, not an empty message. Unknown wire types fail
// the same way. Fixed-width fields are skipped so a later Cursor field
// does not make the rows we do know unreadable.
func parseFields(b []byte) ([]field, error) {
	var out []field
	for len(b) > 0 {
		key, n := takeVarint(b)
		if n == 0 {
			return nil, fmt.Errorf("cursor ide: truncated protobuf")
		}
		b = b[n:]
		num := int(key >> 3)
		wt := int(key & 7)
		if num <= 0 {
			return nil, fmt.Errorf("cursor ide: protobuf field %d", num)
		}
		f := field{num: num, wt: wt}
		switch wt {
		case 0:
			v, n := takeVarint(b)
			if n == 0 {
				return nil, fmt.Errorf("cursor ide: truncated protobuf")
			}
			f.v = v
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, fmt.Errorf("cursor ide: truncated protobuf")
			}
			f.b = b[:8]
			b = b[8:]
		case 2:
			ln, n := takeVarint(b)
			if n == 0 {
				return nil, fmt.Errorf("cursor ide: truncated protobuf")
			}
			b = b[n:]
			if uint64(len(b)) < ln {
				return nil, fmt.Errorf("cursor ide: truncated protobuf")
			}
			f.b = b[:ln]
			b = b[ln:]
		case 5:
			if len(b) < 4 {
				return nil, fmt.Errorf("cursor ide: truncated protobuf")
			}
			b = b[4:]
		default:
			return nil, fmt.Errorf("cursor ide: protobuf wire type %d", wt)
		}
		out = append(out, f)
	}
	return out, nil
}

func fieldString(fs []field, num int) (string, bool) {
	for _, f := range fs {
		if f.num == num && f.wt == 2 {
			return string(f.b), true
		}
	}
	return "", false
}

func fieldUint(fs []field, num int) (uint64, bool) {
	for _, f := range fs {
		if f.num == num && f.wt == 0 {
			return f.v, true
		}
	}
	return 0, false
}

func fieldBytes(fs []field, num int) [][]byte {
	var out [][]byte
	for _, f := range fs {
		if f.num == num && f.wt == 2 {
			out = append(out, f.b)
		}
	}
	return out
}

func hasField(fs []field, num int) bool {
	for _, f := range fs {
		if f.num == num {
			return true
		}
	}
	return false
}

// UnwrapUnary takes the protobuf out of a Connect or gRPC unary frame.
// A body that is not a frame is returned as itself. A compressed frame
// is an error: merging into it would corrupt Cursor's catalog.
func UnwrapUnary(body []byte) (payload []byte, framed bool, err error) {
	if len(body) < 5 {
		return body, false, nil
	}
	flags := body[0]
	n := binary.BigEndian.Uint32(body[1:5])
	if int(n) != len(body)-5 {
		return body, false, nil
	}
	if flags != 0 {
		return nil, true, fmt.Errorf("cursor ide: compressed catalog frame")
	}
	return body[5:], true, nil
}

// MergeUnary appends extra, a protobuf message of the same type, to
// upstream. Repeated fields concatenate, which is how Cursor's catalog
// gains magpie's models without replacing Cursor's own. A framed upstream
// stays framed and its length is rewritten. extra is the raw message,
// not a frame.
func MergeUnary(upstream, extra []byte) ([]byte, error) {
	if len(extra) == 0 {
		return upstream, nil
	}
	payload, framed, err := UnwrapUnary(upstream)
	if err != nil {
		return nil, err
	}
	if uint64(len(payload))+uint64(len(extra)) > math.MaxUint32 {
		return nil, fmt.Errorf("cursor ide: catalog frame too large")
	}
	merged := make([]byte, 0, len(payload)+len(extra))
	merged = append(merged, payload...)
	merged = append(merged, extra...)
	if !framed {
		return merged, nil
	}
	out := make([]byte, 5+len(merged))
	out[0] = 0
	binary.BigEndian.PutUint32(out[1:5], uint32(len(merged)))
	copy(out[5:], merged)
	return out, nil
}
