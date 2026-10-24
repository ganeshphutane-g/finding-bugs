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
	"sync/atomic"
	"time"

	"github.com/ganeshphutane-g/tsroam/internal/config"
	"github.com/ganeshphutane-g/tsroam/internal/killswitch"
	"github.com/ganeshphutane-g/tsroam/internal/policy"
	"github.com/ganeshphutane-g/tsroam/internal/sys"
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
	// Intact: in place with all of r's rules.
	Intact(r killswitch.Rules) (bool, error)
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

	// gen counts network changes; a pass that saw it move while looking
	// does not act on a trusted verdict.
	gen atomic.Uint64

	mu      sync.Mutex
	state   State
	lastKey string
	lastLog string
	// notes are the problems logged on this network.
	notes map[string]bool
	// offPending: arrived on a trusted network; the exit node is still to
	// be turned off (tailscaled was not running yet).
	offPending bool
	// pauseDone is the pause whose exit-node-off was done (its Until).
	pauseDone time.Time
	// lastAllowLAN is the allow-lan setting the exit node was last set
	// with.
	lastAllowLAN *bool
	wallFor      *killswitch.Rules // the rules last installed
	changes      int
	errors       int
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

// PauseKey names the network for a pause: its kind, interface, gateway
// address, connected prefix and the gateway's MAC (asked fresh), so another
// network that happens to use the same gateway address ends the pause.
func PauseKey(ctx context.Context, s policy.System, p policy.Place) string {
	k := p.Key()
	if p.Gateway.IsValid() {
		if _, _, n, err := s.Connected(netip.PrefixFrom(p.Gateway, 32)); err == nil && n.IsValid() {
			k += " " + n.String()
		}
		if mac, err := s.GatewayMAC(ctx, p.Iface, p.Gateway); err == nil {
			k += " " + mac.String()
		}
	}
	return k
}

