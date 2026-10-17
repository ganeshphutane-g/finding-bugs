package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/docker-edit/internal/engine"
)

// endpoint runs docker-edit with the given environment and returns the
// endpoint it would connect to (it never connects).
func endpoint(t *testing.T, vars map[string]string, args ...string) (engine.Endpoint, int, string) {
	t.Helper()
	var ep engine.Endpoint
	var out bytes.Buffer
	env := Env{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out,
		Getenv: func(k string) string {
			if k == "HOME" || k == "DOCKER_CONFIG" {
				if v, ok := vars[k]; ok {
					return v
				}
				return t.TempDir()
			}
			return vars[k]
		},
		NewClient: func(e engine.Endpoint) (*engine.Client, error) { ep = e; return nil, errors.New("not connecting") }}
	code := Run(context.Background(), env, args)
	return ep, code, out.String()
}

func TestDockerGlobalFlags(t *testing.T) {
	plugin := map[string]string{"DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND": "docker", "DOCKER_HOST": "tcp://10.0.0.1:2375"}
	if ep, _, out := endpoint(t, plugin, "--context", "default", "edit", "web", "-e", "A=1"); ep.Host != "tcp://10.0.0.1:2375" {
		t.Errorf("--context default with DOCKER_HOST: %+v %s", ep, out)
	}
	if ep, _, out := endpoint(t, plugin, "-H", "tcp://10.0.0.2:2376", "--tls=false", "edit", "web", "-e", "A=1"); ep.Host != "tcp://10.0.0.2:2376" || ep.TLS {
		t.Errorf("--tls=false: %+v %s", ep, out)
	}
	tlsEnv := map[string]string{"DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND": "docker", "DOCKER_TLS_VERIFY": "1"}
	if ep, _, out := endpoint(t, tlsEnv, "-H", "tcp://10.0.0.2:2376", "--tlsverify=false", "edit", "web", "-e", "A=1"); ep.TLSVerify {
		t.Errorf("--tlsverify=false: %+v %s", ep, out)
	}
	// An older docker CLI that does not say it called a plugin.
	if ep, _, out := endpoint(t, nil, "-H", "tcp://10.0.0.3:2375", "edit", "web", "-e", "A=1"); ep.Host != "tcp://10.0.0.3:2375" {
		t.Errorf("older docker CLI: %+v %s", ep, out)
	}
}

func TestStandaloneArguments(t *testing.T) {
	// docker-edit's own flags first, and a network named "edit".
	_, code, out := endpoint(t, nil, "--dry-run", "web", "--network", "edit")
	if code != 1 || !strings.Contains(out, "not connecting") {
		t.Errorf("exit %d: %s", code, out)
	}
	// A container named version.
	if _, code, out := endpoint(t, nil, "version", "--dry-run", "-e", "A=1"); code != 1 || !strings.Contains(out, "not connecting") {
		t.Errorf("container named version: exit %d: %s", code, out)
	}
	if _, code, out := endpoint(t, nil); code != 2 || !strings.Contains(out, "name the container") {
		t.Errorf("no arguments: exit %d: %s", code, out)
	}
}

func TestParse(t *testing.T) {
	o, err := parse([]string{"web", "--wait", "45", "-e", "A=1", "--env=B=2", "--restart=always"})
	if err != nil || o.wait != 45*time.Second || len(o.changes.Env) != 2 || o.changes.Restart != "always" || o.container != "web" {
		t.Fatalf("%+v %v", o, err)
	}
	if o, err := parse([]string{"web", "--wait", "2m"}); err != nil || o.wait != 2*time.Minute {
		t.Fatalf("--wait 2m: %v %v", o.wait, err)
	}
	if o, _ := parse([]string{"web"}); o.wait != 30*time.Second {
		t.Fatalf("default wait %v", o.wait)
	}
	for args, want := range map[string]string{
		"web --wait soon":                   "--wait",
		"web --image a:1 --image a:2":       "given twice",
		"web --restart always --restart no": "given twice",
		"web --bogus":                       "not defined: --bogus",
		"web --wait=x":                      "for flag --wait: expected a duration",
		"web other":                         "name one container",
		"web --wait -5s":                    "negative",
	} {
		_, err := parse(strings.Fields(args))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", args, err)
		}
	}
}

func TestPluginCall(t *testing.T) {
	for args, want := range map[string]bool{
		"edit web":                     true,
		"--context edit edit web":      true, // a context named edit
		"-H=tcp://x:1 --tls edit web":  true,
		"--dry-run web --network edit": false,
		"web --network edit":           false,
		"--context prod":               false,
	} {
		if got := pluginCall(strings.Fields(args)); got != want {
			t.Errorf("%s: %v", args, got)
		}
	}
}
