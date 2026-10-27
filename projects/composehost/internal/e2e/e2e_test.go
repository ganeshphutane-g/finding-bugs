// End-to-end tests with real dockerd and Docker Compose (v2.36.1 or later).
// They run as root when COMPOSEHOST_INTEGRATION=1: they mount a tmpfs and
// chown directories. COMPOSEHOST_BIN is the binary to test (else it is
// built); COMPOSEHOST_IMAGE is the image for the dependent services
// (busybox:latest).
package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

var image = "busybox:latest"

func TestMain(m *testing.M) {
	if os.Getenv("COMPOSEHOST_INTEGRATION") != "1" {
		fmt.Println("skipping: set COMPOSEHOST_INTEGRATION=1 (root, dockerd, docker compose)")
		os.Exit(0)
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "the end-to-end tests need root")
		os.Exit(1)
	}
	if v := os.Getenv("COMPOSEHOST_IMAGE"); v != "" {
		image = v
	}
	// Compose finds the provider by name on PATH.
	bin, err := os.MkdirTemp("", "composehost-bin")
	if err != nil {
		panic(err)
	}
	if b := os.Getenv("COMPOSEHOST_BIN"); b != "" {
		abs, _ := filepath.Abs(b)
		err = os.Symlink(abs, filepath.Join(bin, "composehost"))
	} else {
		_, file, _, _ := runtime.Caller(0)
		cmd := exec.Command("go", "build", "-o", filepath.Join(bin, "composehost"), ".")
		cmd.Dir = filepath.Join(filepath.Dir(file), "..", "..")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err = cmd.Run()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "composehost binary:", err)
		os.Exit(1)
	}
	os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if exec.Command("docker", "image", "inspect", image).Run() == nil {
		// Already here: no pull (and no registry rate limit).
	} else if out, err := exec.Command("docker", "pull", "-q", image).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "docker pull %s: %v: %s", image, err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(bin)
	os.Exit(code)
}

type project struct {
	t    *testing.T
	dir  string
	name string
}

// newProject writes compose.yaml (IMAGE replaced) into a new directory.
func newProject(t *testing.T, yaml string) *project {
	t.Helper()
	dir := t.TempDir()
	// The containers' user (1000) must reach the bind mounts.
	os.Chmod(dir, 0o755)
	p := &project{t, dir, fmt.Sprintf("chtest%d", time.Now().UnixNano()%1e9)}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(strings.ReplaceAll(yaml, "IMAGE", image)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.compose("down", "-t", "0", "--remove-orphans") })
	return p
}

// compose runs docker compose in the project directory; the output is
// stdout and stderr, with the provider's messages (plain progress).
func (p *project) compose(args ...string) (string, error) {
	p.t.Helper()
	// With -p, -f too: without the file, stop and down skip providers.
	cmd := exec.Command("docker", append([]string{"compose", "--progress", "plain", "-p", p.name, "-f", "compose.yaml"}, args...)...)
	cmd.Dir = p.dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	p.t.Logf("docker compose %s:\n%s", strings.Join(args, " "), out.String())
	return out.String(), err
}

func (p *project) up(args ...string) string {
	p.t.Helper()
	out, err := p.compose(append([]string{"up", "-d"}, args...)...)
	if err != nil {
		p.t.Fatalf("up: %v", err)
	}
	return out
}

// logs waits for a service's container to exit and returns its output.
func (p *project) logs(service string) string {
	p.t.Helper()
	if out, err := p.compose("wait", service); err != nil {
		p.t.Fatalf("wait %s: %v: %s", service, err, out)
	}
	cmd := exec.Command("docker", "compose", "-p", p.name, "logs", "--no-log-prefix", service)
	cmd.Dir = p.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		p.t.Fatalf("logs %s: %v: %s", service, err, out)
	}
	return string(out)
}

func (p *project) exists(name string) bool {
	_, err := os.Stat(filepath.Join(p.dir, name))
	return err == nil
}

func mustContain(t *testing.T, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("no %q in:\n%s", sub, s)
		}
	}
}

// Directories with the owner and mode, the run command and the env values
// are all there before the dependent container starts.
func TestUp(t *testing.T) {
	p := newProject(t, `
services:
  host:
    provider:
      type: composehost
      options:
        mkdir: [./data, ./config/sub]
        owner: "1000:1000"
        mode: "0750"
        run: "echo preparing; date > ./data/prepared"
        env: "echo GREETING=hello; echo 'export TOKEN=\"abc def\"'"
  app:
    image: IMAGE
    user: "1000:1000"
    command: ["sh", "-c", "stat -c '%n %u:%g %a' /data /config/sub; cat /data/prepared >/dev/null && echo prepared; echo \"GREETING=$$GREETING TOKEN=$$TOKEN\""]
    volumes: [./data:/data, ./config:/config]
    depends_on: [host]
`)
	out := p.up()
	mustContain(t, out, "host created ./data", "host ./data owned by 1000:1000", "host run: echo preparing", "host env: 2 values")
	logs := p.logs("app")
	mustContain(t, logs, "/data 1000:1000 750", "/config/sub 1000:1000 750", "prepared", "GREETING=hello TOKEN=abc def")
	// The parent of a nested directory gets the usual mode, not the
	// directory's own.
	if st, err := os.Stat(filepath.Join(p.dir, "config")); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("config: %v %v", st.Mode(), err)
	}
}

