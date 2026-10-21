// Package parity builds and uses the repair data for one file: the file is
// cut into blocks, block i goes to group i mod G (so a run of bad blocks is
// spread over the groups), and each group gets Reed-Solomon parity blocks.
// Every block has a short hash, so bad blocks are found before they are
// rebuilt, and the rebuilt file is checked against the file's SHA-256.
package parity

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/klauspost/reedsolomon"
)

const (
	magic   = "PKPARITY"
	version = 1
	// DefaultBlockSize is a disk sector's worth (smaller for small files).
	DefaultBlockSize = 4096
	// GroupData is the most data blocks per group.
	GroupData = 100
	hashLen   = 16
	headerLen = 8 + 2 + 2 + 4 + 8 + 4 + 2 + 2 + 32
)

// Params are the settings for new parity.
type Params struct {
	// Redundancy is parity per data, in percent (1 to 100).
	Redundancy int
	// BlockSize is the block size for files of at least that size.
	BlockSize int
}

// Defaults: 10% redundancy, 4 KiB blocks.
var Defaults = Params{Redundancy: 10, BlockSize: DefaultBlockSize}

// Layout is how a file is cut up.
type Layout struct {
	FileSize  int64
	BlockSize int
	Blocks    int // data blocks
	Groups    int
	K, M      int // data and parity blocks per group
}

// NewLayout works out the layout for a file of size n.
func NewLayout(n int64, p Params) (Layout, error) {
	if p.Redundancy < 1 || p.Redundancy > 100 {
		return Layout{}, fmt.Errorf("redundancy %d%%: it must be 1 to 100", p.Redundancy)
	}
	if p.BlockSize < 64 || p.BlockSize%64 != 0 || p.BlockSize > 1<<20 {
		return Layout{}, fmt.Errorf("block size %d: it must be a multiple of 64, up to 1 MiB", p.BlockSize)
	}
	l := Layout{FileSize: n, BlockSize: p.BlockSize}
	if n == 0 {
		return l, nil
	}
	if n < int64(p.BlockSize) {
		// One block, rounded up to 64 bytes: a small file is simply copied.
		l.BlockSize = int((n + 63) / 64 * 64)
	}
	l.Blocks = int((n + int64(l.BlockSize) - 1) / int64(l.BlockSize))
	l.Groups = (l.Blocks + GroupData - 1) / GroupData
	l.K = (l.Blocks + l.Groups - 1) / l.Groups
	l.M = (l.K*p.Redundancy + 99) / 100
	return l, nil
}

// block is data block i's place: its group and position in the group.
func (l Layout) block(i int) (group, pos int) { return i % l.Groups, i / l.Groups }

// index is the data block at a group position (>= Blocks: a zero pad block).
func (l Layout) index(group, pos int) int { return pos*l.Groups + group }

// ParitySize is the size of the parity file.
func (l Layout) ParitySize() int64 {
	np := int64(l.Groups * l.M)
	return headerLen + hashLen + int64(l.Blocks)*hashLen + np*hashLen + hashLen + np*int64(l.BlockSize)
}

func short(b []byte) (h [hashLen]byte) {
	s := sha256.Sum256(b)
	copy(h[:], s[:hashLen])
	return h
}

var (
	encMu    sync.Mutex
	encoders = map[[2]int]reedsolomon.Encoder{}
)

func encoder(k, m int) (reedsolomon.Encoder, error) {
	encMu.Lock()
	defer encMu.Unlock()
	if e, ok := encoders[[2]int{k, m}]; ok {
		return e, nil
	}
	e, err := reedsolomon.New(k, m)
	if err != nil {
		return nil, err
	}
	encoders[[2]int{k, m}] = e
	return e, nil
}

// dataBlock is block i of data, padded with zeros to the block size; nil
// when the data does not reach it.
func (l Layout) dataBlock(data []byte, i int) []byte {
	off := int64(i) * int64(l.BlockSize)
	if i >= l.Blocks || off >= int64(len(data)) {
		return nil
	}
	end := off + int64(l.BlockSize)
	b := make([]byte, l.BlockSize)
	end = min(end, int64(len(data)), l.FileSize)
	copy(b, data[off:end])
	return b
}

