package project

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
)

func TestParseFlags(t *testing.T) {
	f, rest, err := ParseFlags([]string{"--progress", "plain", "-p", "up", "--dry-run", "-f", "a.yaml", "--file=b.yaml", "-fc.yaml",
		"--project-directory=/srv", "--env-file", "x.env", "up", "-d", "-f", "not-a-global"})
	want := Flags{Files: []string{"a.yaml", "b.yaml", "c.yaml"}, ProjectDirectory: "/srv", EnvFiles: []string{"x.env"}}
	if err != nil || !reflect.DeepEqual(f, want) || !reflect.DeepEqual(rest, []string{"up", "-d", "-f", "not-a-global"}) {
		t.Fatalf("%+v %v %v", f, rest, err)
	}
	// The Docker CLI's flags come first when Compose is its plugin.
	for _, args := range [][]string{
		{"--context", "default", "-D", "compose", "-f", "/p/c.yaml", "up"},
		{"-c", "x", "--log-level", "info", "-H", "unix:///s", "--tlsverify", "compose", "--file", "/p/c.yaml", "up"},
		{"--context=x", "-f", "/p/c.yaml", "up"}, // docker-compose on its own
		{"compose", "--workdir", "/p", "up"},
	} {
		f, rest, err := ParseFlags(args)
		if err != nil || rest[0] != "up" || (f.Files == nil && f.ProjectDirectory != "/p") || (f.Files != nil && f.Files[0] != "/p/c.yaml") {
			t.Errorf("%v: %+v %v %v", args, f, rest, err)
		}
	}
	// A flag it does not know could take a value: an error, not a guess.
	for _, args := range [][]string{{"-f"}, {"--frobnicate", "x", "-f", "c.yaml", "up"}, {"-x", "up"}} {
		if _, _, err := ParseFlags(args); err == nil {
			t.Errorf("%v: no error", args)
		}
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
	env["COMPOSE_ENV_FILES"] = "none.env,other.env"
	if d := dir(Flags{}, sub); d != "/opt/other" {
		t.Fatal(d)
	}
	delete(env, "COMPOSE_ENV_FILES")
	env["COMPOSE_DISABLE_ENV_FILE"] = "true"
	if d := dir(Flags{}, sub); d != root {
		t.Fatal(d)
	}
	delete(env, "COMPOSE_DISABLE_ENV_FILE")
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
	cmdline("/usr/libexec/docker/cli-plugins/docker-compose", "--context", "default", "compose", "-f", proj+"/compose.yaml", "up")
	if d, err := FromParent(); err != nil || d != proj {
		t.Fatalf("%q %v", d, err)
	}
	// Relative to the logical working directory ($PWD through a link), as
	// Compose sees it, not the physical one.
	base, _ := filepath.EvalSymlinks(t.TempDir())
	os.MkdirAll(filepath.Join(base, "real", "deep", "here"), 0o755)
	os.MkdirAll(filepath.Join(base, "logical", "proj"), 0o755)
	os.WriteFile(filepath.Join(base, "logical", "proj", "compose.yaml"), nil, 0o644)
	os.Symlink(filepath.Join(base, "real", "deep", "here"), filepath.Join(base, "logical", "here"))
	os.Remove(filepath.Join(pid, "cwd"))
	os.Symlink(filepath.Join(base, "real", "deep", "here"), filepath.Join(pid, "cwd"))
	os.WriteFile(filepath.Join(pid, "environ"), []byte("PWD="+filepath.Join(base, "logical", "here")+"\x00"), 0o644)
	cmdline("docker-compose", "-f", "../proj/compose.yaml", "up")
	if d, err := FromParent(); err != nil || d != filepath.Join(base, "logical", "proj") {
		t.Fatalf("%q %v", d, err)
	}
	// A $PWD that is not the working directory is ignored.
	os.WriteFile(filepath.Join(pid, "environ"), []byte("PWD=/\x00"), 0o644)
	if _, err := FromParent(); err == nil {
		t.Fatal("used a stale $PWD")
	}
	cmdline("docker-compose", "--frob", "x", "-f", proj+"/compose.yaml", "up")
	if _, err := FromParent(); err == nil {
		t.Fatal("an unknown flag, no error")
	}
	os.Remove(filepath.Join(pid, "cwd"))
	os.Symlink("/", filepath.Join(pid, "cwd"))
	cmdline("/usr/bin/bash", "-c", "composehost compose up x")
	if _, err := FromParent(); err == nil {
		t.Fatal("not Compose, no error")
	}
	cmdline("docker-compose", "--project-directory", "/does/not/exist", "up")
	if _, err := FromParent(); err == nil {
		t.Fatal("a missing directory, no error")
	}
}