// A second up changes nothing that is already right, and repairs what is
// not.
func TestUpAgain(t *testing.T) {
	p := newProject(t, `
services:
  host:
    provider:
      type: composehost
      options:
        mkdir: ./data
        mode: "0700"
  app:
    image: IMAGE
    command: ["sleep", "300"]
    depends_on: [host]
`)
	p.up()
	out := p.up()
	if strings.Contains(out, "host created") || strings.Contains(out, "mode 0700") {
		t.Fatalf("second up did something:\n%s", out)
	}
	mustContain(t, out, "Container "+p.name+"-app-1 Running")
	os.Chmod(filepath.Join(p.dir, "data"), 0o777)
	mustContain(t, p.up(), "host ./data mode 0700")
}

// The dependent waits until the mount appears; it sees the mounted
// filesystem, not the empty directory beneath.
func TestWaitMount(t *testing.T) {
	p := newProject(t, `
services:
  host:
    provider:
      type: composehost
      options:
        mkdir: ./nas
        wait-mount: ./nas
        wait-timeout: 60s
  app:
    image: IMAGE
    command: ["cat", "/nas/hello"]
    volumes: [./nas:/nas]
    depends_on: [host]
`)
	done := make(chan string, 1)
	start := time.Now()
	go func() {
		out, err := p.compose("up", "-d")
		if err != nil {
			out += "\nup failed: " + err.Error()
		}
		done <- out
	}()
	nas := filepath.Join(p.dir, "nas")
	time.Sleep(3 * time.Second)
	select {
	case out := <-done:
		t.Fatalf("up did not wait:\n%s", out)
	default:
	}
	if err := syscall.Mount("tmpfs", nas, "tmpfs", 0, "size=1m"); err != nil {
		t.Fatal(err)
	}
	defer syscall.Unmount(nas, syscall.MNT_DETACH)
	os.WriteFile(filepath.Join(nas, "hello"), []byte("from the mount\n"), 0o644)
	var out string
	select {
	case out = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("up still waiting after the mount")
	}
	mustContain(t, out, "host waiting for ./nas to be mounted", "Started")
	if time.Since(start) < 3*time.Second {
		t.Fatal("up was too quick")
	}
	mustContain(t, p.logs("app"), "from the mount")
}

// The waits give up after wait-timeout, and up fails before the dependent
// is created.
func TestWaitTimeout(t *testing.T) {
	p := newProject(t, `
services:
  host:
    provider:
      type: composehost
      options:
        wait-path: ./never
        wait-tcp: 127.0.0.1:1
        wait-timeout: 2s
  app:
    image: IMAGE
    command: ["true"]
    depends_on: [host]
`)
	out, err := p.compose("up", "-d")
	if err == nil {
		t.Fatal("up succeeded")
	}
	mustContain(t, out, "gave up after 2s waiting for ./never to exist, 127.0.0.1:1 to accept connections")
	if strings.Contains(out, "app-1 Created") {
		t.Fatal("the dependent was created")
	}
}

// A failing run command fails up with its error output.
func TestRunFails(t *testing.T) {
	p := newProject(t, `
services:
  host:
    provider:
      type: composehost
      options:
        run: "echo the NAS is asleep >&2; exit 3"
  app:
    image: IMAGE
    command: ["true"]
    depends_on: [host]
`)
	out, err := p.compose("up", "-d")
	if err == nil {
		t.Fatal("up succeeded")
	}
	mustContain(t, out, "exit status 3: the NAS is asleep")
}

// stop and down run their commands. Compose's limit: with -p NAME and no
// -f, stop and down work from the containers alone and skip providers.
func TestStopAndDown(t *testing.T) {
	p := newProject(t, `
services:
  host:
    provider:
      type: composehost
      options:
        run: "rm -f stopped downed"
        stop: "touch stopped"
        down: "touch downed"
  app:
    image: IMAGE
    command: ["sleep", "300"]
    depends_on: [host]
`)
	p.up()
	if _, err := p.compose("stop", "-t", "0"); err != nil {
		t.Fatal(err)
	}
	if !p.exists("stopped") {
		t.Fatal("stop did not run")
	}
	if _, err := p.compose("down", "-t", "0"); err != nil {
		t.Fatal(err)
	}
	if !p.exists("downed") {
		t.Fatal("down did not run")
	}

	p.up()
	if p.exists("downed") {
		t.Fatal("run did not run")
	}
	for _, c := range []string{"stop", "down"} {
		cmd := exec.Command("docker", "compose", "-p", p.name, c, "-t", "0")
		cmd.Dir = p.dir // the file is here, but -p without -f ignores it
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	if p.exists("stopped") || p.exists("downed") {
		t.Fatal("-p without -f called the provider: the documented limit has changed")
	}
}

// check shows the steps and looks at the waits once.
func TestCheck(t *testing.T) {
	p := newProject(t, `
services:
  host:
    provider:
      type: composehost
      options:
        mkdir: ./data
        wait-path: ./ready
        wait-timeout: 5s
        mdoe: "0750"
  app:
    image: IMAGE
    depends_on: [host]
`)
	sub := filepath.Join(p.dir, "sub")
	os.Mkdir(sub, 0o755)
	run := func() (int, string) {
		cmd := exec.Command("composehost", "check")
		cmd.Dir = sub // like Compose, it finds the file further up
		out, err := cmd.CombinedOutput()
		code := 0
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return code, string(out)
	}
	code, out := run()
	if code != 1 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	mustContain(t, out, `unknown option "mdoe"`, "on up:     mkdir ./data", "up would wait for ./ready to exist")
	if p.exists("data") {
		t.Fatal("check created a directory")
	}

	os.WriteFile(filepath.Join(p.dir, "compose.yaml"), bytes.Replace(must(os.ReadFile(filepath.Join(p.dir, "compose.yaml"))), []byte("mdoe"), []byte("mode"), 1), 0o644)
	if code, out = run(); code != 3 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	os.WriteFile(filepath.Join(p.dir, "ready"), nil, 0o644)
	if code, out = run(); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	mustContain(t, out, "the waits are met")
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
