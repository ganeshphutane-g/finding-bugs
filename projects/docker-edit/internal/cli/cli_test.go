package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/docker-edit/internal/engine"
)

func integration(t *testing.T) {
	t.Helper()
	if os.Getenv("DOCKEREDIT_INTEGRATION") == "" {
		t.Skip("set DOCKEREDIT_INTEGRATION=1 to run tests against the Docker daemon")
	}
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func inspect(t *testing.T, name, format string) string {
	t.Helper()
	return docker(t, "inspect", "--format", format, name)
}

// build makes a small local image (no pulls).
func build(t *testing.T, tag, dockerfile string) {
	t.Helper()
	cmd := exec.Command("docker", "build", "-q", "-t", tag, "-")
	cmd.Stdin = strings.NewReader(dockerfile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building %s: %v\n%s", tag, err, out)
	}
	t.Cleanup(func() { exec.Command("docker", "rmi", "-f", tag).Run() })
}

func name(t *testing.T, base string) string {
	n := fmt.Sprintf("de-%s-%d", base, time.Now().UnixNano()%1e9)
	t.Cleanup(func() {
		out, _ := exec.Command("docker", "ps", "-aq", "--filter", "name="+n).Output()
		for _, id := range strings.Fields(string(out)) {
			exec.Command("docker", "rm", "-f", id).Run()
		}
	})
	return n
}

// de runs docker-edit and returns the exit code and output.
func de(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	env := DefaultEnv()
	env.Stdin, env.Stdout, env.Stderr = strings.NewReader(""), &out, &out
	code := Run(context.Background(), env, args)
	return code, out.String()
}

const base = "FROM alpine:3\nENV APP_VERSION=%d\nLABEL app.version=%d\nCMD [\"sleep\", \"3600\"]\n"

func TestEdits(t *testing.T) {
	integration(t)
	build(t, "de-app:1", fmt.Sprintf(base, 1, 1))
	build(t, "de-app:2", fmt.Sprintf(base, 2, 2))
	n := name(t, "edits")
	vol := name(t, "vol")
	docker(t, "run", "-d", "--name", n, "-e", "MINE=yes", "-e", "DROP=me", "-l", "keep=1", "-l", "drop=1", "-p", "18100:80", "de-app:1")
	code, out := de(t, n, "-y", "--publish", "127.0.0.1:18101:81", "--unpublish", "80", "--env", "MINE=changed", "--unset-env", "DROP",
		"--label", "new=2", "--unlabel", "drop", "--volume", vol+":/data", "--restart", "on-failure:3", "--image", "de-app:2")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"~ image: de-app:1 -> de-app:2", "~ env APP_VERSION: 1 -> 2", "~ env MINE: yes -> changed",
		"- env DROP", "+ mount /data: " + vol, "~ restart: no -> on-failure:3", "- port 80/tcp", "+ port 81/tcp: 127.0.0.1:18101", "Done:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	env := inspect(t, n, "{{json .Config.Env}}")
	if !strings.Contains(env, `"MINE=changed"`) || strings.Contains(env, "DROP") || !strings.Contains(env, `"APP_VERSION=2"`) {
		t.Errorf("env %s", env)
	}
	labels := inspect(t, n, "{{json .Config.Labels}}")
	if !strings.Contains(labels, `"app.version":"2"`) || !strings.Contains(labels, `"keep":"1"`) || strings.Contains(labels, `"drop"`) {
		t.Errorf("labels %s", labels)
	}
	if ports := inspect(t, n, "{{json .HostConfig.PortBindings}}"); strings.Contains(ports, "80/tcp") || !strings.Contains(ports, "18101") {
		t.Errorf("ports %s", ports)
	}
	if r := inspect(t, n, "{{.HostConfig.RestartPolicy.Name}}:{{.HostConfig.RestartPolicy.MaximumRetryCount}} {{.State.Running}}"); r != "on-failure:3 true" {
		t.Errorf("restart/running %s", r)
	}
	if code, out := de(t, n, "--unpublish", "9999"); code != 2 || !strings.Contains(out, "does not publish 9999/tcp") {
		t.Errorf("unpublishing what is not published: exit %d %s", code, out)
	}
	if code, out := de(t, n, "-y"); code != 0 || !strings.Contains(out, "Nothing to change.") {
		t.Errorf("no changes: exit %d %s", code, out)
	}
}

func TestRestartChangesInPlace(t *testing.T) {
	integration(t)
	build(t, "de-app:1", fmt.Sprintf(base, 1, 1))
	n := name(t, "inplace")
	id := docker(t, "run", "-d", "--name", n, "de-app:1")
	code, out := de(t, n, "-y", "--restart", "always")
	if code != 0 || !strings.Contains(out, "changes in place") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := inspect(t, n, "{{.Id}} {{.HostConfig.RestartPolicy.Name}}"); got != id+" always" {
		t.Fatalf("after: %s", got)
	}
}

func TestRefusals(t *testing.T) {
	integration(t)
	build(t, "de-app:1", fmt.Sprintf(base, 1, 1))
	compose := name(t, "compose")
	docker(t, "run", "-d", "--name", compose, "-l", "com.docker.compose.project=shop", "de-app:1")
	if code, out := de(t, compose, "-y", "--env", "A=1"); code != 1 || !strings.Contains(out, `Compose project "shop"`) {
		t.Errorf("compose: exit %d %s", code, out)
	}
	rm := name(t, "rm")
	docker(t, "run", "-d", "--rm", "--name", rm, "de-app:1")
	if code, out := de(t, rm, "-y", "--env", "A=1"); code != 1 || !strings.Contains(out, "--rm") {
		t.Errorf("--rm: exit %d %s", code, out)
	}
	host := name(t, "nethost")
	docker(t, "run", "-d", "--name", host, "de-app:1")
	side := name(t, "sidecar")
	docker(t, "run", "-d", "--name", side, "--network", "container:"+host, "de-app:1")
	if code, out := de(t, host, "-y", "--env", "A=1"); code != 1 || !strings.Contains(out, side+" (--network container:") {
		t.Errorf("shared network: exit %d %s", code, out)
	}
	linked := name(t, "linked")
	docker(t, "run", "-d", "--name", linked, "de-app:1")
	linker := name(t, "linker")
	docker(t, "run", "-d", "--name", linker, "--link", linked+":backend", "de-app:1")
	if code, out := de(t, linked, "-y", "--env", "A=1"); code != 1 || !strings.Contains(out, linker+" (--link)") {
		t.Errorf("linked: exit %d %s", code, out)
	}
	dirty := name(t, "dirty")
	id := docker(t, "run", "-d", "--name", dirty, "de-app:1")
	docker(t, "exec", dirty, "sh", "-c", "echo data > /etc/important.conf; mkdir -p /run/x; touch /run/x/pid /tmp/scratch")
	code, out := de(t, dirty, "-y", "--env", "A=1")
	if code != 1 || !strings.Contains(out, "/etc/important.conf") || strings.Contains(out, "/run/x") || strings.Contains(out, "/tmp/scratch") {
		t.Errorf("changed files: exit %d %s", code, out)
	}
	if got := inspect(t, dirty, "{{.Id}}"); got != id {
		t.Error("the container was changed although it was refused")
	}
	if code, out := de(t, dirty, "-y", "--env", "A=1", "--discard-changes"); code != 0 {
		t.Errorf("--discard-changes: exit %d %s", code, out)
	}
}

func TestBrokenNewContainerIsRolledBack(t *testing.T) {
	integration(t)
	build(t, "de-app:1", fmt.Sprintf(base, 1, 1))
	build(t, "de-bad:1", "FROM alpine:3\nCMD [\"sh\", \"-c\", \"echo cannot start; exit 3\"]\n")
	n := name(t, "broken")
	id := docker(t, "run", "-d", "--name", n, "de-app:1")
	code, out := de(t, n, "-y", "--image", "de-bad:1", "--wait", "5s")
	if code != 1 || !strings.Contains(out, "exit code 3") || !strings.Contains(out, "cannot start") || !strings.Contains(out, "put the old container back") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if got := inspect(t, n, "{{.Id}} {{.State.Running}}"); got != id+" true" {
		t.Fatalf("after: %s", got)
	}
	// A healthcheck that never passes counts as a failure too.
	build(t, "de-sick:1", "FROM alpine:3\nHEALTHCHECK --interval=1s --retries=1 CMD false\nCMD [\"sleep\", \"3600\"]\n")
	if code, out := de(t, n, "-y", "--image", "de-sick:1", "--wait", "20s"); code != 1 || !strings.Contains(out, "unhealthy") {
		t.Fatalf("unhealthy: exit %d:\n%s", code, out)
	}
	if got := inspect(t, n, "{{.Id}} {{.State.Running}}"); got != id+" true" {
		t.Fatalf("after unhealthy: %s", got)
	}
}

func TestMovedTagKeepsTheImage(t *testing.T) {
	integration(t)
	build(t, "de-moving:1", fmt.Sprintf(base, 1, 1))
	n := name(t, "moved")
	docker(t, "run", "-d", "--name", n, "de-moving:1")
	old := inspect(t, n, "{{.Image}}")
	build(t, "de-moving:1", fmt.Sprintf(base, 9, 9)) // the tag now names another image
	code, out := de(t, n, "-y", "--env", "A=1")
	// The classic image store keeps the old image, and docker edit keeps
	// using it; the containerd store removes it, and docker edit refuses
	// rather than switch images silently.
	if _, err := exec.Command("docker", "image", "inspect", old).CombinedOutput(); err == nil {
		if code != 0 || !strings.Contains(out, "now names a different image") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if got := inspect(t, n, "{{.Image}}"); got != old {
			t.Fatalf("the image changed to %s", got)
		}
	} else if code != 1 || !strings.Contains(out, "no longer here") {
		t.Fatalf("old image gone: exit %d:\n%s", code, out)
	}
	// Moving on deliberately works either way. (The plan shows the image
	// IDs; with the classic store the container already runs the old image
	// by ID.)
	if code, out := de(t, n, "-y", "--image", "de-moving:1"); code != 0 || !strings.Contains(out, "~ env APP_VERSION: 1 -> 9") ||
		!regexp.MustCompile(`~ image: (de-moving:1 \(\w{12}\)|\w{12}) -> de-moving:1`).MatchString(out) {
		t.Fatalf("--image: exit %d:\n%s", code, out)
	}
}

func TestPluginMetadata(t *testing.T) {
	code, out := de(t, "docker-cli-plugin-metadata")
	if code != 0 || !strings.Contains(out, `"SchemaVersion":"0.1.0"`) {
		t.Fatalf("exit %d %s", code, out)
	}
	_ = engine.Doc{}
}
