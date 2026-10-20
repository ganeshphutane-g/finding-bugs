// Package dnsmsg reads the few parts of a DNS message xffdns needs (the
// header, the question and the EDNS buffer size) and builds the answers it
// gives itself: errors and truncated replies. It is not a DNS library.
package dnsmsg

import (
	"encoding/binary"
	"errors"
	"strings"
)

// Header flags and codes (RFC 1035 4.1.1).
const (
	flagQR = 1 << 15
	flagTC = 1 << 9
	flagRD = 1 << 8
	flagRA = 1 << 7

	RcodeFormErr  = 1
	RcodeServFail = 2
	RcodeRefused  = 5

	typeOPT = 41
)

// HeaderLen is the size of a DNS header.
const HeaderLen = 12

// Message sizes for UDP answers.
const (
	MinUDPSize = 512  // without EDNS (RFC 1035)
	MaxUDPSize = 4096 // the most xffdns sends, whatever the client asks for
	ednsSize   = 1232 // what xffdns advertises in the answers it makes
)

var (
	ErrShort    = errors.New("the message is cut short")
	ErrQuestion = errors.New("the message has more than one question")
	ErrName     = errors.New("a name is malformed")
)

// Info is what xffdns knows about a message.
type Info struct {
	ID    uint16
	Flags uint16
	// Question is the question section as it is in the message (a name in
	// it cannot be compressed: nothing comes before it to point to).
	Question []byte
	// Name is the question's name, lowercase, ending in ".". QType and
	// QClass go with it. All three are empty without a question.
	Name   string
	QType  uint16
	QClass uint16
	// OPT is set when the message has an EDNS OPT record; UDPSize is the
	// buffer size it advertises.
	OPT     bool
	UDPSize int
}

// Parse reads a message: its header, its question (at most one) and, past
// any answers, its OPT record.
func Parse(msg []byte) (*Info, error) {
	if len(msg) < HeaderLen {
		return nil, ErrShort
	}
	in := &Info{ID: binary.BigEndian.Uint16(msg), Flags: binary.BigEndian.Uint16(msg[2:])}
	qd := binary.BigEndian.Uint16(msg[4:])
	an := binary.BigEndian.Uint16(msg[6:])
	ns := binary.BigEndian.Uint16(msg[8:])
	ar := binary.BigEndian.Uint16(msg[10:])
	if qd > 1 {
		return nil, ErrQuestion
	}
	off := HeaderLen
	if qd == 1 {
		name, end, err := readName(msg, off, false)
		if err != nil {
			return nil, err
		}
		if end+4 > len(msg) {
			return nil, ErrShort
		}
		in.Name = name
		in.QType = binary.BigEndian.Uint16(msg[end:])
		in.QClass = binary.BigEndian.Uint16(msg[end+2:])
		in.Question = msg[off : end+4]
		off = end + 4
	}
	for i := 0; i < int(an)+int(ns)+int(ar); i++ {
		_, end, err := readName(msg, off, true)
		if err != nil {
			return nil, err
		}
		if end+10 > len(msg) {
			return nil, ErrShort
		}
		typ := binary.BigEndian.Uint16(msg[end:])
		class := binary.BigEndian.Uint16(msg[end+2:])
		rdlen := int(binary.BigEndian.Uint16(msg[end+8:]))
		if end+10+rdlen > len(msg) {
			return nil, ErrShort
		}
		if typ == typeOPT && i >= int(an)+int(ns) {
			in.OPT = true
			in.UDPSize = int(class)
		}
		off = end + 10 + rdlen
	}
	return in, nil
}

// readName reads the name at off and returns it and the offset just past
// it. Compression pointers are followed only when allowed.
func readName(msg []byte, off int, pointers bool) (string, int, error) {
	var b strings.Builder
	end := -1
	for hops := 0; ; {
		if off >= len(msg) {
			return "", 0, ErrShort
		}
		l := int(msg[off])
		switch {
		case l == 0:
			if end < 0 {
				end = off + 1
			}
			if b.Len() == 0 {
				b.WriteByte('.')
			}
			return b.String(), end, nil
		case l&0xC0 == 0xC0:
			if !pointers || off+1 >= len(msg) {
				return "", 0, ErrName
			}
			if hops++; hops > 64 {
				return "", 0, ErrName
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(msg[off:]) & 0x3FFF)
		case l > 63:
			return "", 0, ErrName
		default:
			if off+1+l > len(msg) {
				return "", 0, ErrShort
			}
			b.WriteString(strings.ToLower(string(msg[off+1 : off+1+l])))
			b.WriteByte('.')
			if b.Len() > 254 {
				return "", 0, ErrName
			}
			off += 1 + l
		}
	}
}

// IsQuery reports whether the message is a query (QR not set).
func (in *Info) IsQuery() bool { return in.Flags&flagQR == 0 }

// MaxUDP is the largest UDP answer the sender of this query can take.
func (in *Info) MaxUDP() int {
	if !in.OPT || in.UDPSize < MinUDPSize {
		return MinUDPSize
	}
	return min(in.UDPSize, MaxUDPSize)
}

// SetID writes id into a message's header.
func SetID(msg []byte, id uint16) {
	if len(msg) >= 2 {
		binary.BigEndian.PutUint16(msg, id)
	}
}

// ID reads a message's ID (0 when there is no header).
func ID(msg []byte) uint16 {
	if len(msg) < 2 {
		return 0
	}
	return binary.BigEndian.Uint16(msg)
}

// Reply builds an answer to q with no records: the error rcode, or a
// truncated answer when tc is set. It keeps the query's ID, opcode, RD and
// question, and has an OPT record when the query had one.
func Reply(q *Info, rcode int, tc bool) []byte {
	flags := flagQR | flagRA | q.Flags&(0x7800|flagRD) | uint16(rcode&0xF)
	if tc {
		flags |= flagTC
	}
	out := make([]byte, HeaderLen, HeaderLen+len(q.Question)+11)
	binary.BigEndian.PutUint16(out, q.ID)
	binary.BigEndian.PutUint16(out[2:], flags)
	if len(q.Question) > 0 {
		binary.BigEndian.PutUint16(out[4:], 1)
		out = append(out, q.Question...)
	}
	if q.OPT {
		binary.BigEndian.PutUint16(out[10:], 1)
		// Root name, type OPT, class = buffer size, TTL 0, no options.
		out = append(out, 0, 0, typeOPT, byte(ednsSize>>8), byte(ednsSize&0xFF), 0, 0, 0, 0, 0, 0)
	}
	return out
}

// FitUDP returns resp unchanged when it fits the query's UDP size, and a
// truncated answer (TC set, no records) otherwise.
func FitUDP(q *Info, resp []byte) []byte {
	if len(resp) <= q.MaxUDP() {
		return resp
	}
	r := Reply(q, 0, true)
	// Keep the upstream's rcode.
	if len(resp) >= 4 {
		r[3] = r[3]&0xF0 | resp[3]&0x0F
	}
	return r
}
