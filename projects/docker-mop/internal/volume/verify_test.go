package volume

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
	"time"
)

type file struct {
	name string
	typ  byte
	mode int64
	uid  int
	body string
	link string
}

func archiveOf(t *testing.T, files ...file) *tar.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		h := &tar.Header{Name: f.name, Typeflag: f.typ, Mode: f.mode, Uid: f.uid, Linkname: f.link, ModTime: time.Unix(1700000000, 0)}
		if f.typ == tar.TypeReg {
			h.Size = int64(len(f.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if f.typ == tar.TypeReg {
			tw.Write([]byte(f.body))
		}
	}
	tw.Close()
	return tar.NewReader(&buf)
}

func manifestFrom(t *testing.T, files ...file) manifest {
	t.Helper()
	m, err := buildManifest(archiveOf(t, files...))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var base = []file{
	{name: "volume/", typ: tar.TypeDir, mode: 0o755},
	{name: "volume/a.txt", typ: tar.TypeReg, mode: 0o644, body: "hello"},
	{name: "volume/link", typ: tar.TypeSymlink, link: "a.txt"},
	{name: "volume/hard", typ: tar.TypeLink, link: "volume/a.txt"},
}

func TestManifestIdentical(t *testing.T) {
	a, b := manifestFrom(t, base...), manifestFrom(t, base...)
	if d := a.diff(b); len(d) != 0 {
		t.Fatalf("unexpected diffs: %v", d)
	}
	if r := a.report(); r.Files != 1 || r.Bytes != 5 {
		t.Fatalf("report = %+v", r)
	}
	if _, ok := a["hard"]; !ok || a["hard"].Link != "a.txt" {
		t.Fatalf("hard link not recorded relative to the volume: %+v", a["hard"])
	}
}

func TestManifestDetectsDifferences(t *testing.T) {
	changed := func(i int, f func(*file)) []file {
		out := append([]file(nil), base...)
		f(&out[i])
		return out
	}
	cases := map[string][]file{
		"content":      changed(1, func(f *file) { f.body = "HELLO" }),
		"mode":         changed(1, func(f *file) { f.mode = 0o600 }),
		"owner":        changed(1, func(f *file) { f.uid = 1000 }),
		"link target":  changed(2, func(f *file) { f.link = "b.txt" }),
		"missing file": base[:3],
		"extra file":   append(append([]file(nil), base...), file{name: "volume/b.txt", typ: tar.TypeReg, body: "x"}),
	}
	src := manifestFrom(t, base...)
	for name, files := range cases {
		if d := src.diff(manifestFrom(t, files...)); len(d) == 0 {
			t.Errorf("%s: difference not detected", name)
		}
	}
}

func TestVerifyErrorTruncates(t *testing.T) {
	var diffs []string
	for i := 0; i < 15; i++ {
		diffs = append(diffs, "x")
	}
	msg := (&VerifyError{Diffs: diffs}).Error()
	if !strings.Contains(msg, "15 differences") || !strings.Contains(msg, "and 5 more") {
		t.Fatalf("got %q", msg)
	}
}

func TestNewLabels(t *testing.T) {
	src := map[string]string{"app": "web", composeProjectLabel: "old", composeVolumeLabel: "data"}
	got, dropped := newLabels(src, "new_data", "new")
	if dropped || got[composeProjectLabel] != "new" || got[composeVolumeLabel] != "data" || got["app"] != "web" {
		t.Fatalf("got %v dropped=%v", got, dropped)
	}
	if src[composeProjectLabel] != "old" {
		t.Fatal("source labels were modified")
	}
	if err := checkComposeName("data", "new"); err == nil {
		t.Fatal("expected an error for a name without the project prefix")
	}
	got, dropped = newLabels(src, "anything", "")
	if !dropped || got[composeProjectLabel] != "" || got[composeVolumeLabel] != "" || got["app"] != "web" {
		t.Fatalf("without --compose-project the Compose labels must be dropped: %v dropped=%v", got, dropped)
	}
	if _, dropped := newLabels(map[string]string{"app": "web"}, "x", ""); dropped {
		t.Fatal("nothing to drop")
	}
}

func TestManifestComparesXattrs(t *testing.T) {
	write := func(cap string) manifest {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		h := &tar.Header{Name: "volume/bin", Typeflag: tar.TypeReg, Mode: 0o755, Format: tar.FormatPAX}
		if cap != "" {
			h.PAXRecords = map[string]string{"SCHILY.xattr.security.capability": cap}
		}
		tw.WriteHeader(h)
		tw.Close()
		m, err := buildManifest(tar.NewReader(&buf))
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if d := write("\x01").diff(write("")); len(d) == 0 {
		t.Fatal("a lost file capability was not detected")
	}
	if d := write("\x01").diff(write("\x01")); len(d) != 0 {
		t.Fatalf("unexpected diffs: %v", d)
	}
}
