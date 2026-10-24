// Package agent applies tsroam's policy: on an untrusted network the exit
// node is on (and, if configured, the kill switch); on a trusted one both
// are off; offline nothing changes; while paused ("tsroam off") nothing is
// forced until the network changes or the pause runs out.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ganeshphutane-g/tsroam/internal/config"
	"github.com/ganeshphutane-g/tsroam/internal/killswitch"
	"github.com/ganeshphutane-g/tsroam/internal/policy"
	"github.com/ganeshphutane-g/tsroam/internal/tailscale"
)

// Tailnet is tailscaled (tailscale.CLI; a fake in tests).
type Tailnet interface {
	Status(ctx context.Context) (tailscale.Status, error)
	SetExitNode(ctx context.Context, node string, allowLAN bool) error
}

// Wall is the kill switch (killswitch.Switch; a fake in tests).
type Wall interface {
	On(r killswitch.Rules) error
	Off() error
	Active() (bool, error)
}

// State is the last pass.
type State struct {
	Place policy.Place
	// ExitNode: the exit node in use is the configured one.
	ExitNode bool
	// Wall: the kill switch is in place.
	Wall bool
	// Paused: "tsroam off" is in effect.
	Paused bool
	// Note is what kept tsroam from doing all it wanted ("" none).
	Note string
	At   time.Time
}

// Agent is the policy, applied.
type Agent struct {
	Sys    policy.System
	TS     Tailnet
	Wall   Wall
	Config config.Config
	// Rules is the kill switch's base (Tun, Mark, Mask); the LAN is added
	// for allow-lan.
	Rules killswitch.Rules
	// PauseFile is where "tsroam off" leaves its pause ("" none).
	PauseFile string
	// Log gets a line per change (nil: none).
	Log    io.Writer
	DryRun bool
	Now    func() time.Time

	mu      sync.Mutex
	state   State
	lastKey string
	lastLog string
	// lastNote is the last problem logged.
	lastNote string
	// offPending: arrived on a trusted network; the exit node is still to
	// be turned off (tailscaled was not running yet).
	offPending bool
	wallFor    *killswitch.Rules // the rules last installed
	changes    int
	errors     int
}

func (a *Agent) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Agent) logf(format string, args ...any) {
	if a.Log != nil {
		fmt.Fprintf(a.Log, format+"\n", args...)
	}
}

// Pause is "tsroam off": until the network's Key changes or Until.
type Pause struct {
	Key   string
	Until time.Time
}

// ReadPause reads a pause file (ok false: none, or unreadable).
func ReadPause(path string) (Pause, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Pause{}, false
	}
	var p Pause
	if json.Unmarshal(b, &p) != nil {
		return Pause{}, false
	}
	return p, true
}

