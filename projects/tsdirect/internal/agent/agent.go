// Package agent keeps tsdirect's ip rules in step with where the machine is:
// for each LAN, it looks (connected route, gateway MAC, probe), decides, and
// adds or removes the LAN's rule.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/ganeshphutane-g/tsdirect/internal/config"
	"github.com/ganeshphutane-g/tsdirect/internal/decide"
	"github.com/ganeshphutane-g/tsdirect/internal/sys"
)

// System is what the agent needs from the machine (sys.Linux; a fake in
// tests).
type System interface {
	Connected(p netip.Prefix) (iface string, index int, connected netip.Prefix, err error)
	DefaultGateway(index int) (netip.Addr, bool)
	GatewayMAC(ctx context.Context, iface string, addr netip.Addr) (net.HardwareAddr, error)
	Probe(ctx context.Context, iface string, ap netip.AddrPort) error
	Rules() ([]sys.Rule, error)
	AddRule(r sys.Rule) error
	DelRule(r sys.Rule) error
}

// Status is one LAN as last seen.
type Status struct {
	LAN         config.LAN
	Observation decide.Observation
	Verdict     decide.Verdict
	// Rule: the LAN's rule is in place.
	Rule bool
	// Since: when the verdict last changed.
	Since time.Time
	// Changes: rules added or removed for the LAN.
	Changes int
}

// Agent reconciles the rules.
type Agent struct {
	Sys      System
	LANs     []config.LAN
	Priority int
	// Log gets a line for each rule added or removed and each changed
	// verdict (nil: none).
	Log io.Writer
	// DryRun looks and decides but changes nothing.
	DryRun bool
	Now    func() time.Time

	mu       sync.Mutex
	statuses []Status
	stale    []sys.Rule
	errors   int
	last     time.Time
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

// Observe looks at the machine for one LAN. A fingerprint step is only
// taken when the earlier ones passed: no probe goes out on a network whose
// gateway is not the LAN's.
func Observe(ctx context.Context, s System, l config.LAN) (decide.Observation, error) {
	var o decide.Observation
	iface, index, connected, err := s.Connected(l.Prefix)
	if err != nil {
		return o, fmt.Errorf("reading routes: %w", err)
	}
	if iface == "" {
		return o, nil
	}
	o.Iface, o.Connected = iface, connected
	if connected.Bits() > l.Prefix.Bits() {
		return o, nil
	}
	if l.GatewayMAC != nil {
		gw, ok := l.Gateway, l.Gateway.IsValid()
		if !ok {
			gw, ok = s.DefaultGateway(index)
		}
		if !ok {
			return o, nil
		}
		o.Gateway = gw
		mac, err := s.GatewayMAC(ctx, iface, gw)
		switch {
		case errors.Is(err, sys.ErrNoAnswer):
			return o, nil
		case err != nil:
			o.GatewayErr = err.Error()
			return o, nil
		}
		o.GatewayMAC = mac
		if !bytes.Equal(mac, l.GatewayMAC) {
			return o, nil
		}
	}
	if l.Probe.IsValid() {
		err := s.Probe(ctx, iface, l.Probe)
		ok := err == nil
		o.ProbeOK = &ok
		if err != nil {
			o.ProbeErr = err.Error()
		}
	}
	return o, nil
}

// Reconcile looks at every LAN once, then removes the rules that should not
// be there and adds those that should. Its own rules for prefixes no longer
// configured, or at another priority, are removed too.
func (a *Agent) Reconcile(ctx context.Context) error {
	var errs []error
	have, err := a.Sys.Rules()
	if err != nil {
		a.mu.Lock()
		a.errors++
		a.mu.Unlock()
		return fmt.Errorf("listing ip rules: %w", err)
	}
	present := map[sys.Rule]bool{}
	for _, r := range have {
		present[r] = true
	}
	a.mu.Lock()
	lans := a.LANs
	prev := map[netip.Prefix]Status{}
	for _, s := range a.statuses {
		prev[s.LAN.Prefix] = s
	}
	a.mu.Unlock()

	now := a.now()
	statuses := make([]Status, len(lans))
	want := map[sys.Rule]int{} // the LAN's index
	newReason := make([]bool, len(lans))
	changed := make([]bool, len(lans))
	for i, l := range lans {
		o, err := Observe(ctx, a.Sys, l)
		v := decide.Decide(l, o)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", l.Prefix, err))
			v = decide.Verdict{Kind: decide.Error, Reason: fmt.Sprintf("could not look at %s: %v", l.Prefix, err)}
		}
		st := Status{LAN: l, Observation: o, Verdict: v, Since: now}
		// A verdict is the same while its kind and interface are: a
		// failing probe's error text may differ from one pass to the next.
		if p, ok := prev[l.Prefix]; ok && p.Verdict.Kind == v.Kind && p.Observation.Iface == o.Iface {
			st.Since = p.Since
		} else {
			newReason[i] = true
		}
		if p, ok := prev[l.Prefix]; ok {
			st.Changes = p.Changes
		}
		if v.On {
			want[sys.Rule{Prefix: l.Prefix, Priority: a.Priority}] = i
		}
		statuses[i] = st
	}
	index := map[netip.Prefix]int{}
	for i, l := range lans {
		index[l.Prefix] = i
	}

	// Removals first, so a rule moving to a new priority is never doubled.
	var stale []sys.Rule
	for _, r := range have {
		if _, ok := want[r]; ok {
			continue
		}
		why := "not configured"
		if i, ok := index[r.Prefix]; ok {
			if r.Priority != a.Priority {
				why = fmt.Sprintf("priority is now %d", a.Priority)
			} else {
				why = statuses[i].Verdict.Reason
			}
		}
		if _, ok := index[r.Prefix]; !ok || r.Priority != a.Priority {
			stale = append(stale, r)
		}
		if a.DryRun {
			continue
		}
		if err := a.Sys.DelRule(r); err != nil {
			errs = append(errs, fmt.Errorf("removing the rule for %s: %w", r.Prefix, err))
			if i, ok := index[r.Prefix]; ok && r.Priority == a.Priority {
				statuses[i].Rule = true
			}
			continue
		}
		a.logf("removed rule to %s lookup main priority %d: %s", r.Prefix, r.Priority, why)
		if i, ok := index[r.Prefix]; ok {
			statuses[i].Changes++
			changed[i] = true
		}
	}
	for i, l := range lans {
		r := sys.Rule{Prefix: l.Prefix, Priority: a.Priority}
		if _, ok := want[r]; !ok {
			continue
		}
		if present[r] || a.DryRun {
			statuses[i].Rule = present[r]
			continue
		}
		if err := a.Sys.AddRule(r); err != nil {
			errs = append(errs, fmt.Errorf("adding the rule for %s: %w", r.Prefix, err))
			continue
		}
		statuses[i].Rule = true
		statuses[i].Changes++
		changed[i] = true
		a.logf("added rule to %s lookup main priority %d: %s", r.Prefix, r.Priority, statuses[i].Verdict.Reason)
	}
	// A changed verdict is logged with the rule it moved, or on its own.
	for i, l := range lans {
		if newReason[i] && !changed[i] {
			a.logf("%s: %s", l.Prefix, statuses[i].Verdict.Reason)
		}
	}
	if a.DryRun {
		for i, l := range lans {
			statuses[i].Rule = present[sys.Rule{Prefix: l.Prefix, Priority: a.Priority}]
		}
	}

	a.mu.Lock()
	a.statuses = statuses
	a.stale = stale
	a.last = a.now()
	a.errors += len(errs)
	a.mu.Unlock()
	return errors.Join(errs...)
}

