package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func env(vars map[string]string, files map[string]string) (Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return Env{
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return vars[k] },
		ReadFile: func(name string) ([]byte, error) {
			if v, ok := files[name]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("no such file")
		},
	}, &out, &errb
}

func TestTokens(t *testing.T) {
	e, _, _ := env(map[string]string{"ISSUEMOVE_TOKEN": "shared", "ISSUEMOVE_SOURCE_TOKEN_FILE": "/src"}, map[string]string{"/src": "fromfile\n"})
	if tok, err := token(e, "SOURCE", true); err != nil || tok != "fromfile" {
		t.Fatalf("the side's file should win over the shared token: %q %v", tok, err)
	}
	if tok, err := token(e, "TARGET", true); err != nil || tok != "shared" {
		t.Fatalf("shared token on one instance: %q %v", tok, err)
	}
	if _, err := token(e, "TARGET", false); err == nil || !strings.Contains(err.Error(), "instances differ") {
		t.Fatalf("the shared token must not go to another instance: %v", err)
	}
}

func TestRunUsage(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"--url", "http://x", "plan"}, 2, "the command comes first"},
		{[]string{"plan", "-h"}, 0, "Usage:"},
		{[]string{"plan", "--state", "bad", "a/b", "c/d"}, 2, "--state must be"},
		{[]string{"plan", "--source-url", "http://a", "--target-url", "http://b", "a/b#1", "c/d"}, 1, "instances differ"},
	} {
		e, out, errb := env(map[string]string{"ISSUEMOVE_TOKEN": "t"}, nil)
		code := Run(context.Background(), e, c.args)
		if code != c.code || !strings.Contains(out.String()+errb.String(), c.out) {
			t.Errorf("%v: exit %d, output %q", c.args, code, out.String()+errb.String())
		}
	}
	// A value that looks like -h is a value, not a request for help.
	o, err := parse([]string{"--label", "-h", "a/b", "c/d"})
	if err != nil || o.help || o.labels != "-h" {
		t.Fatalf("parse: %+v %v", o, err)
	}
}

func TestConfirmStopsOnInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked, _ := pipeReader()
	if _, err := confirm(ctx, blocked, &bytes.Buffer{}, "go?"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if yes, _ := confirm(context.Background(), strings.NewReader("y\n"), &bytes.Buffer{}, "go?"); !yes {
		t.Fatal("y was not yes")
	}
}

// pipeReader is a reader that never returns (until closed).
func pipeReader() (*blockingReader, func()) {
	r := &blockingReader{done: make(chan struct{})}
	return r, func() { close(r.done) }
}

type blockingReader struct{ done chan struct{} }

func (r *blockingReader) Read([]byte) (int, error) { <-r.done; return 0, errors.New("closed") }
