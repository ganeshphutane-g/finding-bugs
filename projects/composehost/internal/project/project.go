// Package project finds a Compose project's directory, by Compose's own
// rules. Compose runs providers in its caller's working directory, not the
// project's, and does not say which directory that is: on Linux, the
// provider reads the arguments and environment of the Compose process that
// started it.
package project

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Files are the names Compose looks for, in its order.
var Files = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

// valueFlags are the global flags that take a value: Compose's, and the
// Docker CLI's, which come before "compose" when Compose runs as its plugin
// (docker --context X compose ...).
var valueFlags = map[string]bool{
	"-f": true, "--file": true, "-p": true, "--project-name": true,
	"--project-directory": true, "--workdir": true, "--env-file": true,
	"--profile": true, "--ansi": true, "--progress": true, "--parallel": true,
	"-c": true, "--context": true, "-H": true, "--host": true, "--config": true,
	"-l": true, "--log-level": true, "--tlscacert": true, "--tlscert": true,
	"--tlskey": true,
}

// boolFlags are the global flags without a value.
var boolFlags = map[string]bool{
	"--all-resources": true, "--compatibility": true, "--dry-run": true,
	"--verbose": true, "--no-ansi": true, "-D": true, "--debug": true,
	"--tls": true, "--tlsverify": true,
}

// Flags are the global flags that decide the directory.
type Flags struct {
	Files            []string
	ProjectDirectory string
	EnvFiles         []string
}

// ParseFlags reads the global flags, up to the subcommand (or the end),
// passing over a "compose" between the Docker CLI's flags and Compose's. A
// flag it does not know is an error: guessing whether it takes a value
// could mean missing -f, and the wrong directory.
func ParseFlags(args []string) (Flags, []string, error) {
	var f Flags
	sawCompose := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "compose" && !sawCompose {
			sawCompose = true
			continue
		}
		if a == "--" || !strings.HasPrefix(a, "-") || a == "-" {
			return f, args[i:], nil
		}
		name, value, hasValue := strings.Cut(a, "=")
		if !hasValue && len(a) > 2 && a[1] != '-' && valueFlags[a[:2]] {
			name, value, hasValue = a[:2], a[2:], true // -fFILE
		}
		if boolFlags[name] {
			continue
		}
		if !valueFlags[name] {
			return f, nil, fmt.Errorf("unknown flag %q", name)
		}
		if !hasValue {
			if i+1 >= len(args) {
				return f, nil, fmt.Errorf("flag %s needs a value", name)
			}
			i++
			value = args[i]
		}
		switch name {
		case "-f", "--file":
			f.Files = append(f.Files, value)
		case "--project-directory", "--workdir":
			f.ProjectDirectory = value
		case "--env-file":
			f.EnvFiles = append(f.EnvFiles, value)
		}
	}
	return f, nil, nil
}

// Dir is the project directory for these flags, environment and working
// directory:
//   - --project-directory;
//   - else the first compose file's directory (-f, else COMPOSE_FILE, from
//     the environment or the .env file);
//   - else the nearest directory, from cwd up, with a compose file.
func Dir(f Flags, getenv func(string) string, cwd string) (string, error) {
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(cwd, p)
	}
	if f.ProjectDirectory != "" {
		return abs(f.ProjectDirectory), nil
	}
	files := f.Files
	if len(files) == 0 {
		cf := getenv("COMPOSE_FILE")
		sep := getenv("COMPOSE_PATH_SEPARATOR")
		if cf == "" {
			// Compose also reads these from the .env file: the
			// --env-file ones, else COMPOSE_ENV_FILES, else the working
			// directory's, unless COMPOSE_DISABLE_ENV_FILE.
			envFiles := f.EnvFiles
			if len(envFiles) == 0 && getenv("COMPOSE_ENV_FILES") != "" {
				envFiles = strings.Split(getenv("COMPOSE_ENV_FILES"), ",")
			}
			if len(envFiles) == 0 && !truthy(getenv("COMPOSE_DISABLE_ENV_FILE")) {
				envFiles = []string{".env"}
			}
			for _, ef := range envFiles {
				vars := dotenv(abs(ef))
				if v := vars["COMPOSE_FILE"]; v != "" {
					cf = v
				}
				if v := vars["COMPOSE_PATH_SEPARATOR"]; v != "" && sep == "" {
					sep = v
				}
			}
		}
		if cf != "" {
			if sep == "" {
				sep = string(os.PathListSeparator)
			}
			files = strings.Split(cf, sep)
		}
	}
	if len(files) > 0 {
		if files[0] == "-" {
			return cwd, nil
		}
		return filepath.Dir(abs(files[0])), nil
	}
	for d := cwd; ; d = filepath.Dir(d) {
		for _, n := range Files {
			if _, err := os.Stat(filepath.Join(d, n)); err == nil {
				return d, nil
			}
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("no compose file in %s or above", cwd)
		}
	}
}

func truthy(s string) bool {
	b, err := strconv.ParseBool(s)
	return err == nil && b
}

// dotenv reads KEY=VALUE lines from a .env file (missing: none).
func dotenv(path string) map[string]string {
	vars := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return vars
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		vars[strings.TrimSpace(k)] = v
	}
	return vars
}

func sameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && os.SameFile(sa, sb)
}

// Proc is /proc (a variable for tests).
var Proc = "/proc"

// FromParent finds the directory of the project that the Compose process
// which started this one is working on.
func FromParent() (string, error) {
	pid := strconv.Itoa(os.Getppid())
	cmdline, err := os.ReadFile(filepath.Join(Proc, pid, "cmdline"))
	if err != nil {
		return "", fmt.Errorf("reading the Compose process's arguments: %w", err)
	}
	args := strings.Split(strings.TrimRight(string(cmdline), "\x00"), "\x00")
	// docker-compose as a Docker CLI plugin ("docker-compose compose ...")
	// or on its own ("docker-compose ...").
	if len(args) == 0 || !strings.Contains(filepath.Base(args[0]), "compose") {
		return "", fmt.Errorf("composehost was not started by Docker Compose (but by %q)", args[0])
	}
	args = args[1:]
	cwd, err := os.Readlink(filepath.Join(Proc, pid, "cwd"))
	if err != nil {
		return "", fmt.Errorf("reading the Compose process's directory: %w", err)
	}
	environ, err := os.ReadFile(filepath.Join(Proc, pid, "environ"))
	if err != nil {
		return "", fmt.Errorf("reading the Compose process's environment: %w", err)
	}
	env := map[string]string{}
	for _, kv := range strings.Split(string(environ), "\x00") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	// Compose works from the logical directory ($PWD, which may go through
	// a link), the kernel gives the physical one: take $PWD when it is the
	// same directory.
	if pwd := env["PWD"]; filepath.IsAbs(pwd) && sameFile(pwd, filepath.Join(Proc, pid, "cwd")) {
		cwd = pwd
	}
	f, _, err := ParseFlags(args)
	if err != nil {
		return "", fmt.Errorf("reading the Compose command line: %w", err)
	}
	dir, err := Dir(f, func(k string) string { return env[k] }, cwd)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", errors.New("the project directory " + dir + " is not a directory")
	}
	return dir, nil
}
