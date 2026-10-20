package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/netip"
	"os"
	"strings"
	"testing"
)

// fixture reads a capture: what a real proxy sent, as hex (see
// testdata/README).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	out, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCaptured(t *testing.T) {
	for _, c := range []struct {
		file    string
		version int
		src     string
	}{
		{"haproxy-v1.hex", 1, "127.0.0.1:53988"},
		{"haproxy-v2.hex", 2, "127.0.0.1:39200"},
		{"traefik-v2.hex", 2, "127.0.0.1:53446"},
	} {
		r := bufio.NewReader(bytes.NewReader(fixture(t, c.file)))
		h, err := Read(r)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if h.Version != c.version || h.Local || h.Source.String() != c.src || len(h.TLVs) != 0 {
			t.Fatalf("%s: %+v", c.file, h)
		}
		// The client's data comes after the header, untouched.
		rest, _ := io.ReadAll(r)
		if string(rest) != "HELLO" {
			t.Fatalf("%s: then %q", c.file, rest)
		}
	}
}

func TestCapturedDatagram(t *testing.T) {
	h, payload, err := ReadDatagram(fixture(t, "dnsdist-v2-udp.hex"))
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != 2 || h.Local || h.Source.Addr() != netip.MustParseAddr("127.0.0.1") || h.Dest.Port() != 9864 {
		t.Fatalf("%+v", h)
	}
	// The DNS query for example.com/A follows.
	if len(payload) != 29 || !bytes.Contains(payload, []byte("\x07example\x03com\x00")) {
		t.Fatalf("%x", payload)
	}
}

func v2(cmd, fam byte, body []byte) []byte {
	b := append([]byte{}, v2sig...)
	b = append(b, 0x20|cmd, fam, 0, 0)
	binary.BigEndian.PutUint16(b[14:], uint16(len(body)))
	return append(b, body...)
}

func TestV2(t *testing.T) {
	addrs6 := make([]byte, 36)
	copy(addrs6, netip.MustParseAddr("2001:db8::5").AsSlice())
	copy(addrs6[16:], netip.MustParseAddr("2001:db8::1").AsSlice())
	binary.BigEndian.PutUint16(addrs6[32:], 4444)
	binary.BigEndian.PutUint16(addrs6[34:], 853)
	tlv := []byte{0x02, 0, 3, 'a', 'b', 'c', 0x04, 0, 0} // AUTHORITY "abc", NOOP
	h, err := Read(bufio.NewReader(bytes.NewReader(v2(1, 0x21, append(addrs6, tlv...)))))
	if err != nil {
		t.Fatal(err)
	}
	if h.Source.String() != "[2001:db8::5]:4444" || h.Dest.String() != "[2001:db8::1]:853" || len(h.TLVs) != 2 ||
		h.TLVs[0].Type != 2 || string(h.TLVs[0].Value) != "abc" {
		t.Fatalf("%+v", h)
	}
	// LOCAL (a health check), UNSPEC and UNIX: no client address.
	for _, b := range [][]byte{v2(0, 0x11, make([]byte, 12)), v2(1, 0x00, nil), v2(1, 0x31, make([]byte, 216))} {
		h, err := Read(bufio.NewReader(bytes.NewReader(b)))
		if err != nil || !h.Local {
			t.Fatalf("%x: %+v %v", b[12:14], h, err)
		}
	}
}

func TestV1(t *testing.T) {
	for line, want := range map[string]string{
		"PROXY TCP4 192.0.2.10 192.0.2.1 5000 853\r\n":          "192.0.2.10:5000",
		"PROXY TCP6 2001:db8::5 2001:db8::1 5000 853\r\n":       "[2001:db8::5]:5000",
		"PROXY UNKNOWN\r\n":                                     "local",
		"PROXY UNKNOWN ffff:f...f:ffff ffff:f...f:ffff 1 2\r\n": "local",
	} {
		h, err := Read(bufio.NewReader(strings.NewReader(line)))
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		got := h.Source.String()
		if h.Local {
			got = "local"
		}
		if got != want {
			t.Fatalf("%q: %s", line, got)
		}
	}
}

func TestBad(t *testing.T) {
	for _, s := range []string{
		"",
		"GET / HTTP/1.1\r\n",
		"PROXY TCP4 192.0.2.10 192.0.2.1 5000 853\n",
		"PROXY TCP4 192.0.2.10 192.0.2.1 5000\r\n",
		"PROXY TCP4 2001:db8::5 192.0.2.1 5000 853\r\n",
		"PROXY TCP6 192.0.2.10 192.0.2.1 5000 853\r\n",
		"PROXY TCP4 192.0.2.10 192.0.2.1 05000 853\r\n",
		"PROXY TCP4 192.0.2.10 192.0.2.1 70000 853\r\n",
		"PROXY TCP4 192.0.2.10 192.0.2.1 -1 853\r\n",
		"PROXY UDP4 192.0.2.10 192.0.2.1 5000 853\r\n",
		"PROXY TCP6 fe80::1%eth0 2001:db8::1 5000 853\r\n",
		"PROXY TCP4 192.0.2.10 192.0.2.1 5000 853" + strings.Repeat(" ", 100) + "\r\n",
		"PROXY TCP4 192.0.2.10",
		string(v2(1, 0x11, make([]byte, 8))),                // addresses cut short
		string(v2(2, 0x11, make([]byte, 12))),               // unknown command
		string(v2(1, 0x41, make([]byte, 12))),               // unknown family
		string(v2(1, 0x13, make([]byte, 12))),               // unknown transport
		string(v2(1, 0x11, append(make([]byte, 12), 1, 0))), // a field cut short
		string(v2(1, 0x11, append(make([]byte, 12), 1, 0, 9, 1))),
		string(v2(1, 0x11, make([]byte, 5000))),                               // too long
		string(append(v2(1, 0x11, make([]byte, 12))[:12], 0x11, 0x11, 0, 12)), // version 1 in a v2 header
		string(v2(1, 0x11, make([]byte, 12)))[:20],                            // the stream ends
	} {
		if h, err := Read(bufio.NewReader(strings.NewReader(s))); err == nil {
			t.Errorf("%q: read %+v", s, h)
		}
	}
}

func TestDatagramBad(t *testing.T) {
	q := fixture(t, "dnsdist-v2-udp.hex")
	for name, b := range map[string][]byte{
		"plain DNS":   q[28:],
		"cut short":   q[:20],
		"short sig":   q[:6],
		"nothing":     nil,
		"bad command": v2(5, 0x12, make([]byte, 12)),
	} {
		if _, _, err := ReadDatagram(b); err == nil {
			t.Errorf("%s: read", name)
		}
	}
	if _, _, err := ReadDatagram(q[28:]); err != ErrNoHeader {
		t.Fatalf("plain DNS: %v", err)
	}
}