// Encode builds the parity file for data.
func Encode(data []byte, p Params) ([]byte, error) {
	l, err := NewLayout(int64(len(data)), p)
	if err != nil {
		return nil, err
	}
	var hdr bytes.Buffer
	hdr.WriteString(magic)
	le := binary.LittleEndian
	hdr.Write(le.AppendUint16(nil, version))
	hdr.Write(le.AppendUint16(nil, 0))
	hdr.Write(le.AppendUint32(nil, uint32(l.BlockSize)))
	hdr.Write(le.AppendUint64(nil, uint64(l.FileSize)))
	hdr.Write(le.AppendUint32(nil, uint32(l.Groups)))
	hdr.Write(le.AppendUint16(nil, uint16(l.K)))
	hdr.Write(le.AppendUint16(nil, uint16(l.M)))
	sum := sha256.Sum256(data)
	hdr.Write(sum[:])
	hh := short(hdr.Bytes())
	hdr.Write(hh[:])

	parity := make([][]byte, l.Groups*l.M)
	if l.Blocks > 0 {
		enc, err := encoder(l.K, l.M)
		if err != nil {
			return nil, err
		}
		zero := make([]byte, l.BlockSize)
		for g := 0; g < l.Groups; g++ {
			shards := make([][]byte, l.K+l.M)
			for pos := 0; pos < l.K; pos++ {
				if b := l.dataBlock(data, l.index(g, pos)); b != nil {
					shards[pos] = b
				} else {
					shards[pos] = zero
				}
			}
			for j := 0; j < l.M; j++ {
				shards[l.K+j] = make([]byte, l.BlockSize)
			}
			if err := enc.Encode(shards); err != nil {
				return nil, err
			}
			copy(parity[g*l.M:], shards[l.K:])
		}
	}

	var table bytes.Buffer
	for i := 0; i < l.Blocks; i++ {
		h := short(l.dataBlock(data, i))
		table.Write(h[:])
	}
	for _, b := range parity {
		h := short(b)
		table.Write(h[:])
	}
	th := short(table.Bytes())
	table.Write(th[:])

	out := make([]byte, 0, l.ParitySize())
	out = append(out, hdr.Bytes()...)
	out = append(out, table.Bytes()...)
	for _, b := range parity {
		out = append(out, b...)
	}
	return out, nil
}

// File is a parsed parity file.
type File struct {
	Layout
	// SHA256 is the whole file's hash.
	SHA256 [32]byte
	data   [][hashLen]byte
	phash  [][hashLen]byte
	parity [][]byte // nil where the parity block is damaged
	// BadParity counts parity blocks that are damaged.
	BadParity int
}

// ErrDamaged means the parity file's header or block table is damaged: it
// cannot be used, and should be made again from good data.
var ErrDamaged = errors.New("the parity file is damaged")

// Parse reads a parity file. Damaged parity blocks are noted and left out;
// a damaged header or table is ErrDamaged.
func Parse(b []byte) (*File, error) {
	if len(b) < headerLen+hashLen || string(b[:8]) != magic {
		return nil, fmt.Errorf("%w: not a parity file", ErrDamaged)
	}
	if short(b[:headerLen]) != [hashLen]byte(b[headerLen:headerLen+hashLen]) {
		return nil, fmt.Errorf("%w: its header does not match its checksum", ErrDamaged)
	}
	le := binary.LittleEndian
	if v := le.Uint16(b[8:]); v != version {
		return nil, fmt.Errorf("parity format version %d is not known to this packparity", v)
	}
	f := &File{}
	f.BlockSize = int(le.Uint32(b[12:]))
	f.FileSize = int64(le.Uint64(b[16:]))
	f.Groups = int(le.Uint32(b[24:]))
	f.K = int(le.Uint16(b[28:]))
	f.M = int(le.Uint16(b[30:]))
	copy(f.SHA256[:], b[32:64])
	if f.FileSize > 0 {
		if f.BlockSize < 64 || f.Groups < 1 || f.K < 1 || f.M < 1 || f.K+f.M > 256 {
			return nil, fmt.Errorf("%w: its layout is impossible", ErrDamaged)
		}
		f.Blocks = int((f.FileSize + int64(f.BlockSize) - 1) / int64(f.BlockSize))
		// K must be ceil(Blocks/Groups), as NewLayout makes it.
		if f.Groups > f.Blocks || f.Blocks > f.Groups*f.K || f.Blocks <= f.Groups*(f.K-1) {
			return nil, fmt.Errorf("%w: its layout does not fit the file size", ErrDamaged)
		}
	}
	if int64(len(b)) != f.ParitySize() {
		return nil, fmt.Errorf("%w: it is %d bytes, not %d", ErrDamaged, len(b), f.ParitySize())
	}
	np := f.Groups * f.M
	off := headerLen + hashLen
	tableEnd := off + (f.Blocks+np)*hashLen
	if short(b[off:tableEnd]) != [hashLen]byte(b[tableEnd:tableEnd+hashLen]) {
		return nil, fmt.Errorf("%w: its block table does not match its checksum", ErrDamaged)
	}
	for i := 0; i < f.Blocks; i++ {
		f.data = append(f.data, [hashLen]byte(b[off+i*hashLen:]))
	}
	off += f.Blocks * hashLen
	for i := 0; i < np; i++ {
		f.phash = append(f.phash, [hashLen]byte(b[off+i*hashLen:]))
	}
	off = tableEnd + hashLen
	for i := 0; i < np; i++ {
		blk := b[off+i*f.BlockSize : off+(i+1)*f.BlockSize]
		if short(blk) == f.phash[i] {
			f.parity = append(f.parity, blk)
		} else {
			f.parity = append(f.parity, nil)
			f.BadParity++
		}
	}
	return f, nil
}

