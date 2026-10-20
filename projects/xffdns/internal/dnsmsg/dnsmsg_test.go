package dnsmsg

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// query builds a query for name/A, with an OPT record advertising size when
// size > 0.
func query(id uint16, name string, size int) []byte {
	m := make([]byte, HeaderLen)
	binary.BigEndian.PutUint16(m, id)
	binary.BigEndian.PutUint16(m[2:], flagRD)
	binary.BigEndian.PutUint16(m[4:], 1)
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		m = append(m, byte(len(l)))
		m = append(m, l...)
	}
	m = append(m, 0, 0, 1, 0, 1)
	if size > 0 {
		binary.BigEndian.PutUint16(m[10:], 1)
		m = append(m, 0, 0, typeOPT, byte(size>>8), byte(size), 0, 0, 0x80, 0, 0, 0)
	}
	return m
}

func TestParse(t *testing.T) {
	in, err := Parse(query(0x1234, "WWW.Example.com", 0))
	if err != nil {
		t.Fatal(err)
	}
	if in.ID != 0x1234 || in.Name != "www.example.com." || in.QType != 1 || in.QClass != 1 || in.OPT || !in.IsQuery() {
		t.Fatalf("%+v", in)
	}
	if in.MaxUDP() != 512 {
		t.Fatal(in.MaxUDP())
	}
	for size, want := range map[int]int{1232: 1232, 100: 512, 65000: 4096} {
		in, err := Parse(query(1, "example.com", size))
		if err != nil || !in.OPT || in.MaxUDP() != want {
			t.Fatalf("size %d: %+v %v", size, in, err)
		}
	}
}

func TestParseAnswers(t *testing.T) {
	// An answer with a compressed name, then an OPT record.
	m := query(7, "example.com", 0)
	binary.BigEndian.PutUint16(m[2:], flagQR|flagRD|flagRA)
	binary.BigEndian.PutUint16(m[6:], 1)
	binary.BigEndian.PutUint16(m[10:], 1)
	m = append(m, 0xC0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 1)
	m = append(m, 0, 0, typeOPT, 0x10, 0, 0, 0, 0, 0, 0, 0)
	in, err := Parse(m)
	if err != nil {
		t.Fatal(err)
	}
	if in.IsQuery() || !in.OPT || in.UDPSize != 4096 {
		t.Fatalf("%+v", in)
	}
}

