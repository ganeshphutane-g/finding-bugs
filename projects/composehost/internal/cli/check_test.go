package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/composehost/internal/steps"
)

// config is "docker compose config --format json" as Compose prints it
// (options as lists).
const config = `{
  "name": "demo",
  "services": {
    "app": {"image": "busybox", "depends_on": {"host": {"condition": "service_started", "required": true}}},
    "db": {"image": "postgres"},
    "host": {"provider": {"type": "composehost", "options": {
      "mkdir": ["./data", "./config"], "owner": ["1000:1000"], "mode": ["0750"],
      "wait-mount": ["/mnt/nas"], "wait-timeout": ["20s"],
      "run": ["./prepare.sh"], "down": ["./teardown.sh"]}}},
    "other": {"provider": {"type": "model", "options": {"model": ["ai/smollm2"]}}}
  }
}`

func check(t *testing.T, cfg string, cfgErr error, pending []string, args ...string) (int, string, string, []string) {
	t.Helper()
	old := ComposeConfig
	defer func() { ComposeConfig = old }()
	var gotFiles []string
	ComposeConfig = func(files []string) ([]byte, error) {
		gotFiles = files
		return []byte(cfg), cfgErr
	}
	var out, errb bytes.Buffer
	code := Check(args, &out, &errb, func(dir string, w steps.Waits) ([]string, error) {
		return pending, nil
	})
	return code, out.String(), errb.String(), gotFiles
}

func TestCheck(t *testing.T) {
	code, out, errs, files := check(t, config, nil, nil, "-f", "a.yaml", "--file=b.yaml")
	if code != 0 || errs != "" {
		t.Fatalf("code %d: %s%s", code, out, errs)
	}
	if !reflect.DeepEqual(files, []string{"a.yaml", "b.yaml"}) {
		t.Fatalf("files %v", files)
	}
	want := `host:
  before:    app
  on up:     mkdir ./data ./config (owner 1000:1000, mode 0750)
  on up:     wait until /mnt/nas mounted (at most 20s)
  on up:     run ./prepare.sh
  now:       the waits are met
  on down:   ./teardown.sh
`
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

func TestCheckPending(t *testing.T) {
	code, out, _, _ := check(t, config, nil, []string{"/mnt/nas to be mounted"})
	if code != 3 || !strings.Contains(out, "now:       up would wait for /mnt/nas to be mounted") {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestCheckErrors(t *testing.T) {
	typo := strings.Replace(config, `"mode"`, `"mdoe"`, 1)
	code, out, _, _ := check(t, typo, nil, nil)
	if code != 1 || !strings.Contains(out, `unknown option "mdoe" (Compose would drop it silently)`) {
		t.Fatalf("code %d: %s", code, out)
	}
	// A bad value is an error, and the other commands are still shown.
	bad := strings.Replace(config, `"20s"`, `"soon"`, 1)
	code, out, _, _ = check(t, bad, nil, nil)
	if code != 1 || !strings.Contains(out, "error:     up: ") || !strings.Contains(out, "on down:") {
		t.Fatalf("code %d: %s", code, out)
	}
	// An error beats waits that are not met.
	code, _, _, _ = check(t, typo, nil, []string{"x"})
	if code != 1 {
		t.Fatalf("code %d", code)
	}
}

func TestCheckNothing(t *testing.T) {
	code, out, _, _ := check(t, `{"name":"demo","services":{"db":{"image":"postgres"}}}`, nil, nil)
	if code != 1 || !strings.Contains(out, "no service has provider type composehost") {
		t.Fatalf("code %d: %s", code, out)
	}
	code, _, errs, _ := check(t, "", errors.New("no configuration file provided"), nil)
	if code != 1 || !strings.Contains(errs, "no configuration file provided") {
		t.Fatalf("code %d: %s", code, errs)
	}
	code, _, errs, _ = check(t, config, nil, nil, "up")
	if code != 2 || !strings.Contains(errs, `unexpected "up"`) {
		t.Fatalf("code %d: %s", code, errs)
	}
}

func TestCheckNoDependents(t *testing.T) {
	alone := strings.Replace(config, `"depends_on": {"host": {"condition": "service_started", "required": true}}`, `"depends_on": {"db": {}}`, 1)
	_, out, _, _ := check(t, alone, nil, nil)
	if !strings.Contains(out, "note:      no service depends on host") {
		t.Fatal(out)
	}
}

func TestIsComposehost(t *testing.T) {
	for typ, want := range map[string]bool{
		"composehost": true, "/usr/local/bin/composehost": true, "docker-composehost": true,
		"model": false, "composehostx": false,
	} {
		if isComposehost(typ) != want {
			t.Errorf("%s: %v", typ, !want)
		}
	}
}

func TestProjectDir(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("services: {}\n"), 0o644)
	old, _ := os.Getwd()
	defer os.Chdir(old)
	os.Chdir(sub)
	t.Setenv("COMPOSE_FILE", "")

	// Like Compose: the nearest directory up with a compose file.
	if d, err := projectDir(nil); err != nil || d != root {
		t.Fatalf("%q %v", d, err)
	}
	if d, _ := projectDir([]string{"../x.yaml", "y.yaml"}); d != filepath.Join(root, "a") {
		t.Fatalf("%q", d)
	}
	t.Setenv("COMPOSE_FILE", "/srv/app/compose.yaml:/srv/app/override.yaml")
	if d, _ := projectDir(nil); d != "/srv/app" {
		t.Fatalf("%q", d)
	}
	os.Remove(filepath.Join(root, "compose.yaml"))
	t.Setenv("COMPOSE_FILE", "")
	if d, _ := projectDir(nil); d != sub {
		t.Fatalf("%q", d)
	}
}
