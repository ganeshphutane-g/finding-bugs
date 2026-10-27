package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type msg struct{ Type, Message string }

// call runs composehost as Compose would, in dir, and returns its messages.
func call(t *testing.T, dir string, args ...string) (int, []msg, string) {
	t.Helper()
	old, _ := os.Getwd()
	os.Chdir(dir)
	defer os.Chdir(old)
	var out, errb bytes.Buffer
	code := Provider(context.Background(), append([]string{"compose", "--project-name=demo"}, args...), &out, &errb)
	var msgs []msg
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m msg
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not a JSON line: %q", l)
		}
		msgs = append(msgs, m)
	}
	return code, msgs, errb.String()
}

func find(msgs []msg, typ, sub string) bool {
	for _, m := range msgs {
		if m.Type == typ && strings.Contains(m.Message, sub) {
			return true
		}
	}
	return false
}

func TestMetadata(t *testing.T) {
	var out bytes.Buffer
	if code := Provider(context.Background(), []string{"compose", "metadata"}, &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	var m struct {
		Up   struct{ Parameters []struct{ Name string } }
		Down struct{ Parameters []struct{ Name string } }
		Stop *struct{ Parameters []struct{ Name string } }
	}
	if err := json.Unmarshal(out.Bytes(), &m); err != nil || len(m.Up.Parameters) != 10 || m.Stop == nil || m.Down.Parameters[0].Name != "down" {
		t.Fatalf("%v %s", err, out.String())
	}
}

func TestUp(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ready"), nil, 0o644)
	code, msgs, _ := call(t, dir, "up", "--mkdir=./data", "--mode=0700", "--wait-path=ready",
		"--run=echo hi > data/x", "--env=echo A=1; echo B=two", "host")
	if code != 0 {
		t.Fatalf("%d %v", code, msgs)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "data/x")); string(b) != "hi\n" {
		t.Errorf("run did not run in the project directory: %q", b)
	}
	if !find(msgs, "rawsetenv", "A=1") || !find(msgs, "rawsetenv", "B=two") || !find(msgs, "info", "created ./data") {
		t.Errorf("%v", msgs)
	}
	// The order: mkdir before run (run writes into it), env last.
	var order []string
	for _, m := range msgs {
		if m.Type == "info" {
			order = append(order, strings.SplitN(m.Message, " ", 2)[0])
		}
	}
	if strings.Join(order, ",") != "created,run:,env:,env:" {
		t.Errorf("order %v", order)
	}
}

func TestUpFails(t *testing.T) {
	dir := t.TempDir()
	for args, want := range map[string]string{
		"up --run=exit\\ 1 host":                 "",
		"up --owner=1000 host":                   "owner and mode apply to mkdir directories",
		"up --mkdir=a --mode=9 host":             "octal permission mode",
		"up --wait-timeout=soon --run=true host": "a duration such as",
		"up --run=a --run=b host":                "given 2 times",
		"up --frob=1 host":                       `unknown option "frob"`,
	} {
		a := strings.Fields(strings.ReplaceAll(args, "\\ ", "\x00"))
		for i := range a {
			a[i] = strings.ReplaceAll(a[i], "\x00", " ")
		}
		code, msgs, _ := call(t, dir, a...)
		if code != 1 || len(msgs) == 0 || msgs[len(msgs)-1].Type != "error" || !strings.Contains(msgs[len(msgs)-1].Message, want) {
			t.Errorf("%s: %d %v, want an error with %q", args, code, msgs, want)
		}
	}
}

func TestDownAndStop(t *testing.T) {
	dir := t.TempDir()
	if code, msgs, _ := call(t, dir, "down", "--down=touch went-down", "host"); code != 0 {
		t.Fatal(msgs)
	}
	if code, msgs, _ := call(t, dir, "stop", "--stop=touch stopped", "host"); code != 0 {
		t.Fatal(msgs)
	}
	for _, f := range []string{"went-down", "stopped"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	// Nothing to do is fine.
	if code, msgs, _ := call(t, dir, "down", "host"); code != 0 || len(msgs) != 0 {
		t.Errorf("%d %v", code, msgs)
	}
}

func TestMain(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Main(nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "provider service") {
		t.Errorf("%d %q", code, errb.String())
	}
	if code := Main([]string{"version"}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "composehost ") {
		t.Errorf("%d %q", code, out.String())
	}
	errb.Reset()
	if code := Main([]string{"compose"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no command") {
		t.Errorf("%d %q", code, errb.String())
	}
}