// Pass looks once and acts.
func (a *Agent) Pass(ctx context.Context) State {
	gen := a.gen.Load()
	a.mu.Lock()
	cfg := a.Config
	a.mu.Unlock()
	place := policy.Look(ctx, a.Sys, cfg.Trusted)
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
	var err error
	if a.PauseFile != "" {
		if p, ok := ReadPause(a.PauseFile); ok {
			switch {
			case a.now().After(p.Until):
				a.logf("pause over: it ran out")
				a.removePause()
			case place.Kind == policy.Offline:
				a.logf("pause over: the machine went offline")
				a.removePause()
			case p.Key != PauseKey(ctx, a.Sys, place):
				a.logf("pause over: the network changed")
				a.removePause()
			default:
				st.Paused = true
				st.ExitNode, err = a.exitOffForPause(ctx, cfg, p)
				if err != nil {
					notes = append(notes, err.Error())
				}
			}
		}
	}

	switch {
	case st.Paused:
		// Both down, nothing forced.
		st.Wall, err = a.Wall.Active()
		if err == nil && st.Wall && !a.DryRun {
			err = a.Wall.Off()
			st.Wall = err != nil
		}
		if err != nil {
			notes = append(notes, err.Error())
		}
	case place.Kind == policy.Offline:
		// With the kill switch, offline is where a move to an untrusted
		// network starts: it goes in now, before that network is up. The
		// exit node is left as it is.
		if cfg.KillSwitch {
			st.Wall, err = a.wallOn(cfg, place)
			if err != nil {
				notes = append(notes, err.Error())
			}
		} else {
			st.Wall, _ = a.Wall.Active()
		}
		st.ExitNode = a.exitNodeOn(ctx, cfg)
	case place.Kind == policy.Trusted && a.gen.Load() != gen:
		// The network changed while this pass looked: the verdict may be
		// about the last one. The next pass (it is coming) decides.
		st.Wall, _ = a.Wall.Active()
		st.ExitNode = a.exitNodeOn(ctx, cfg)
	case place.Kind == policy.Trusted:
		st.Wall, err = a.wallOff()
		if err != nil {
			notes = append(notes, err.Error())
		}
		st.ExitNode, err = a.exitOffOnArrival(ctx, cfg, moved)
		if err != nil {
			notes = append(notes, err.Error())
		}
	default: // untrusted
		a.mu.Lock()
		a.offPending = false
		a.mu.Unlock()
		// Protect first, then route.
		if cfg.KillSwitch {
			st.Wall, err = a.wallOn(cfg, place)
		} else {
			st.Wall, err = a.wallOff()
		}
		if err != nil {
			notes = append(notes, err.Error())
		}
		st.ExitNode, err = a.exitOn(ctx, cfg, place, moved)
		if err != nil {
			notes = append(notes, err.Error())
		}
	}
	if len(notes) > 0 {
		st.Note = errors.Join(errorsOf(notes)...).Error()
	}

	a.mu.Lock()
	if moved {
		a.notes = nil // a new network: its problems are news
	}
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

// wallRules is the kill switch for a place: DHCP only on its interface,
// and its LAN with allow-lan when the LAN is a private network.
func (a *Agent) wallRules(cfg config.Config, place policy.Place) killswitch.Rules {
	r := a.Rules
	if !place.Gateway.IsValid() {
		return r // offline, or IPv6 only: no LAN interface to name
	}
	r.Iface = place.Iface
	lan := a.lanOf(place)
	if lan.Addr().Is4() {
		r.Net = lan
	}
	if cfg.AllowLAN {
		if killswitch.LANOK(lan) {
			r.LAN = lan
		} else if lan.IsValid() {
			a.noteOnce(fmt.Sprintf("allow-lan: %s is not a private network; it is not let through", lan))
		}
	}
	return r
}

// lanOf is the connected network the place's gateway is on.
func (a *Agent) lanOf(place policy.Place) netip.Prefix {
	if !place.Gateway.IsValid() {
		return netip.Prefix{}
	}
	if _, _, lan, err := a.Sys.Connected(netip.PrefixFrom(place.Gateway, 32)); err == nil {
		return lan
	}
	return netip.Prefix{}
}

// wallOn puts the kill switch in place, or replaces it when its rules
// change (another LAN with allow-lan); it also restores a table someone
// removed.
func (a *Agent) wallOn(cfg config.Config, place policy.Place) (bool, error) {
	r := a.wallRules(cfg, place)
	intact, err := a.Wall.Intact(r)
	if err != nil {
		return false, fmt.Errorf("reading the kill switch: %w", err)
	}
	active := intact
	if !intact {
		if active, err = a.Wall.Active(); err != nil {
			return false, fmt.Errorf("reading the kill switch: %w", err)
		}
	}
	a.mu.Lock()
	same := a.wallFor != nil && *a.wallFor == r
	a.mu.Unlock()
	if intact && same {
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
func (a *Agent) exitOn(ctx context.Context, cfg config.Config, place policy.Place, arrived bool) (bool, error) {
	s, err := a.TS.Status(ctx)
	if err != nil {
		return false, err
	}
	if !s.Running() {
		return false, fmt.Errorf("tailscaled is %s; the exit node is set when it runs", stateName(s.State))
	}
	p, ok := s.Find(cfg.ExitNode)
	if !ok {
		return false, fmt.Errorf("exit node %q is not in the tailnet (tailscale status)", cfg.ExitNode)
	}
	// The LAN-access setting cannot be read back (status does not show
	// it), so it is set again on arrival and when it changes.
	// allow-lan only for a private LAN, as the kill switch does.
	allowLAN := cfg.AllowLAN && killswitch.LANOK(a.lanOf(place))
	a.mu.Lock()
	lanChanged := a.lastAllowLAN == nil || *a.lastAllowLAN != allowLAN
	a.mu.Unlock()
	if s.ExitNodeID == p.ID && !arrived && !lanChanged {
		return true, nil
	}
	if a.DryRun {
		return s.ExitNodeID == p.ID, nil
	}
	if err := a.TS.SetExitNode(ctx, cfg.ExitNode, allowLAN); err != nil {
		return false, err
	}
	a.mu.Lock()
	a.lastAllowLAN = &allowLAN
	was := s.ExitNodeID == p.ID
	if !was {
		a.changes++
	}
	a.mu.Unlock()
	if was {
		return true, nil
	}
	note := ""
	if !p.Online {
		note = " (it is offline now: traffic waits for it)"
	}
	a.logf("exit node on: %s%s", cfg.ExitNode, note)
	return true, nil
}

// exitOffOnArrival turns the exit node off when the machine arrives on a
// trusted network (or tsroam starts on one), as soon as tailscaled runs.
// It is not forced after that: on a trusted network, the user may choose an
// exit node.
func (a *Agent) exitOffOnArrival(ctx context.Context, cfg config.Config, arrived bool) (bool, error) {
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
	p, ok := s.Find(cfg.ExitNode)
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

// exitOffForPause turns the exit node off once per pause: "tsroam off"
// did it too, but a pass running at that moment may have set it again.
func (a *Agent) exitOffForPause(ctx context.Context, cfg config.Config, p Pause) (bool, error) {
	a.mu.Lock()
	done := a.pauseDone.Equal(p.Until)
	a.mu.Unlock()
	s, err := a.TS.Status(ctx)
	if err != nil {
		return false, err
	}
	if done || a.DryRun {
		peer, ok := s.Find(cfg.ExitNode)
		return ok && s.ExitNodeID == peer.ID, nil
	}
	// Passes run one at a time: one that set the exit node just before
	// the pause was written has finished by now.
	if s.ExitNodeID == "" {
		a.mu.Lock()
		a.pauseDone = p.Until
		a.mu.Unlock()
		return false, nil
	}
	if err := a.TS.SetExitNode(ctx, "", false); err != nil {
		return true, err
	}
	a.mu.Lock()
	a.pauseDone = p.Until
	a.changes++
	a.mu.Unlock()
	a.logf("exit node off (paused)")
	return false, nil
}

func (a *Agent) exitNodeOn(ctx context.Context, cfg config.Config) bool {
	s, err := a.TS.Status(ctx)
	if err != nil {
		return false
	}
	p, ok := s.Find(cfg.ExitNode)
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
func (a *Agent) Run(ctx context.Context, interval time.Duration, events <-chan sys.Event) {
	pass := func() {
		if st := a.Pass(ctx); st.Note != "" && ctx.Err() == nil {
			a.noteOnce(st.Note)
		}
	}
	// The first look takes a moment (ARP, probes) and the machine may be
	// on an untrusted network already: protect first.
	a.mu.Lock()
	ks := a.Config.KillSwitch
	a.mu.Unlock()
	if ks && !a.DryRun {
		if ok, err := a.Wall.Intact(a.Rules); err != nil || !ok {
			a.arm("starting; looking at the network")
		}
	}
	pass()
	t := time.NewTicker(interval)
	defer t.Stop()
	guard := time.NewTicker(Guard)
	defer guard.Stop()
	var again <-chan time.Time
	left := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-guard.C:
			if !a.needsPass() {
				continue
			}
		case <-again:
			if left--; left > 0 {
				again = time.After(Again)
			} else {
				again = nil
			}
		case c := <-events:
			a.preArm(c)
			settle := time.NewTimer(Settle)
		wait:
			for {
				select {
				case <-ctx.Done():
					settle.Stop()
					return
				case c := <-events:
					a.preArm(c)
				case <-settle.C:
					break wait
				}
			}
			again, left = time.After(Again), 2
		}
		pass()
	}
}

// Touch says the network changed (a netlink event): a pass looking at that
// moment may be out of date.
func (a *Agent) Touch() { a.gen.Add(1) }

// Guard is how often Run checks, between passes, that the kill switch is
// still intact and whether "tsroam off" has paused it.
var Guard = 2 * time.Second

// needsPass says whether a pass is due before the next interval: the kill
// switch should be in place but is not intact (someone flushed or removed
// it), or a pause appeared or went away.
func (a *Agent) needsPass() bool {
	a.mu.Lock()
	last, cfg, r := a.state, a.Config, a.wallFor
	a.mu.Unlock()
	if a.PauseFile != "" {
		p, paused := ReadPause(a.PauseFile)
		if paused != last.Paused || paused && a.now().After(p.Until) {
			return true
		}
	}
	if cfg.KillSwitch && last.Wall && r != nil {
		if ok, err := a.Wall.Intact(*r); err == nil && !ok {
			return true
		}
	}
	return false
}

// preArm puts the kill switch in at once when the default route's link went
// down or up (a cable moved, Wi-Fi joined: a café can have the same gateway
// address as home) or the default route moved (another interface or gateway, or
// none): the full look (ARP, probes) takes a moment, and the new network
// may already be carrying traffic. A trusted verdict takes it out again.
func (a *Agent) preArm(c sys.Event) {
	a.mu.Lock()
	cfg, last := a.Config, a.state
	a.mu.Unlock()
	if !cfg.KillSwitch || last.Wall || a.DryRun {
		return
	}
	linkMoved := false
	for _, l := range c.Links {
		linkMoved = linkMoved || l == last.Place.Iface
	}
	if !linkMoved {
		iface, _, gw, ok, err := a.Sys.DefaultRoute()
		if err == nil && ok && iface == last.Place.Iface && gw == last.Place.Gateway {
			return
		}
	}
	a.arm("the network changed; looking at it")
}

// arm puts the base kill switch in (no LAN, DHCP on any interface).
func (a *Agent) arm(why string) {
	r := a.Rules
	if err := a.Wall.On(r); err != nil {
		a.noteOnce(err.Error())
		return
	}
	a.mu.Lock()
	a.wallFor = &r
	a.state.Wall = true // until the next pass says otherwise
	a.changes++
	a.mu.Unlock()
	a.logf("kill switch on: %s", why)
}

// noteOnce logs a problem when it is new.
func (a *Agent) noteOnce(n string) {
	a.mu.Lock()
	seen := a.notes[n]
	if a.notes == nil {
		a.notes = map[string]bool{}
	}
	a.notes[n] = true
	a.mu.Unlock()
	if !seen {
		a.logf("note: %s", n)
	}
}

// Stop is a clean stop. On a trusted network nothing is left in place. On
// any other the kill switch stays (a restart or an upgrade must not open a
// gap); "tsroam cleanup" or "tsroam off" removes it. The exit node is left as
// it is.
func (a *Agent) Stop() error {
	active, err := a.Wall.Active()
	if err != nil || !active {
		return err
	}
	if a.Last().Place.Kind != policy.Trusted {
		a.logf("kill switch kept: not on a trusted network (tsroam cleanup removes it)")
		return nil
	}
	if err := a.Wall.Off(); err != nil {
		return err
	}
	a.logf("kill switch off: tsroam is stopping")
	return nil
}

// SetConfig replaces the config (a reload) for the next pass.
func (a *Agent) SetConfig(c config.Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Config = c
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
