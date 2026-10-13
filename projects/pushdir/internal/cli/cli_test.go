package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	c, err := parse([]string{"--delete", "/data", "--max-delete", "7", "b2:bucket/x", "--", "--bwlimit", "5M", "--exclude", "*.tmp"})
	if err != nil || c.show != "" {
		t.Fatalf("err=%v show=%q", err, c.show)
	}
	o := c.opts
	if !o.Delete || o.MaxDelete != 7 || o.Local != "/data" || o.Remote != "b2:bucket/x" {
		t.Fatalf("got %+v", o)
	}
	if strings.Join(o.Runner.Extra, " ") != "--bwlimit 5M --exclude *.tmp" {
		t.Fatalf("rclone flags: %v", o.Runner.Extra)
	}
	if o.Quiet != 2*time.Second || o.Resync != 6*time.Hour {
		t.Fatalf("defaults: %+v", o)
	}
	for _, bad := range [][]string{
		{"/data"},
		{"/a", "r:", "extra"},
		{"--quiet", "0s", "/a", "r:"},
		{"--quiet", "10s", "--max-delay", "1s", "/a", "r:"},
		{"--max-delete", "-1", "/a", "r:"},
		{"--nope", "/a", "r:"},
		{"--max-delete", "0", "/a", "r:"},
		{"--settle", "0", "/a", "r:"},
		{"/a", ""},
		{"/a", "relative/dir"},
		{"/a", "/a/backup"},
		{"/a/sub", "/a"},
		{"/a", ":local:/a/x"},
		{"/a", "r:", "--backup-dir", "/a/old"},
		{"/a", "-r:"},
	} {
		if _, err := parse(bad); err == nil {
			t.Errorf("%v: expected an error", bad)
		}
	}
}

func TestHelpAndVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errOut, Getenv: func(string) string { return "" }}
	if code := Run(context.Background(), env, []string{"--help"}); code != 0 || !strings.Contains(out.String(), "Usage: pushdir") {
		t.Fatalf("help: %d %q", code, out.String())
	}
	out.Reset()
	if code := Run(context.Background(), env, []string{"--version"}); code != 0 || !strings.HasPrefix(out.String(), "pushdir ") {
		t.Fatalf("version: %d %q", code, out.String())
	}
	for _, v := range []string{"-version", "--version=true"} {
		out.Reset()
		if code := Run(context.Background(), env, []string{v}); code != 0 || !strings.HasPrefix(out.String(), "pushdir ") {
			t.Fatalf("%s: %d %q", v, code, out.String())
		}
	}
	if code := Run(context.Background(), env, []string{"only-one"}); code != 2 {
		t.Fatalf("usage error should exit 2, got %d", code)
	}
}

func TestGoodDestinations(t *testing.T) {
	for _, dest := range []string{"b2:bucket", "/srv/backup", ":s3,provider=AWS:bkt", "/ab"} {
		if _, err := parse([]string{"/a", dest}); err != nil {
			t.Errorf("%s: %v", dest, err)
		}
	}
}

func TestSecondInstanceIsRefused(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := lock(dir); err == nil || !strings.Contains(err.Error(), "already watching") {
		t.Fatalf("got %v", err)
	}
}
