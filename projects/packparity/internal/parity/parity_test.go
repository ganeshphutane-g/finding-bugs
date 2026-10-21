package parity

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
)

func random(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestLayout(t *testing.T) {
	for _, c := range []struct {
		size             int64
		bs, n, g, k, m   int
		wantParityApprox int64
	}{
		{0, 4096, 0, 0, 0, 0, 0},
		{300, 320, 1, 1, 1, 1, 320},
		{4096, 4096, 1, 1, 1, 1, 4096},
		{40 << 10, 4096, 10, 1, 10, 1, 4096},
		{16 << 20, 4096, 4096, 41, 100, 10, 410 * 4096},
		{16<<20 + 1, 4096, 4097, 41, 100, 10, 410 * 4096},
	} {
		l, err := NewLayout(c.size, Defaults)
		if err != nil {
			t.Fatal(err)
		}
		if l.BlockSize != c.bs || l.Blocks != c.n || l.Groups != c.g || l.K != c.k || l.M != c.m {
			t.Errorf("%d: %+v", c.size, l)
		}
		if l.Groups*l.M*l.BlockSize != int(c.wantParityApprox) {
			t.Errorf("%d: parity blocks %d", c.size, l.Groups*l.M*l.BlockSize)
		}
	}
	for _, p := range []Params{{0, 4096}, {101, 4096}, {10, 100}, {10, 0}} {
		if _, err := NewLayout(1, p); err == nil {
			t.Errorf("%+v accepted", p)
		}
	}
}

func roundTrip(t *testing.T, data []byte, p Params) *File {
	t.Helper()
	pb, err := Encode(data, p)
	if err != nil {
		t.Fatal(err)
	}
	l, _ := NewLayout(int64(len(data)), p)
	if int64(len(pb)) != l.ParitySize() {
		t.Fatalf("parity is %d bytes, layout says %d", len(pb), l.ParitySize())
	}
	f, err := Parse(pb)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.Check(data); !r.OK {
		t.Fatalf("intact data reported: %+v", r)
	}
	return f
}

func TestSizes(t *testing.T) {
	for _, n := range []int{0, 1, 63, 64, 65, 4095, 4096, 4097, 100 * 4096, 100*4096 + 1, 1 << 20, 3<<20 + 17} {
		data := random(n, int64(n))
		f := roundTrip(t, data, Defaults)
		if n == 0 {
			continue
		}
		// Damage the first byte: always repairable.
		bad := append([]byte(nil), data...)
		bad[0] ^= 0xff
		r := f.Check(bad)
		if r.OK || !r.Repairable || len(r.BadBlocks) != 1 {
			t.Fatalf("%d: %+v", n, r)
		}
		got, err := f.Repair(bad)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%d: %v", n, err)
		}
	}
}

func TestBurstAndScatter(t *testing.T) {
	data := random(16<<20, 1)
	f := roundTrip(t, data, Defaults)
	// A run of 410 consecutive blocks (1.6 MiB) zeroed: 10 per group.
	bad := append([]byte(nil), data...)
	clear(bad[1000*4096 : 1410*4096])
	if r := f.Check(bad); !r.Repairable || r.WorstGroup != 10 || len(r.BadBlocks) != 410 {
		t.Fatalf("%+v", r.WorstGroup)
	}
	if got, err := f.Repair(bad); err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	// One block more is too much for one group.
	clear(bad[1410*4096 : 1411*4096])
	r := f.Check(bad)
	if r.Repairable || r.WorstGroup != 11 {
		t.Fatalf("%+v", r.WorstGroup)
	}
	if _, err := f.Repair(bad); !errors.Is(err, ErrUnrepairable) {
		t.Fatal(err)
	}
	// Scattered single-bit flips, one per block in 300 random blocks.
	bad = append(bad[:0], data...)
	rng := rand.New(rand.NewSource(2))
	perGroup := map[int]int{}
	for _, i := range rng.Perm(4096)[:300] {
		if perGroup[i%41] == 10 {
			continue
		}
		perGroup[i%41]++
		bad[i*4096+rng.Intn(4096)] ^= 1 << rng.Intn(8)
	}
	if got, err := f.Repair(bad); err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
}

func TestShortLongFiles(t *testing.T) {
	data := random(200<<10, 3)
	f := roundTrip(t, data, Defaults)
	// Truncated by 3 blocks and a bit: the tail is rebuilt.
	short := data[:len(data)-3*4096-100]
	if r := f.Check(short); !r.SizeWrong || !r.Repairable {
		t.Fatalf("%+v", r)
	}
	if got, err := f.Repair(short); err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	// Junk appended: cut off.
	long := append(append([]byte(nil), data...), 1, 2, 3)
	if r := f.Check(long); r.OK || !r.SizeWrong || len(r.BadBlocks) != 0 {
		t.Fatalf("%+v", r)
	}
	if got, err := f.Repair(long); err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	// Gone entirely: not repairable at 10%.
	if r := f.Check(nil); r.Repairable {
		t.Fatalf("%+v", r)
	}
	// At 100% a lost file can come back.
	f = roundTrip(t, data, Params{Redundancy: 100, BlockSize: 4096})
	if got, err := f.Repair(nil); err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
}

func TestDamagedParity(t *testing.T) {
	data := random(1<<20, 4)
	pb, _ := Encode(data, Defaults)
	// A damaged parity block costs that group one repair.
	pb2 := append([]byte(nil), pb...)
	pb2[len(pb2)-1] ^= 1
	f, err := Parse(pb2)
	if err != nil || f.BadParity != 1 {
		t.Fatal(err, f.BadParity)
	}
	if r := f.Check(data); !r.OK {
		t.Fatal(r)
	}
	// The header and the table are checked.
	for _, at := range []int{0, 20, headerLen + hashLen + 5} {
		bad := append([]byte(nil), pb...)
		bad[at] ^= 1
		if _, err := Parse(bad); !errors.Is(err, ErrDamaged) {
			t.Errorf("byte %d: %v", at, err)
		}
	}
	if _, err := Parse(pb[:len(pb)-1]); !errors.Is(err, ErrDamaged) {
		t.Fatal(err)
	}
	if _, err := Parse([]byte("nonsense")); !errors.Is(err, ErrDamaged) {
		t.Fatal(err)
	}
}
