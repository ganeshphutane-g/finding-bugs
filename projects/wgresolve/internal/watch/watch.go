// Package watch is the loop: each round it reads the peers' live state,
// resolves their names (from the cache while the TTL holds) and applies the
// engine's decisions.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/ganeshphutane-g/wgresolve/internal/device"
	"github.com/ganeshphutane-g/wgresolve/internal/engine"
	"github.com/ganeshphutane-g/wgresolve/internal/resolve"
	"github.com/ganeshphutane-g/wgresolve/internal/source"
)

// Load finds the configured peers (wg-quick, networkd, NetworkManager and
// flags).
type Load func() (*source.Result, error)

// Watcher holds the loop's state.
type Watcher struct {
	Load     Load
	Resolver *resolve.Resolver
	Devices  device.Devices
	Stale    time.Duration
	// DryRun decides but changes nothing (check).
	DryRun bool
	Log    io.Writer
	Now    func() time.Time

	mu       sync.Mutex
	peers    *source.Result
	memory   map[key]engine.Memory
	status   map[key]*Status
	lastLoad error
	lastRun  time.Time
	alive    time.Time
	confErr  bool
	noted    map[string]bool
	logged   map[key]string
}

type key struct {
	iface string
	pub   wgtypes.Key
}

// Status is one peer's state after the last round.
type Status struct {
	Peer      source.Peer
	State     string // "ok", "changed", "would change", "waiting", "down", "no device", "no peer", "dns error"
	Endpoint  netip.AddrPort
	Handshake time.Time
	Addrs     []netip.Addr
	TTLLeft   time.Duration
	Reason    string
	Changes   int
	DNSErrors int
	LastErr   string
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) logf(k key, msg string) {
	if w.Log == nil {
		return
	}
	fmt.Fprintf(w.Log, "%s %s\n", k.iface, msg)
}

// logOnce logs a peer's message only when its state (what, not the
// message, which carries changing ages) differs from the last one logged,
// so a peer that stays down does not fill the log.
func (w *Watcher) logOnce(k key, what, msg string) {
	if w.logged[k] == what {
		return
	}
	w.logged[k] = what
	if msg != "" {
		w.logf(k, msg)
	}
}

// Reload reads the configuration again. A failure keeps the peers known so
// far.
func (w *Watcher) Reload() error {
	r, err := w.Load()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastLoad = err
	if err != nil {
		return err
	}
	w.peers = r
	return nil
}

// Problems are the configuration problems found by the last load.
func (w *Watcher) Problems() []source.Problem {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.peers == nil {
		return nil
	}
	return w.peers.Problems
}

// Round checks every peer once.
func (w *Watcher) Round(ctx context.Context) {
	w.mu.Lock()
	if w.memory == nil {
		w.memory, w.status, w.logged = map[key]engine.Memory{}, map[key]*Status{}, map[key]string{}
	}
	var peers []source.Peer
	if w.peers != nil {
		peers = w.peers.Peers
	}
	w.alive = w.now()
	w.mu.Unlock()

	// resolv.conf may have changed (another network, a VPN's DNS).
	changed, err := w.Resolver.Refresh()
	switch {
	case changed && w.Log != nil:
		srv, _ := w.Resolver.ServerList()
		fmt.Fprintf(w.Log, "%s changed: asking %s now\n", w.Resolver.ConfPath, strings.Join(srv, ", "))
	case err != nil && !w.confErr && w.Log != nil:
		fmt.Fprintf(w.Log, "cannot read %s, keeping the DNS servers known so far: %v\n", w.Resolver.ConfPath, err)
	}
	w.confErr = err != nil

	live := map[string]*device.Device{}
	liveErr := map[string]error{}
	seen := map[key]bool{}
	next := map[key]*Status{}
	for _, p := range peers {
		if ctx.Err() != nil {
			return
		}
		k := key{p.Interface, p.PublicKey}
		seen[k] = true
		if _, ok := live[p.Interface]; !ok && liveErr[p.Interface] == nil {
			d, err := w.Devices.Device(p.Interface)
			live[p.Interface], liveErr[p.Interface] = d, err
		}
		st := w.statusFor(k, p)
		w.checkPeer(ctx, k, p, st, live[p.Interface], liveErr[p.Interface])
		next[k] = st
		w.mu.Lock()
		w.alive = w.now()
		w.mu.Unlock()
	}
	// Published together, so /metrics never shows half a round.
	w.mu.Lock()
	w.status = next
	for k := range w.memory {
		if !seen[k] {
			delete(w.memory, k)
			delete(w.logged, k)
		}
	}
	w.lastRun = w.now()
	w.mu.Unlock()
}

