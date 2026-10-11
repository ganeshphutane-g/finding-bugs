package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func testEnv(vars map[string]string) (Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	env := DefaultEnv()
	env.Stdin = strings.NewReader("")
	env.Stdout = &out
	env.Stderr = &errOut
	env.Getenv = func(k string) string { return vars[k] }
	return env, &out, &errOut
}

func TestMetadata(t *testing.T) {
	env, out, _ := testEnv(nil)
	if code := Run(context.Background(), env, []string{metadataCommand}); code != 0 {
		t.Fatalf("exit code %d", code)
	}
	var meta map[string]string
	if err := json.Unmarshal(out.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["SchemaVersion"] != "0.1.0" || meta["Vendor"] == "" || meta["ShortDescription"] == "" {
		t.Fatalf("bad metadata: %v", meta)
	}
}

func TestHelpWithAndWithoutPluginName(t *testing.T) {
	for _, args := range [][]string{nil, {"mop"}, {"help"}, {"mop", "--help"}} {
		env, out, _ := testEnv(nil)
		if code := Run(context.Background(), env, args); code != 0 {
			t.Fatalf("%v: exit code %d", args, code)
		}
		if !strings.Contains(out.String(), "Usage: docker mop") {
			t.Fatalf("%v: no usage in %q", args, out.String())
		}
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	env, _, errOut := testEnv(nil)
	if code := Run(context.Background(), env, []string{"mop", "frobnicate"}); code != 2 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "frobnicate"`) {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestVersionWithoutDaemon(t *testing.T) {
	env, out, errOut := testEnv(map[string]string{"DOCKER_HOST": "unix:///nonexistent/docker.sock"})
	if code := Run(context.Background(), env, []string{"version"}); code != 1 {
		t.Fatalf("exit code %d", code)
	}
	if !strings.Contains(out.String(), "docker-mop:") {
		t.Fatalf("stdout = %q", out.String())
	}
	if !strings.Contains(errOut.String(), "cannot connect") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestParsePruneArgs(t *testing.T) {
	pf, help, err := parsePruneArgs("plan", []string{"--filter", "until=24h", "images", "-a", "--format", "json"}, false)
	if err != nil || help {
		t.Fatalf("err=%v help=%v", err, help)
	}
	if pf.kind != "images" || !pf.all || pf.format != "json" || len(pf.filters) != 1 {
		t.Fatalf("got %+v", pf)
	}

	pf, _, err = parsePruneArgs("plan", nil, false)
	if err != nil || pf.kind != "system" || pf.format != "table" {
		t.Fatalf("defaults: %+v %v", pf, err)
	}

	for _, bad := range [][]string{
		{"images", "volumes"},
		{"builder"},
		{"--format", "yaml"},
		{"images", "--volumes"},
		{"--nope"},
	} {
		if _, _, err := parsePruneArgs("plan", bad, false); err == nil {
			t.Errorf("%v: expected an error", bad)
		}
	}

	pf, _, err = parsePruneArgs("prune", []string{"volumes", "-y"}, true)
	if err != nil || !pf.yes {
		t.Fatalf("prune -y: %+v %v", pf, err)
	}
	if _, _, err := parsePruneArgs("plan", []string{"-y"}, false); err == nil {
		t.Fatal("plan must not accept -y")
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{0: "0B", 999: "999B", 4096: "4.096kB", 209527: "209.5kB", 1_500_000_000: "1.5GB", -1: "unknown"}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("until=24h"); got != "until=24h" {
		t.Errorf("got %q", got)
	}
	if got := shellQuote("label=a b"); got != "'label=a b'" {
		t.Errorf("got %q", got)
	}
	if got := shellQuote("label!=x"); got != "'label!=x'" {
		t.Errorf("got %q", got)
	}
}