// Report is what a check of data against its parity found.
type Report struct {
	OK bool
	// BadBlocks are the data blocks that do not match (including blocks
	// missing because the file is too short).
	BadBlocks []int
	// Repairable: every group has enough good blocks to rebuild.
	Repairable bool
	// WorstGroup is the most bad blocks in one group, against M.
	WorstGroup int
	// SizeWrong: the file is not the size it was.
	SizeWrong bool
}

// Check compares data with the parity.
func (f *File) Check(data []byte) Report {
	r := Report{SizeWrong: int64(len(data)) != f.FileSize}
	if !r.SizeWrong && sha256.Sum256(data) == f.SHA256 {
		r.OK, r.Repairable = true, true
		return r
	}
	bad := make([]int, f.Groups)
	for i := 0; i < f.Blocks; i++ {
		b := f.dataBlock(data, i)
		if b == nil || short(b) != f.data[i] {
			r.BadBlocks = append(r.BadBlocks, i)
			g, _ := f.block(i)
			bad[g]++
		}
	}
	r.Repairable = true
	for g := 0; g < f.Groups; g++ {
		lost := bad[g]
		for j := 0; j < f.M; j++ {
			if f.parity[g*f.M+j] == nil {
				lost++
			}
		}
		if bad[g] > r.WorstGroup {
			r.WorstGroup = bad[g]
		}
		if lost > f.M {
			r.Repairable = false
		}
	}
	// Every block matches but the whole does not (only extra bytes at the
	// end, or a hash collision): cutting to size is the repair.
	return r
}

// ErrUnrepairable means some group lost more blocks than it has parity.
var ErrUnrepairable = errors.New("too much is damaged to rebuild")

// Repair rebuilds the file from data (damaged, short or long) and the
// parity, and checks the result against the file's SHA-256.
func (f *File) Repair(data []byte) ([]byte, error) {
	out := make([]byte, int64(f.Blocks)*int64(f.BlockSize))
	if f.Blocks > 0 {
		enc, err := encoder(f.K, f.M)
		if err != nil {
			return nil, err
		}
		for g := 0; g < f.Groups; g++ {
			shards := make([][]byte, f.K+f.M)
			for pos := 0; pos < f.K; pos++ {
				i := f.index(g, pos)
				if i >= f.Blocks {
					shards[pos] = make([]byte, f.BlockSize)
					continue
				}
				if b := f.dataBlock(data, i); b != nil && short(b) == f.data[i] {
					shards[pos] = b
				}
			}
			for j := 0; j < f.M; j++ {
				if p := f.parity[g*f.M+j]; p != nil {
					shards[f.K+j] = append([]byte(nil), p...)
				}
			}
			if err := enc.ReconstructData(shards); err != nil {
				return nil, fmt.Errorf("%w (block group %d): %v", ErrUnrepairable, g, err)
			}
			for pos := 0; pos < f.K; pos++ {
				i := f.index(g, pos)
				if i >= f.Blocks {
					continue
				}
				if short(shards[pos]) != f.data[i] {
					return nil, fmt.Errorf("%w: block %d did not rebuild to its hash", ErrUnrepairable, i)
				}
				copy(out[int64(i)*int64(f.BlockSize):], shards[pos])
			}
		}
	}
	out = out[:f.FileSize]
	if sha256.Sum256(out) != f.SHA256 {
		return nil, fmt.Errorf("%w: the rebuilt file does not match its SHA-256", ErrUnrepairable)
	}
	return out, nil
}