// Alive is when the loop last made progress (a peer checked, or a round
// begun): a slow DNS server makes rounds long, not the daemon dead.
func (w *Watcher) Alive() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.alive
}

// checkPeer checks one peer and acts; st is this round's copy of its status.
func (w *Watcher) checkPeer(ctx context.Context, k key, p source.Peer, st *Status, live *device.Device, liveErr error) {
	if err := liveErr; err != nil {
		st.State, st.Reason = "no device", err.Error()
		if errors.Is(err, device.ErrNoDevice) {
			st.Reason = "the interface is not up"
		}
		w.logOnce(k, "no device", fmt.Sprintf("peer %s (%s): %s", short(p.PublicKey), p.Host, st.Reason))
		return
	}
	lp, ok := live.Peers[p.PublicKey]
	if !ok {
		st.State, st.Reason = "no peer", "the interface has no such peer (configured in "+p.Source+")"
		w.logOnce(k, "no peer", fmt.Sprintf("peer %s (%s): %s", short(p.PublicKey), p.Host, st.Reason))
		return
	}
	st.Endpoint, st.Handshake = lp.Endpoint, lp.LastHandshake
	now := w.now()
	w.mu.Lock()
	mem := engine.Observe(now, enginePeer(lp), w.memory[k])
	w.memory[k] = mem
	w.mu.Unlock()
	// A working peer needs no lookup at all.
	if lp.Endpoint.IsValid() && engine.Working(now, mem, w.stale()) {
		st.State, st.Reason = "ok", fmt.Sprintf("working (last heard from %s ago)", now.Sub(mem.Active).Round(time.Second))
		if w.DryRun {
			// check shows the answers too, and flags a name that no longer
			// resolves before the peer needs it.
			if a := w.Resolver.Lookup(ctx, p.Host, live.Mark); a.Err != nil {
				st.State, st.Reason = "dns error", "working, but its name does not resolve: "+a.Err.Error()
			} else {
				st.Addrs, st.TTLLeft = a.Addrs, a.Expires.Sub(now)
			}
			return
		}
		if prev := w.logged[k]; prev != "" && prev != "ok" {
			w.logf(k, fmt.Sprintf("peer %s (%s): working again", short(p.PublicKey), p.Host))
		}
		w.logOnce(k, "ok", "")
		return
	}
	a := w.Resolver.Lookup(ctx, p.Host, live.Mark)
	if a.Err != nil {
		st.State, st.Reason, st.LastErr = "dns error", a.Err.Error(), a.Err.Error()
		if !a.Cached {
			st.DNSErrors++
		}
		w.logOnce(k, "dns error "+resolve.Class(a.Err), fmt.Sprintf("peer %s: cannot resolve %s: %v", short(p.PublicKey), p.Host, a.Err))
		return
	}
	st.Addrs, st.TTLLeft = a.Addrs, a.Expires.Sub(now)
	d := engine.Decide(now, enginePeer(lp), &mem, engine.Answer{Addrs: a.Addrs, Failed: a.Failed}, uint16(p.Port), w.stale())
	w.mu.Lock()
	if !w.DryRun {
		w.memory[k] = mem // the pass through the addresses is remembered
	}
	w.mu.Unlock()
	st.Reason = d.Reason
	if !d.Set.IsValid() {
		st.State = "down"
		if d.Waiting {
			st.State = "waiting"
		} else if d.Reason == "working" {
			st.State = "ok"
		}
		w.logOnce(k, st.State+" "+lp.Endpoint.String(), fmt.Sprintf("peer %s (%s): %s", short(p.PublicKey), p.Host, d.Reason))
		return
	}
	if w.DryRun {
		st.State = "would change"
		st.Reason = fmt.Sprintf("would set %s: %s", d.Set, d.Reason)
		return
	}
	// The lookup may have taken a while: look at the peer again, and leave
	// it if anything arrived or its endpoint moved meanwhile.
	if again, err := w.Devices.Device(p.Interface); err == nil {
		now2 := w.now()
		cur, ok := again.Peers[p.PublicKey]
		if !ok {
			st.State, st.Reason = "no peer", "the peer was removed from the interface"
			return
		}
		if cur.Rx > lp.Rx || cur.Endpoint != lp.Endpoint || cur.LastHandshake.After(lp.LastHandshake) {
			w.mu.Lock()
			w.memory[k] = engine.Observe(now2, enginePeer(cur), w.memory[k])
			w.mu.Unlock()
			st.State, st.Reason, st.Endpoint = "ok", "it answered while its name was looked up; left alone", cur.Endpoint
			return
		}
	}
	if err := w.Devices.SetEndpoint(p.Interface, p.PublicKey, d.Set); err != nil {
		st.State, st.LastErr = "error", err.Error()
		w.logOnce(k, "error "+resolve.Class(err), fmt.Sprintf("peer %s: setting %s failed: %v", short(p.PublicKey), d.Set, err))
		return
	}
	st.State, st.Endpoint = "changed", d.Set
	st.Changes++
	w.mu.Lock()
	w.memory[k] = engine.Changed(now, enginePeer(lp), mem, d.Set)
	w.mu.Unlock()
	w.logged[k] = "changed"
	was := "none"
	if lp.Endpoint.IsValid() {
		was = lp.Endpoint.String()
	}
	w.logf(k, fmt.Sprintf("peer %s (%s): endpoint %s -> %s: %s", short(p.PublicKey), p.Host, was, d.Set, d.Reason))
}

