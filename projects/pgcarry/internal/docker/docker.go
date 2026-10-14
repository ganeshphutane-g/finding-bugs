// Package docker runs the docker command-line tool. pgcarry uses the CLI
// rather than the Engine API so that Docker contexts, DOCKER_HOST and Compose
// behave exactly as they do for the user.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// CLI runs docker commands.
type CLI struct {
	// Bin is the docker program (default "docker" on PATH).
	Bin string
	// Env, when set, replaces the environment of every command.
	Env []string
}

// Error is a failed docker command.
type Error struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.Code)
	}
	return fmt.Sprintf("docker %s: %s", strings.Join(redact(e.Args), " "), msg)
}

func (c *CLI) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "docker"
}

// Run runs docker with args and returns its standard output.
func (c *CLI) Run(ctx context.Context, args ...string) (string, error) {
	var out bytes.Buffer
	err := c.Stream(ctx, nil, &out, args...)
	return out.String(), err
}

// Stream runs docker with args, feeding stdin (if not nil) and writing
// standard output to stdout.
func (c *CLI) Stream(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	if c.Env != nil {
		cmd.Env = c.Env
	}
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &limited{w: &stderr, n: 64 << 10}
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return &Error{Args: args, Code: exit.ExitCode(), Stderr: stderr.String()}
	}
	return fmt.Errorf("running docker: %w", err)
}

// limited keeps the first n bytes written to it.
type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := len(p)
		if k > l.n {
			k = l.n
		}
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// IsNotFound reports whether a docker command failed because the object
// does not exist.
func IsNotFound(err error) bool {
	var de *Error
	if !errors.As(err, &de) {
		return false
	}
	s := strings.ToLower(de.Stderr)
	return strings.Contains(s, "no such") || strings.Contains(s, "not found")
}

// redact hides the values of -e NAME=value arguments, which may be
// passwords.
func redact(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "-e" || out[i] == "--env" {
			if k, _, ok := strings.Cut(out[i+1], "="); ok {
				out[i+1] = k + "=***"
			}
		}
	}
	return out
}
