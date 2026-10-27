// Package protocol is the Compose side of a provider service: how Compose
// calls the binary ("compose [--project-name=P] up|down|stop|metadata
// [--opt=v ...] SERVICE") and the JSON lines it reads back on stdout.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Call is one invocation by Compose.
type Call struct {
	Project string
	// Command is up, down, stop or metadata.
	Command string
	// Options are the --name=value flags, in order; a repeated flag has
	// several values.
	Options map[string][]string
	Service string
}

// ParseArgs reads Compose's arguments (without the program name):
//
//	compose --project-name=P up --mkdir=./data --owner=1000:1000 SERVICE
//	compose metadata
//
// known says, per command, which options exist; any other is an error, so a
// typo in the compose file does not pass unnoticed.
func ParseArgs(args []string, known map[string]map[string]bool) (Call, error) {
	var c Call
	if len(args) == 0 || args[0] != "compose" {
		return c, errors.New(`composehost runs as a Compose provider: Compose calls it as "composehost compose ..." (see "composehost help")`)
	}
	args = args[1:]
	// Global flags, then the command.
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		name, value, ok, rest, err := flagOf(args)
		if err != nil {
			return c, err
		}
		args = rest
		switch name {
		case "project-name", "p":
			if !ok {
				return c, errors.New("--project-name needs a value")
			}
			c.Project = value
		default:
			return c, fmt.Errorf("unknown flag --%s before the command", name)
		}
	}
	if len(args) == 0 {
		return c, errors.New("no command (up, down, stop or metadata)")
	}
	c.Command, args = args[0], args[1:]
	opts, isKnown := known[c.Command]
	if !isKnown && c.Command != "metadata" {
		return c, fmt.Errorf("unknown command %q (up, down, stop or metadata)", c.Command)
	}
	c.Options = map[string][]string{}
	for len(args) > 0 {
		if !strings.HasPrefix(args[0], "--") {
			if c.Service != "" {
				return c, fmt.Errorf("unexpected %q after the service %q", args[0], c.Service)
			}
			c.Service, args = args[0], args[1:]
			continue
		}
		name, value, ok, rest, err := flagOf(args)
		if err != nil {
			return c, err
		}
		args = rest
		if name == "project-name" { // Compose may also put it after the command
			c.Project = value
			continue
		}
		if !opts[name] {
			return c, fmt.Errorf("unknown option %q for %s", name, c.Command)
		}
		if !ok {
			return c, fmt.Errorf("option %q needs a value", name)
		}
		c.Options[name] = append(c.Options[name], value)
	}
	if c.Command != "metadata" && c.Service == "" {
		return c, fmt.Errorf("%s: no service name", c.Command)
	}
	return c, nil
}

// flagOf reads one flag, "--name=value" or "--name value" (a value that
// does not start with "-"), and returns the remaining arguments.
func flagOf(args []string) (name, value string, hasValue bool, rest []string, err error) {
	a := strings.TrimLeft(args[0], "-")
	if a == "" {
		return "", "", false, nil, fmt.Errorf("bad flag %q", args[0])
	}
	if n, v, ok := strings.Cut(a, "="); ok {
		return n, v, true, args[1:], nil
	}
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") && a == "project-name" {
		return a, args[1], true, args[2:], nil
	}
	return a, "", false, args[1:], nil
}

// One returns an option's only value ("" if absent); several are an error.
func (c Call) One(name string) (string, error) {
	v := c.Options[name]
	switch len(v) {
	case 0:
		return "", nil
	case 1:
		return v[0], nil
	}
	return "", fmt.Errorf("option %q given %d times; it takes one value", name, len(v))
}

// Out writes JSON lines for Compose.
type Out struct {
	mu sync.Mutex
	W  io.Writer
}

type message struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (o *Out) send(typ, msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	b, _ := json.Marshal(message{typ, msg})
	o.W.Write(append(b, '\n'))
}

// Info is shown as the service's state in Compose's progress.
func (o *Out) Info(format string, args ...any) { o.send("info", fmt.Sprintf(format, args...)) }

// Debug is shown with docker compose --verbose.
func (o *Out) Debug(format string, args ...any) { o.send("debug", fmt.Sprintf(format, args...)) }

// Error is shown as the reason the service failed.
func (o *Out) Error(msg string) { o.send("error", msg) }

// RawSetEnv gives dependent services KEY=VALUE as is.
func (o *Out) RawSetEnv(key, value string) { o.send("rawsetenv", key+"="+value) }

// Param is one option in the metadata.
type Param struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Type        string `json:"type"`
	Default     string `json:"default,omitempty"`
}

// Command lists a command's options in the metadata.
type Command struct {
	Parameters []Param `json:"parameters"`
}

// Metadata is what "compose metadata" prints. Declaring stop opts into
// "docker compose stop".
type Metadata struct {
	Description string   `json:"description"`
	Up          Command  `json:"up"`
	Down        Command  `json:"down"`
	Stop        *Command `json:"stop,omitempty"`
}

// Known is the option names per command, from the metadata.
func (m Metadata) Known() map[string]map[string]bool {
	k := map[string]map[string]bool{"up": {}, "down": {}, "stop": {}}
	for cmd, c := range map[string]*Command{"up": &m.Up, "down": &m.Down, "stop": m.Stop} {
		if c == nil {
			continue
		}
		for _, p := range c.Parameters {
			k[cmd][p.Name] = true
		}
	}
	return k
}