// WritePause writes a pause file.
func WritePause(path string, p Pause) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(p)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Pass looks once and acts.
func (a *Agent) Pass(ctx context.Context) State {
	place := policy.Look(ctx, a.Sys, a.Config.Trusted)
	key := place.Key()
	st := State{Place: place, At: a.now()}
	var notes []string
	if place.Err != nil {
		notes = append(notes, place.Err.Error())
	}

	a.mu.Lock()
	moved := key != a.lastKey
	a.mu.Unlock()

	// A pause lasts while the machine stays on the network it was made on.
	if a.PauseFile != "" {
		if p, ok := ReadPause(a.PauseFile); ok {
			switch {
			case a.now().After(p.Until):
				a.logf("pause over: it ran out")
				a.removePause()
			case p.Key != key && place.Kind != policy.Offline:
				a.logf("pause over: the network changed")
				a.removePause()
			default:
				st.Paused = true
			}
		}
	}

	var err error
	switch {
	case st.Paused:
		// Nothing forced; "tsroam off" already took both down.
		st.Wall, err = a.Wall.Active()
		if err == nil && st.Wall && !a.DryRun {
			err = a.Wall.Off()
			st.Wall = err != nil
		}
		if err != nil {
			notes = append(notes, err.Error())
		}
	case place.Kind == policy.Offline:
		// No traffic leaves anyway; keep what is in place for when it is
		// back (the next network decides).
		st.Wall, _ = a.Wall.Active()
		st.ExitNode = a.exitNodeOn(ctx)
	case place.Kind == policy.Trusted:
		st.Wall, err = a.wallOff()
		if err != nil {
			notes = append(notes, err.Error())
		}
		st.ExitNode, err = a.exitOffOnArrival(ctx, moved)
		if err != nil {
			notes = append(notes, err.Error())
		}
	default: // untrusted
		a.mu.Lock()
		a.offPending = false
		a.mu.Unlock()
		// Protect first, then route.
		if a.Config.KillSwitch {
			st.Wall, err = a.wallOn(place)
		} else {
			st.Wall, err = a.wallOff()
		}
		if err != nil {
			notes = append(notes, err.Error())
		}
		st.ExitNode, err = a.exitOn(ctx)
		if err != nil {
			notes = append(notes, err.Error())
		}
	}
	if len(notes) > 0 {
		st.Note = errors.Join(errorsOf(notes)...).Error()
	}

	a.mu.Lock()
	a.lastKey = key
	a.state = st
	if st.Note != "" {
		a.errors++
	}
	logLine := fmt.Sprintf("%s: %s", place.Kind, place.Reason)
	if st.Paused {
		logLine = "paused (tsroam off): " + logLine
	}
	newLine := logLine != a.lastLog
	a.lastLog = logLine
	a.mu.Unlock()
	if newLine {
		a.logf("%s", logLine)
	}
	return st
}

func errorsOf(notes []string) []error {
	out := make([]error, len(notes))
	for i, n := range notes {
		out[i] = errors.New(n)
	}
	return out
}

func (a *Agent) removePause() {
	if !a.DryRun {
		os.Remove(a.PauseFile)
	}
}

// wallOn puts the kill switch in place, or replaces it when its rules
// change (another LAN with allow-lan); it also restores a table someone
// removed.
func (a *Agent) wallOn(place policy.Place) (bool, error) {
	r := a.Rules
	if a.Config.AllowLAN && place.Gateway.IsValid() {
		if _, _, lan, err := a.Sys.Connected(netip.PrefixFrom(place.Gateway, 32)); err == nil && lan.IsValid() {
			r.LAN = lan
		}
	}
	active, err := a.Wall.Active()
	if err != nil {
		return false, fmt.Errorf("reading the kill switch: %w", err)
	}
	a.mu.Lock()
	same := a.wallFor != nil && *a.wallFor == r
	a.mu.Unlock()
	if active && same {
		return true, nil
	}
	if a.DryRun {
		return active, nil
	}
	if err := a.Wall.On(r); err != nil {
		return active, err
	}
	a.mu.Lock()
	a.wallFor = &r
	a.changes++
	a.mu.Unlock()
	what := "on"
	if active {
		what = "updated"
	}
	if r.LAN.IsValid() {
		a.logf("kill switch %s: only the tailnet, DHCP and %s (allow-lan) are reachable", what, r.LAN)
	} else {
		a.logf("kill switch %s: only the tailnet and DHCP are reachable", what)
	}
	return true, nil
}

func (a *Agent) wallOff() (bool, error) {
	active, err := a.Wall.Active()
	if err != nil {
		return false, fmt.Errorf("reading the kill switch: %w", err)
	}
	if !active || a.DryRun {
		return active, nil
	}
	if err := a.Wall.Off(); err != nil {
		return true, err
	}
	a.mu.Lock()
	a.wallFor = nil
	a.changes++
	a.mu.Unlock()
	a.logf("kill switch off")
	return false, nil
}

