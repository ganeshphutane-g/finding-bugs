package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	c, done, err := parse([]string{"--delete", "/data", "--max-delete", "7", "b2:bucket/x", "--", "--bwlimit", "5M", "--exclude", "*.tmp"})
	if err != nil || done {
		t.Fatalf("err=%v done=%v", err, done)
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
	} {
		if _, _, err := parse(bad); err == nil {
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
	if code := Run(context.Background(), env, []string{"only-one"}); code != 2 {
		t.Fatalf("usage error should exit 2, got %d", code)
	}
}