// SetLANs replaces the LANs (a reloaded config) for the next pass.
func (a *Agent) SetLANs(lans []config.LAN) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.LANs = lans
}

// Cleanup removes every rule of tsdirect's (by its protocol mark) and says
// how many it removed.
func (a *Agent) Cleanup() (int, error) {
	rules, err := a.Sys.Rules()
	if err != nil {
		return 0, fmt.Errorf("listing ip rules: %w", err)
	}
	n := 0
	var errs []error
	for _, r := range rules {
		if err := a.Sys.DelRule(r); err != nil {
			errs = append(errs, fmt.Errorf("removing the rule for %s: %w", r.Prefix, err))
			continue
		}
		n++
		a.logf("removed rule to %s lookup main priority %d: tsdirect is stopping", r.Prefix, r.Priority)
	}
	return n, errors.Join(errs...)
}

// Settle is how long Run waits after a change event for more to come, so a
// burst (Wi-Fi joining: link, address, routes) is one pass.
var Settle = 500 * time.Millisecond

// Again is how long after a change Run looks again, twice: a gateway or a
// probe target may not answer the moment a network comes up.
var Again = 2 * time.Second

// Run reconciles now, every interval, and soon after each event, until ctx
// ends. It does not remove the rules; the caller does (Cleanup).
func (a *Agent) Run(ctx context.Context, interval time.Duration, events <-chan struct{}) {
	pass := func() {
		if err := a.Reconcile(ctx); err != nil && ctx.Err() == nil {
			a.logf("error: %v", err)
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

// Statuses is each LAN as last seen.
func (a *Agent) Statuses() []Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Status(nil), a.statuses...)
}

// Stale is tsdirect's rules seen in the last pass that belong to no
// configured LAN, or are at another priority (a dry run leaves them).
func (a *Agent) Stale() []sys.Rule {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sys.Rule(nil), a.stale...)
}

// Errors counts failed steps so far.
func (a *Agent) Errors() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.errors
}

// LastPass is when the last pass finished (zero: none yet).
func (a *Agent) LastPass() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}