// exitOn makes the configured exit node the one in use, every pass: if
// anything turned it off on an untrusted network, it comes back.
func (a *Agent) exitOn(ctx context.Context) (bool, error) {
	s, err := a.TS.Status(ctx)
	if err != nil {
		return false, err
	}
	if !s.Running() {
		return false, fmt.Errorf("tailscaled is %s; the exit node is set when it runs", stateName(s.State))
	}
	p, ok := s.Find(a.Config.ExitNode)
	if !ok {
		return false, fmt.Errorf("exit node %q is not in the tailnet (tailscale status)", a.Config.ExitNode)
	}
	if s.ExitNodeID == p.ID {
		return true, nil
	}
	if a.DryRun {
		return false, nil
	}
	if err := a.TS.SetExitNode(ctx, a.Config.ExitNode, a.Config.AllowLAN); err != nil {
		return false, err
	}
	a.mu.Lock()
	a.changes++
	a.mu.Unlock()
	note := ""
	if !p.Online {
		note = " (it is offline now: traffic waits for it)"
	}
	a.logf("exit node on: %s%s", a.Config.ExitNode, note)
	return true, nil
}

// exitOffOnArrival turns the exit node off when the machine arrives on a
// trusted network (or tsroam starts on one), as soon as tailscaled runs.
// It is not forced after that: on a trusted network, the user may choose an
// exit node.
func (a *Agent) exitOffOnArrival(ctx context.Context, arrived bool) (bool, error) {
	a.mu.Lock()
	if arrived {
		a.offPending = true
	}
	pending := a.offPending
	a.mu.Unlock()
	s, err := a.TS.Status(ctx)
	if err != nil {
		return false, err
	}
	p, ok := s.Find(a.Config.ExitNode)
	ours := ok && s.ExitNodeID == p.ID
	if !pending || !s.Running() {
		return ours, nil
	}
	if s.ExitNodeID == "" {
		a.mu.Lock()
		a.offPending = false
		a.mu.Unlock()
		return false, nil
	}
	if a.DryRun {
		return ours, nil
	}
	if err := a.TS.SetExitNode(ctx, "", false); err != nil {
		return ours, err
	}
	a.mu.Lock()
	a.offPending = false
	a.changes++
	a.mu.Unlock()
	a.logf("exit node off")
	return false, nil
}

func (a *Agent) exitNodeOn(ctx context.Context) bool {
	s, err := a.TS.Status(ctx)
	if err != nil {
		return false
	}
	p, ok := s.Find(a.Config.ExitNode)
	return ok && s.ExitNodeID == p.ID
}

func stateName(s string) string {
	if s == "" {
		return "not running"
	}
	return "in state " + s
}

// Settle is how long Run waits after a change event for more to come.
var Settle = 500 * time.Millisecond

// Again is how long after a change Run looks again, twice: a gateway or a
// probe target may not answer the moment a network comes up.
var Again = 2 * time.Second

// Run passes now, every interval, and soon after each event, until ctx
// ends.
func (a *Agent) Run(ctx context.Context, interval time.Duration, events <-chan struct{}) {
	pass := func() {
		if st := a.Pass(ctx); st.Note != "" && ctx.Err() == nil {
			a.noteOnce(st.Note)
		}
	}
	pass()
	t := time.NewTicker(interval)
	defer t.Stop()
	var again <-chan time.Time
	left := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-again:
			if left--; left > 0 {
				again = time.After(Again)
			} else {
				again = nil
			}
		case <-events:
			settle := time.NewTimer(Settle)
		wait:
			for {
				select {
				case <-ctx.Done():
					settle.Stop()
					return
				case <-events:
				case <-settle.C:
					break wait
				}
			}
			again, left = time.After(Again), 2
		}
		pass()
	}
}

// noteOnce logs a problem when it is new.
func (a *Agent) noteOnce(n string) {
	a.mu.Lock()
	seen := a.lastNote == n
	a.lastNote = n
	a.mu.Unlock()
	if !seen {
		a.logf("note: %s", n)
	}
}

// Stop removes the kill switch (a clean stop is the user's choice); the
// exit node is left as it is.
func (a *Agent) Stop() error {
	active, err := a.Wall.Active()
	if err != nil || !active {
		return err
	}
	if err := a.Wall.Off(); err != nil {
		return err
	}
	a.logf("kill switch off: tsroam is stopping")
	return nil
}

// Last is the last pass.
func (a *Agent) Last() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// Counts are changes made and passes with a problem.
func (a *Agent) Counts() (changes, errs int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.changes, a.errors
}