func TestParseBad(t *testing.T) {
	good := query(1, "example.com", 0)
	two := append([]byte{}, good...)
	binary.BigEndian.PutUint16(two[4:], 2)
	ptr := append([]byte{}, good[:HeaderLen]...)
	ptr = append(ptr, 0xC0, 12, 0, 1, 0, 1) // a pointer in the question
	long := append([]byte{}, good[:HeaderLen]...)
	long = append(long, 64)
	long = append(long, bytes.Repeat([]byte{'a'}, 64)...)
	long = append(long, 0, 0, 1, 0, 1)
	var huge []byte
	huge = append(huge, good[:HeaderLen]...)
	for range 5 {
		huge = append(huge, 63)
		huge = append(huge, bytes.Repeat([]byte{'a'}, 63)...)
	}
	huge = append(huge, 0, 0, 1, 0, 1)
	loop := append([]byte{}, good...)
	binary.BigEndian.PutUint16(loop[6:], 1)
	n := len(loop)
	loop = append(loop, 0xC0|byte(n>>8), byte(n)) // points at itself
	extra := append([]byte{}, good...)
	binary.BigEndian.PutUint16(extra[10:], 1) // an OPT that is not there
	for name, m := range map[string][]byte{
		"short header": good[:5], "short question": good[:len(good)-2], "two questions": two,
		"pointer in question": ptr, "label over 63": long, "name over 255": huge, "pointer loop": loop,
		"missing record": extra,
	} {
		if _, err := Parse(m); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	// No question at all is fine.
	m := make([]byte, HeaderLen)
	if in, err := Parse(m); err != nil || in.Name != "" {
		t.Fatal(in, err)
	}
}

func TestReply(t *testing.T) {
	q, _ := Parse(query(0xBEEF, "example.com", 1232))
	r := Reply(q, RcodeServFail, false)
	in, err := Parse(r)
	if err != nil {
		t.Fatal(err)
	}
	if in.ID != 0xBEEF || in.IsQuery() || in.Flags&0xF != RcodeServFail || in.Flags&flagRD == 0 || in.Flags&flagTC != 0 ||
		in.Name != "example.com." || !in.OPT || in.UDPSize != ednsSize {
		t.Fatalf("%+v", in)
	}
	q, _ = Parse(query(1, "example.com", 0))
	if in, _ := Parse(Reply(q, RcodeRefused, false)); in.OPT || in.Flags&0xF != RcodeRefused {
		t.Fatalf("%+v", in)
	}
}

func TestFitUDP(t *testing.T) {
	q, _ := Parse(query(9, "example.com", 0))
	small := Reply(q, 0, false)
	if got := FitUDP(q, small); !bytes.Equal(got, small) {
		t.Fatal("a small answer was changed")
	}
	big := append(append([]byte{}, small...), make([]byte, 600)...)
	big[3] |= 3 // NXDOMAIN
	got := FitUDP(q, big)
	in, err := Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 512 || in.Flags&flagTC == 0 || in.Flags&0xF != 3 || in.ID != 9 || in.Name != "example.com." {
		t.Fatalf("%d %+v", len(got), in)
	}
	q, _ = Parse(query(9, "example.com", 1232))
	if got := FitUDP(q, big); !bytes.Equal(got, big) {
		t.Fatal("an answer within the EDNS size was truncated")
	}
}

func TestSetID(t *testing.T) {
	m := query(5, "a.b", 0)
	SetID(m, 0)
	if ID(m) != 0 {
		t.Fatal(ID(m))
	}
	SetID(m[:1], 3) // too short: no panic
	if ID(nil) != 0 {
		t.Fatal("ID of nothing")
	}
}

func TestCheckQuery(t *testing.T) {
	if err := CheckQuery(query(1, "example.com", 1232)); err != nil {
		t.Fatal(err)
	}
	resp := query(1, "example.com", 0)
	resp[2] |= 0x80
	withAnswer := query(1, "example.com", 0)
	binary.BigEndian.PutUint16(withAnswer[6:], 1)
	many := query(1, "example.com", 0)
	binary.BigEndian.PutUint16(many[10:], 500)
	for name, m := range map[string][]byte{"response": resp, "answer": withAnswer, "500 additional": many, "short": {1, 2}} {
		if CheckQuery(m) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A message built to be slow: thousands of records whose names are long
// pointer chains. Parsing must stay cheap (it once took ~50ms).
func TestParseCost(t *testing.T) {
	m := make([]byte, HeaderLen)
	m = append(m, 1, 'a')
	for i := 0; i < 60; i++ { // a chain of pointers, each to the one before
		p := len(m) - 2
		m = append(m, 0xC0|byte(p>>8), byte(p))
	}
	m = append(m, 0)
	last := len(m) - 3
	n := 0
	for len(m)+13 < 65000 {
		m = append(m, 0xC0|byte(last>>8), byte(last), 0, 1, 0, 1, 0, 0, 0, 0, 0, 0)
		n++
	}
	binary.BigEndian.PutUint16(m[10:], uint16(n))
	start := time.Now()
	for range 10 {
		Parse(m)
	}
	if d := time.Since(start) / 10; d > 5*time.Millisecond {
		t.Fatalf("a crafted message took %s to parse", d)
	}
}

func TestFlagsKept(t *testing.T) {
	q := query(9, "example.com", 1232)
	q[3] |= 0x10 // CD
	in, _ := Parse(q)
	if r := Reply(in, RcodeServFail, false); r[3]&0x10 == 0 {
		t.Fatal("CD was not echoed")
	}
	// A big upstream answer with AA, AD and an extended rcode (BADVERS = 16).
	big := Reply(in, 0, false)
	big[2] |= 0x04 // AA
	big[3] |= 0x20 // AD
	big[len(big)-6] = 1
	big = append(big[:len(big)-11:len(big)-11], append(make([]byte, 0, 2000), big[len(big)-11:]...)...)
	big = append(big, make([]byte, 1500)...)
	binary.BigEndian.PutUint16(big[len(big)-1502:], 1500) // the OPT's rdlength covers the padding
	small, _ := Parse(query(9, "example.com", 512))
	small.Flags |= 0x10
	r := FitUDP(small, big)
	if len(r) > 512 || r[2]&0x04 == 0 || r[3]&0x20 == 0 || r[2]&0x02 == 0 || r[len(r)-6] != 1 {
		t.Fatalf("%x", r)
	}
}

// Names that end in a pointer to a long name, and pointers to names that
// themselves end in pointers, are fine.
func TestCompressedNames(t *testing.T) {
	for _, qname := range []string{"a.www.example.com", "x.a.www.example.com", "1.2.3.4.in-addr.arpa"} {
		m := query(1, qname, 0)
		binary.BigEndian.PutUint16(m[2:], flagQR)
		binary.BigEndian.PutUint16(m[6:], 2)
		// An answer owned by the question's name (0xC00C), then one owned by
		// "cdn" + a pointer to the first answer's owner.
		m = append(m, 0xC0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 1)
		first := len(m) - 16
		m = append(m, 3, 'c', 'd', 'n', 0xC0|byte(first>>8), byte(first), 0, 1, 0, 1, 0, 0, 0, 60, 0, 4, 192, 0, 2, 2)
		if _, err := Parse(m); err != nil {
			t.Errorf("%s: %v", qname, err)
		}
	}
}

func TestDOEchoed(t *testing.T) {
	q := query(1, "example.com", 1232)
	q[len(q)-4] = 0x80 // DO, in the OPT's flags
	in, _ := Parse(q)
	if !in.DO {
		t.Fatal("DO not read")
	}
	r := Reply(in, RcodeServFail, false)
	if a, _ := Parse(r); !a.DO {
		t.Fatal("DO not echoed")
	}
	q2 := query(1, "example.com", 1232)
	q2[len(q2)-4] = 0 // the helper sets DO
	in2, _ := Parse(q2)
	if a, _ := Parse(Reply(in2, RcodeServFail, false)); a.DO {
		t.Fatal("DO set without being asked")
	}
}
