package rclone

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func rcloneBin(t *testing.T) string {
	t.Helper()
	if b := os.Getenv("RCLONE_BIN"); b != "" {
		return b
	}
	b, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not found (set RCLONE_BIN)")
	}
	return b
}

// TestSelectMatchesRclone checks that Select picks exactly the files rclone
// itself copies with the same flags.
func TestSelectMatchesRclone(t *testing.T) {
	bin := rcloneBin(t)
	base := t.TempDir()
	src := filepath.Join(base, "src")
	files := []string{
		"a.txt", "b.tmp", "c.jpg", "sub/d.txt", "sub/e.jpg", "sub/deep/f.tmp",
		"node_modules/x.js", "x*y.txt", "xzy.txt", "[br]{ce}.txt", "trailing ",
		"q?.t(x)+", "back\\slash", "Upper.TXT", "keep/.ignore-me", "keep/g.txt",
	}
	for _, f := range files {
		p := filepath.Join(src, f)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(strings.Repeat("z", len(f))), 0o644))
	}
	inc := filepath.Join(base, "inc.txt")
	must(t, os.WriteFile(inc, []byte("# comment\n\n*.jpg\n  sub/**  \n"), 0o644))
	flt := filepath.Join(base, "flt.txt")
	must(t, os.WriteFile(flt, []byte("- node_modules/**\n+ *.txt\n- *\n"), 0o644))
	conf := []string{"--config", filepath.Join(base, "rclone.conf")}

	cases := [][]string{
		{"--exclude", "*.tmp"},
		{"--exclude=*.tmp", "--exclude", "node_modules/**"},
		{"--include", "*.jpg"},
		{"--include", "*.txt", "--exclude", "sub/**"},
		{"--include-from", inc},
		{"--include-from", inc, "--exclude", "*.jpg"},
		{"--filter-from", flt},
		{"-f", "- *.tmp", "--filter", "+ sub/**", "--filter", "- *"},
		{"--filter", "- *.jpg", "--filter", "!", "--filter", "- *.txt"},
		{"--ignore-case", "--exclude", "*.txt"},
		{"--max-size", "6B"},
		{"--min-size=10B", "--exclude", "*.tmp"},
		{"--exclude-if-present", ".ignore-me"},
		{"--exclude", "x\\*y.txt"},
	}
	for _, flags := range cases {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			dst := filepath.Join(t.TempDir(), "dst")
			cmd := exec.Command(bin, append(append([]string{"copy", src, dst}, conf...), flags...)...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("rclone copy: %v\n%s", err, out)
			}
			var want []string
			filepath.Walk(dst, func(p string, info os.FileInfo, err error) error {
				if err == nil && !info.IsDir() {
					rel, _ := filepath.Rel(dst, p)
					want = append(want, filepath.ToSlash(rel))
				}
				return nil
			})
			r := &Runner{Bin: bin, Extra: append(append([]string{}, conf...), flags...)}
			if err := r.Check(); err != nil {
				t.Fatal(err)
			}
			got, err := r.Select(context.Background(), src, files)
			if err != nil {
				t.Fatal(err)
			}
			sort.Strings(want)
			sort.Strings(got)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Select picked %q\nrclone copied  %q", got, want)
			}
		})
	}
}

func TestSplitFlags(t *testing.T) {
	f := splitFlags([]string{"--bwlimit", "5M", "--exclude", "*.tmp", "--include=*.jpg", "-fx", "--max_size", "1M", "-v", "--ignore-case"})
	if f.err != nil {
		t.Fatal(f.err)
	}
	if !reflect.DeepEqual(f.plain, []string{"--bwlimit", "5M", "-v"}) {
		t.Errorf("plain = %q", f.plain)
	}
	if !reflect.DeepEqual(f.sel, []string{"--max-size=1M", "--ignore-case"}) {
		t.Errorf("sel = %q", f.sel)
	}
	if f.rules["exclude"][0] != "*.tmp" || f.rules["include"][0] != "*.jpg" || f.rules["filter"][0] != "x" {
		t.Errorf("rules = %q", f.rules)
	}
	for _, bad := range [][]string{{"--files-from", "x"}, {"--delete-excluded"}, {"--exclude"}, {"--filter-from", "-"}} {
		if f := splitFlags(bad); f.err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestFilterEnvironmentIsRefused(t *testing.T) {
	t.Setenv("RCLONE_EXCLUDE", "*.tmp")
	if err := (&Runner{}).Check(); err == nil || !strings.Contains(err.Error(), "RCLONE_EXCLUDE") {
		t.Fatalf("got %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
