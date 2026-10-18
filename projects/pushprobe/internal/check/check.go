// Package check runs the checks.
package check

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

// Result is the outcome of one check.
type Result struct {
	OK bool
	// Message says what was found, briefly: it is shown in Kuma and Gatus.
	Message string
	// Duration is how long the check took (the last attempt's).
	Duration time.Duration
}

// Checker runs one kind of check. Run must return when ctx ends.
type Checker interface {
	Run(ctx context.Context) Result
}

// New makes the checker for a check.
func New(c config.Check) (Checker, error) {
	switch {
	case c.HTTP != nil:
		return newHTTP(*c.HTTP), nil
	case c.TCP != nil:
		return tcpCheck{c.TCP.Address}, nil
	case c.Ping != nil:
		return pingCheck{*c.Ping}, nil
	case c.DNS != nil:
		return dnsCheck{*c.DNS}, nil
	case c.TLS != nil:
		return tlsCheck{*c.TLS}, nil
	case c.Disk != nil:
		return diskCheck{*c.Disk}, nil
	case c.Memory != nil:
		return memoryCheck{*c.Memory}, nil
	case c.Exec != nil:
		return execCheck{*c.Exec}, nil
	}
	return nil, fmt.Errorf("check %q has nothing to check", c.Name)
}

// MaxMessage is the longest message sent on.
const MaxMessage = 200

func ok(start time.Time, f string, a ...any) Result {
	return Result{OK: true, Message: short(fmt.Sprintf(f, a...)), Duration: time.Since(start)}
}

func fail(start time.Time, f string, a ...any) Result {
	return Result{OK: false, Message: short(fmt.Sprintf(f, a...)), Duration: time.Since(start)}
}

// short keeps a message to one line of at most MaxMessage characters.
func short(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= MaxMessage {
		return s
	}
	r := []rune(s)
	return string(r[:MaxMessage-1]) + "…"
}

// reason is a network error without the noise Go adds ("dial tcp
// 10.0.0.1:80: connect: connection refused" -> "connection refused").
func reason(err error) string {
	s := err.Error()
	for _, cut := range []string{": connect: ", ": dial: ", ": read: ", ": write: "} {
		if i := strings.LastIndex(s, cut); i >= 0 {
			return s[i+len(cut):]
		}
	}
	if strings.Contains(s, "context deadline exceeded") || strings.Contains(s, "i/o timeout") {
		return "timed out"
	}
	return s
}
