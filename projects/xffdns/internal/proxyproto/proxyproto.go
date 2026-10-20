// Package proxyproto reads PROXY protocol headers, versions 1 and 2, as
// HAProxy's spec describes them (haproxy.org, "The PROXY protocol"): from
// the start of a TCP stream, or from the front of a UDP datagram (version 2
// only).
package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"strings"
)

// v2sig starts every version 2 header.
var v2sig = []byte("\r\n\r\n\x00\r\nQUIT\n")

const (
	v1Max  = 107  // the longest version 1 line, CRLF included
	v2Head = 16   // signature, version and command, family, length
	v2Max  = 4096 // the most xffdns takes after the 16 bytes (addresses and TLVs)
)

// ErrNoHeader means the data does not start with a PROXY header.
var ErrNoHeader = errors.New("no PROXY protocol header")

// Header is what a header says.
type Header struct {
	Version int
	// Local is set for a version 2 LOCAL command (a health check from the
	// proxy itself) and for version 1 UNKNOWN: there is no client address,
	// and the connection's own address stands.
	Local bool
	// Source is the client; Dest is the address it connected to. Both are
	// unset when Local is.
	Source, Dest netip.AddrPort
	// TLVs are version 2's extra fields, as they came.
	TLVs []TLV
}

// TLV is one version 2 type-length-value field.
type TLV struct {
	Type  byte
	Value []byte
}

// Read reads a header, version 1 or 2, from the start of a stream. On an
// error, what was read is lost: the connection should be closed.
func Read(r *bufio.Reader) (*Header, error) {
	first, err := r.Peek(1)
	if err != nil {
		return nil, err
	}
	switch first[0] {
	case 'P':
		line, err := readLine(r)
		if err != nil {
			return nil, err
		}
		return parseV1(line)
	case '\r':
		head := make([]byte, v2Head)
		if _, err := io.ReadFull(r, head); err != nil {
			return nil, err
		}
		n, err := checkV2(head)
		if err != nil {
			return nil, err
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, err
		}
		return parseV2(head, body)
	}
	return nil, ErrNoHeader
}

// readLine reads a version 1 line, CRLF included, refusing anything longer
// than the spec allows.
func readLine(r *bufio.Reader) (string, error) {
	var b []byte
	for len(b) < v1Max {
		c, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		b = append(b, c)
		if c == '\n' {
			if len(b) < 2 || b[len(b)-2] != '\r' {
				return "", errors.New("PROXY v1: the line does not end in CRLF")
			}
			return string(b[:len(b)-2]), nil
		}
	}
	return "", errors.New("PROXY v1: the line is too long")
}

func parseV1(line string) (*Header, error) {
	f := strings.Split(line, " ")
	if f[0] != "PROXY" || len(f) < 2 {
		return nil, ErrNoHeader
	}
	h := &Header{Version: 1}
	switch f[1] {
	case "UNKNOWN":
		h.Local = true
		return h, nil
	case "TCP4", "TCP6":
	default:
		return nil, fmt.Errorf("PROXY v1: unknown protocol %q", f[1])
	}
	if len(f) != 6 {
		return nil, errors.New("PROXY v1: want 4 fields after the protocol")
	}
	src, err1 := netip.ParseAddr(f[2])
	dst, err2 := netip.ParseAddr(f[3])
	sp, err3 := port(f[4])
	dp, err4 := port(f[5])
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return nil, fmt.Errorf("PROXY v1: %v", err)
	}
	if v4 := f[1] == "TCP4"; src.Is4() != v4 || dst.Is4() != v4 || src.Zone() != "" || dst.Zone() != "" {
		return nil, fmt.Errorf("PROXY v1: the addresses are not %s addresses", f[1])
	}
	h.Source = netip.AddrPortFrom(src, sp)
	h.Dest = netip.AddrPortFrom(dst, dp)
	return h, nil
}

// port reads a decimal port with no sign and no leading zeros.
func port(s string) (uint16, error) {
	if s == "" || len(s) > 1 && s[0] == '0' || s[0] < '0' || s[0] > '9' {
		return 0, fmt.Errorf("bad port %q", s)
	}
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, fmt.Errorf("bad port %q", s)
	}
	return uint16(n), nil
}

// checkV2 checks the fixed 16 bytes and returns the length of the rest.
func checkV2(head []byte) (int, error) {
	if !bytes.Equal(head[:12], v2sig) {
		return 0, ErrNoHeader
	}
	if head[12]>>4 != 2 {
		return 0, fmt.Errorf("PROXY v2: unknown version %d", head[12]>>4)
	}
	if cmd := head[12] & 0xF; cmd > 1 {
		return 0, fmt.Errorf("PROXY v2: unknown command %d", cmd)
	}
	n := int(binary.BigEndian.Uint16(head[14:]))
	if n > v2Max {
		return 0, fmt.Errorf("PROXY v2: %d bytes of addresses and fields is more than %d", n, v2Max)
	}
	return n, nil
}

func parseV2(head, body []byte) (*Header, error) {
	h := &Header{Version: 2}
	local := head[12]&0xF == 0
	var alen int
	switch head[13] >> 4 {
	case 0: // UNSPEC
	case 1:
		alen = 12
	case 2:
		alen = 36
	case 3:
		alen = 216 // UNIX: no IP to take
	default:
		return nil, fmt.Errorf("PROXY v2: unknown address family %d", head[13]>>4)
	}
	if head[13]&0xF > 2 {
		return nil, fmt.Errorf("PROXY v2: unknown transport %d", head[13]&0xF)
	}
	if len(body) < alen {
		return nil, errors.New("PROXY v2: the addresses are cut short")
	}
	switch {
	case local || alen == 0 || alen == 216:
		// LOCAL, UNSPEC and UNIX carry no client IP.
		h.Local = true
	case alen == 12:
		h.Source = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[0:4])), binary.BigEndian.Uint16(body[8:]))
		h.Dest = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[4:8])), binary.BigEndian.Uint16(body[10:]))
	default:
		h.Source = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[0:16])), binary.BigEndian.Uint16(body[32:]))
		h.Dest = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[16:32])), binary.BigEndian.Uint16(body[34:]))
	}
	for rest := body[alen:]; len(rest) > 0; {
		if len(rest) < 3 {
			return nil, errors.New("PROXY v2: a field is cut short")
		}
		n := int(binary.BigEndian.Uint16(rest[1:]))
		if 3+n > len(rest) {
			return nil, errors.New("PROXY v2: a field is longer than the header")
		}
		h.TLVs = append(h.TLVs, TLV{Type: rest[0], Value: rest[3 : 3+n]})
		rest = rest[3+n:]
	}
	return h, nil
}

// ReadDatagram reads the version 2 header at the front of a UDP datagram
// and returns it and the payload after it.
func ReadDatagram(b []byte) (*Header, []byte, error) {
	if len(b) < v2Head {
		if bytes.HasPrefix(v2sig, b[:min(len(b), 12)]) && len(b) > 0 {
			return nil, nil, errors.New("PROXY v2: the header is cut short")
		}
		return nil, nil, ErrNoHeader
	}
	n, err := checkV2(b[:v2Head])
	if err != nil {
		return nil, nil, err
	}
	if len(b) < v2Head+n {
		return nil, nil, errors.New("PROXY v2: the header is cut short")
	}
	h, err := parseV2(b[:v2Head], b[v2Head:v2Head+n])
	if err != nil {
		return nil, nil, err
	}
	return h, b[v2Head+n:], nil
}
