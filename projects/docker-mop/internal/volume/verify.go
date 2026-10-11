package volume

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
)

// entry is what must match between source and copy for one path.
type entry struct {
	Type     byte
	Mode     int64
	UID, GID int
	Size     int64
	SHA256   string
	Link     string
	ModTime  int64 // seconds
}

func (e entry) String() string {
	return fmt.Sprintf("type=%c mode=%o owner=%d:%d size=%d link=%q mtime=%d sha256=%.12s", e.Type, e.Mode, e.UID, e.GID, e.Size, e.Link, e.ModTime, e.SHA256)
}

// manifest maps each path (relative to the volume root) to its entry.
type manifest map[string]entry

// buildManifest reads a tar stream and records every entry, hashing file
// contents as it goes.
func buildManifest(tr *tar.Reader) (manifest, error) {
	m := manifest{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return m, nil
		}
		if err != nil {
			return nil, err
		}
		e := entry{
			Type:    normalizeType(hdr.Typeflag),
			Mode:    hdr.Mode & 0o7777,
			UID:     hdr.Uid,
			GID:     hdr.Gid,
			ModTime: hdr.ModTime.Unix(),
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			h := sha256.New()
			n, err := io.Copy(h, tr)
			if err != nil {
				return nil, err
			}
			e.Size = n
			e.SHA256 = hex.EncodeToString(h.Sum(nil))
		case tar.TypeSymlink:
			e.Link = hdr.Linkname
		case tar.TypeLink:
			e.Link = relPath(hdr.Linkname)
		case tar.TypeDir:
			// Directory mtimes change as entries are written into them; the
			// copy keeps them, but Docker's extraction may not set the root
			// directory's, so only compare file and link times.
			e.ModTime = 0
		}
		m[relPath(hdr.Name)] = e
	}
}

// normalizeType treats the old and new regular-file flags the same.
func normalizeType(t byte) byte {
	if t == tar.TypeRegA {
		return tar.TypeReg
	}
	return t
}

// relPath strips the mount point's directory name from an archive path.
func relPath(name string) string {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimSuffix(name, "/")
	if i := strings.IndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return "" // the volume root itself
}

func (m manifest) report() Report {
	var r Report
	for _, e := range m {
		if e.Type == tar.TypeReg {
			r.Files++
			r.Bytes += e.Size
		}
	}
	return r
}

// diff lists every difference between m (the source) and other (the copy).
func (m manifest) diff(other manifest) []string {
	var out []string
	for path, want := range m {
		got, ok := other[path]
		if !ok {
			out = append(out, fmt.Sprintf("%s: missing in the copy", display(path)))
			continue
		}
		if want != got {
			out = append(out, fmt.Sprintf("%s: source %s, copy %s", display(path), want, got))
		}
	}
	for path := range other {
		if _, ok := m[path]; !ok {
			out = append(out, fmt.Sprintf("%s: only in the copy", display(path)))
		}
	}
	sort.Strings(out)
	return out
}

func display(path string) string {
	if path == "" {
		return "/"
	}
	return "/" + path
}

// VerifyError reports that the copy does not match the source.
type VerifyError struct {
	Diffs []string
}

func (e *VerifyError) Error() string {
	const max = 10
	var b strings.Builder
	fmt.Fprintf(&b, "the copy does not match the source (%d %s)", len(e.Diffs), pluralDiffs(len(e.Diffs)))
	for i, d := range e.Diffs {
		if i == max {
			fmt.Fprintf(&b, "\n  ... and %d more", len(e.Diffs)-max)
			break
		}
		b.WriteString("\n  " + d)
	}
	return b.String()
}

func pluralDiffs(n int) string {
	if n == 1 {
		return "difference"
	}
	return "differences"
}
