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
