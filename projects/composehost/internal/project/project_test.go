package project

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func TestParseFlags(t *testing.T) {
	f, rest := ParseFlags([]string{"--progress", "plain", "-p", "up", "--dry-run", "-f", "a.yaml", "--file=b.yaml", "-fc.yaml",
		"--project-directory=/srv", "--env-file", "x.env", "up", "-d", "-f", "not-a-global"})
	want := Flags{Files: []string{"a.yaml", "b.yaml", "c.yaml"}, ProjectDirectory: "/srv", EnvFiles: []string{"x.env"}}
	if !reflect.DeepEqual(f, want) || !reflect.DeepEqual(rest, []string{"up", "-d", "-f", "not-a-global"}) {
		t.Fatalf("%+v %v", f, rest)
	}
	if _, rest := ParseFlags([]string{"-f"}); rest != nil {
		t.Fatal(rest)
	}
}

func TestDir(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	sub := filepath.Join(root, "a", "b")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("services: {}\n"), 0o644)
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	dir := func(f Flags, cwd string) string {
		t.Helper()
		d, err := Dir(f, getenv, cwd)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	// Like Compose: the nearest directory up with a compose file.
	if d := dir(Flags{}, sub); d != root {
		t.Fatal(d)
	}
	if d := dir(Flags{Files: []string{"../x.yaml", "y.yaml"}}, sub); d != filepath.Join(root, "a") {
		t.Fatal(d)
	}
	if d := dir(Flags{Files: []string{"/srv/app/compose.yaml"}, ProjectDirectory: "../.."}, sub); d != root {
		t.Fatal(d)
	}
	if d := dir(Flags{Files: []string{"-"}}, sub); d != sub {
		t.Fatal(d)
	}
	env["COMPOSE_FILE"] = "/srv/app/compose.yaml:/srv/app/override.yaml"
	if d := dir(Flags{}, sub); d != "/srv/app" {
		t.Fatal(d)
	}
	env["COMPOSE_PATH_SEPARATOR"] = ";"
	env["COMPOSE_FILE"] = "/srv/a:b/compose.yaml;/x/y.yaml"
	if d := dir(Flags{}, sub); d != "/srv/a:b" {
		t.Fatal(d)
	}
	delete(env, "COMPOSE_FILE")
	delete(env, "COMPOSE_PATH_SEPARATOR")

	// COMPOSE_FILE from the working directory's .env, or --env-file.
	os.WriteFile(filepath.Join(sub, ".env"), []byte("# x\nexport COMPOSE_FILE=\"/opt/stack/compose.yaml\"\n"), 0o644)
	if d := dir(Flags{}, sub); d != "/opt/stack" {
		t.Fatal(d)
	}
	os.WriteFile(filepath.Join(sub, "other.env"), []byte("COMPOSE_FILE=/opt/other/c.yaml\n"), 0o644)
	if d := dir(Flags{EnvFiles: []string{"other.env"}}, sub); d != "/opt/other" {
		t.Fatal(d)
	}
	os.Remove(filepath.Join(sub, ".env"))

	os.Remove(filepath.Join(root, "compose.yaml"))
	if _, err := Dir(Flags{}, getenv, sub); err == nil {
		t.Fatal("no compose file, no error")
	}
}

func TestFromParent(t *testing.T) {
	old := Proc
	defer func() { Proc = old }()
	Proc = t.TempDir()
	proj, _ := filepath.EvalSymlinks(t.TempDir())
	os.WriteFile(filepath.Join(proj, "compose.yaml"), nil, 0o644)
	pid := filepath.Join(Proc, strconv.Itoa(os.Getppid()))
	os.MkdirAll(pid, 0o755)
	os.Symlink("/", filepath.Join(pid, "cwd"))
	os.WriteFile(filepath.Join(pid, "environ"), []byte("HOME=/root\x00PATH=/bin\x00"), 0o644)

	cmdline := func(args ...string) {
		var b []byte
		for _, a := range args {
			b = append(append(b, a...), 0)
		}
		os.WriteFile(filepath.Join(pid, "cmdline"), b, 0o644)
	}
	cmdline("/usr/libexec/docker/cli-plugins/docker-compose", "compose", "--progress", "plain", "-f", proj+"/compose.yaml", "up", "-d")
	if d, err := FromParent(); err != nil || d != proj {
		t.Fatalf("%q %v", d, err)
	}
	cmdline("/usr/local/bin/docker-compose", "--project-directory", proj, "down")
	if d, err := FromParent(); err != nil || d != proj {
		t.Fatalf("%q %v", d, err)
	}
	cmdline("/usr/bin/bash", "-c", "composehost compose up x")
	if _, err := FromParent(); err == nil {
		t.Fatal("not Compose, no error")
	}
	cmdline("docker-compose", "--project-directory", "/does/not/exist", "up")
	if _, err := FromParent(); err == nil {
		t.Fatal("a missing directory, no error")
	}
}
