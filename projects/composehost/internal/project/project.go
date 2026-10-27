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

// valueFlags are Compose's global flags that take a value.
var valueFlags = map[string]bool{
	"-f": true, "--file": true, "-p": true, "--project-name": true,
	"--project-directory": true, "--env-file": true, "--profile": true,
	"--ansi": true, "--progress": true, "--parallel": true,
}

// Flags are the global flags that decide the directory.
type Flags struct {
	Files            []string
	ProjectDirectory string
	EnvFiles         []string
}

// ParseFlags reads Compose's global flags, up to the subcommand (or the end).
func ParseFlags(args []string) (Flags, []string) {
	var f Flags
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || !strings.HasPrefix(a, "-") || a == "-" {
			return f, args[i:]
		}
		name, value, hasValue := strings.Cut(a, "=")
		if !hasValue && strings.HasPrefix(a, "-f") && len(a) > 2 {
			name, value, hasValue = "-f", a[2:], true // -fFILE
		}
		if !valueFlags[name] {
			continue // a boolean flag
		}
		if !hasValue {
			if i+1 >= len(args) {
				break
			}
			i++
			value = args[i]
		}
		switch name {
		case "-f", "--file":
			f.Files = append(f.Files, value)
		case "--project-directory":
			f.ProjectDirectory = value
		case "--env-file":
			f.EnvFiles = append(f.EnvFiles, value)
		}
	}
	return f, nil
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
			// Compose also reads these from the .env file (the
			// --env-file ones, else the working directory's).
			envFiles := f.EnvFiles
			if len(envFiles) == 0 {
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
	if len(args) > 0 && args[0] == "compose" {
		args = args[1:]
	}
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
	f, _ := ParseFlags(args)
	dir, err := Dir(f, func(k string) string { return env[k] }, cwd)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", errors.New("the project directory " + dir + " is not a directory")
	}
	return dir, nil
}