func enginePeer(p device.Peer) engine.Peer {
	return engine.Peer{Endpoint: p.Endpoint, LastHandshake: p.LastHandshake, Rx: p.Rx, Tx: p.Tx}
}

func (w *Watcher) stale() time.Duration {
	if w.Stale > 0 {
		return w.Stale
	}
	return engine.DefaultStale
}

// statusFor is a copy of the peer's status to fill in this round (counters
// carried over).
func (w *Watcher) statusFor(k key, p source.Peer) *Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	st := &Status{}
	if old := w.status[k]; old != nil {
		st.Changes, st.DNSErrors = old.Changes, old.DNSErrors
	}
	st.Peer = p
	return st
}

// Statuses are the peers' states after the last round, sorted.
func (w *Watcher) Statuses() []Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []Status
	for _, s := range w.status {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Peer.Interface != out[j].Peer.Interface {
			return out[i].Peer.Interface < out[j].Peer.Interface
		}
		return out[i].Peer.PublicKey.String() < out[j].Peer.PublicKey.String()
	})
	return out
}

// LastRun is when the last round finished.
func (w *Watcher) LastRun() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastRun
}

// Run loops until ctx ends: a round every interval, a reload every
// reloadEvery and whenever reload receives.
func (w *Watcher) Run(ctx context.Context, interval, reloadEvery time.Duration, reload <-chan struct{}) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	lastReload := w.now()
	for {
		w.Round(ctx)
		select {
		case <-ctx.Done():
			return
		case <-reload:
			w.reloadLogged()
			lastReload = w.now()
		case <-tick.C:
			if w.now().Sub(lastReload) >= reloadEvery {
				w.reloadLogged()
				lastReload = w.now()
			}
		}
	}
}

func (w *Watcher) reloadLogged() {
	before := len(w.Peers())
	if err := w.Reload(); err != nil {
		if w.Log != nil {
			fmt.Fprintf(w.Log, "reload failed, keeping the peers known so far: %v\n", err)
		}
		return
	}
	if w.Log == nil {
		return
	}
	if n := len(w.Peers()); n != before {
		fmt.Fprintf(w.Log, "reloaded: %d peers whose endpoint is a DNS name (was %d)\n", n, before)
	}
	w.LogProblems()
}

// LogProblems logs configuration problems not logged before.
func (w *Watcher) LogProblems() {
	seen := map[string]bool{}
	for _, p := range w.Problems() {
		key := p.String()
		if !w.noted[key] && w.Log != nil {
			fmt.Fprintf(w.Log, "note: %s\n", key)
		}
		seen[key] = true
	}
	w.noted = seen
}

// Peers are the peers the last load found.
func (w *Watcher) Peers() []source.Peer {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.peers == nil {
		return nil
	}
	return w.peers.Peers
}

func short(k wgtypes.Key) string { return k.String()[:8] + "…" }
