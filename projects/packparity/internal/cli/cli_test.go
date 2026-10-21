package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func repoDir(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "config"), []byte("cfg"), 0o400)
	os.MkdirAll(filepath.Join(root, "keys"), 0o700)
	b := bytes.Repeat([]byte("pack data "), 50000)
	sum := sha256.Sum256(b)
	id := hex.EncodeToString(sum[:])
	p := filepath.Join(root, "data", id[:2], id)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, b, 0o400)
	return root, p
}

func pp(args ...string) (string, int) {
	var out bytes.Buffer
	code := Main(args, &out, &out)
	return out.String(), code
}

func TestExitCodes(t *testing.T) {
	repo, pack := repoDir(t)
	par := filepath.Join(t.TempDir(), "p")
	if out, code := pp("sync", repo, par); code != 0 || !strings.Contains(out, "2 new files covered") {
		t.Fatalf("%d %s", code, out)
	}
	// Flags after the positional arguments work too.
	if out, code := pp("check", repo, par, "--read", "all"); code != 0 || !strings.Contains(out, "read 2 files") {
		t.Fatalf("%d %s", code, out)
	}
	os.Chmod(pack, 0o600)
	f, _ := os.OpenFile(pack, os.O_RDWR, 0)
	f.WriteAt([]byte("XX"), 4096*3)
	f.Close()
	if out, code := pp("check", "--read", "all", repo, par); code != 3 || !strings.Contains(out, "REPAIRABLE") {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := pp("status", repo, par); code != 3 || !strings.Contains(out, "damaged") {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := pp("repair", repo, par); code != 0 || !strings.Contains(out, "repaired") {
		t.Fatalf("%d %s", code, out)
	}
	if out, code := pp("check", "--read", "all", "--json", repo, par); code != 0 || !strings.Contains(out, `"reports": []`) {
		t.Fatalf("%d %s", code, out)
	}
	// A new file that does not match its name, old enough to be damage:
	// exit 4, nothing to repair it from.
	bad := filepath.Join(filepath.Dir(pack), filepath.Base(pack)[:2]+strings.Repeat("0", 62))
	os.WriteFile(bad, []byte("not what the name says"), 0o400)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(bad, old, old)
	if out, code := pp("sync", repo, par); code != 4 || !strings.Contains(out, "DAMAGED") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"nope"},
		{"sync", "only-one"},
		{"sync", "--redundancy", "0", "a", "b"},
		{"sync", "--keep-removed", "soon", "a", "b"},
		{"check", "--read", "some", "a", "b"},
		{"check", "--read", "200%", "a", "b"},
		{"repair", "a"},
	} {
		if _, code := pp(args...); code != 2 {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if out, code := pp("version"); code != 0 || !strings.Contains(out, "packparity") {
		t.Fatal(out)
	}
	if _, code := pp("check", t.TempDir(), t.TempDir()); code != 1 {
		t.Fatal(code)
	}
}
