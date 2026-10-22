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
	w.mu.Unlock()

	live := map[string]map[wgtypes.Key]device.Peer{}
	liveErr := map[string]error{}
	seen := map[key]bool{}
	for _, p := range peers {
		if ctx.Err() != nil {
			return
		}
		k := key{p.Interface, p.PublicKey}
		seen[k] = true
		if _, ok := live[p.Interface]; !ok && liveErr[p.Interface] == nil {
			m, err := w.Devices.Peers(p.Interface)
			live[p.Interface], liveErr[p.Interface] = m, err
		}
		st := w.statusFor(k, p)
		w.checkPeer(ctx, k, p, st, live[p.Interface], liveErr[p.Interface])
		w.mu.Lock()
		w.status[k] = st
		w.mu.Unlock()
	}
	w.mu.Lock()
	for k := range w.status {
		if !seen[k] {
			delete(w.status, k)
			delete(w.memory, k)
			delete(w.logged, k)
		}
	}
	w.lastRun = w.now()
	w.mu.Unlock()
}

// checkPeer checks one peer and acts; st is this round's copy of its status.
func (w *Watcher) checkPeer(ctx context.Context, k key, p source.Peer, st *Status, live map[wgtypes.Key]device.Peer, liveErr error) {
	{
		if err := liveErr; err != nil {
			st.State, st.Reason = "no device", err.Error()
			if errors.Is(err, device.ErrNoDevice) {
				st.Reason = "the interface is not up"
			}
			w.logOnce(k, "no device", fmt.Sprintf("peer %s (%s): %s", short(p.PublicKey), p.Host, st.Reason))
			return
		}
		lp, ok := live[p.PublicKey]
		if !ok {
			st.State, st.Reason = "no peer", "the interface has no such peer (configured in "+p.Source+")"
			w.logOnce(k, "no peer", fmt.Sprintf("peer %s (%s): %s", short(p.PublicKey), p.Host, st.Reason))
			return
		}
		st.Endpoint, st.Handshake = lp.Endpoint, lp.LastHandshake
		now := w.now()
		// A working peer needs no lookup at all.
		if !lp.LastHandshake.IsZero() && lp.Endpoint.IsValid() && now.Sub(lp.LastHandshake) < w.stale() {
			st.State, st.Reason = "ok", fmt.Sprintf("working (handshake %s ago)", now.Sub(lp.LastHandshake).Round(time.Second))
			w.logOnce(k, "ok", "")
			return
		}
		a := w.Resolver.Lookup(ctx, p.Host)
		if a.Err != nil {
			st.State, st.Reason, st.LastErr = "dns error", a.Err.Error(), a.Err.Error()
			st.DNSErrors++
			w.logOnce(k, "dns error "+a.Err.Error(), fmt.Sprintf("peer %s: cannot resolve %s: %v", short(p.PublicKey), p.Host, a.Err))
			return
		}
		st.Addrs, st.TTLLeft = a.Addrs, a.Expires.Sub(now)
		w.mu.Lock()
		mem := w.memory[k]
		w.mu.Unlock()
		d := engine.Decide(now, engine.Peer{Endpoint: lp.Endpoint, LastHandshake: lp.LastHandshake}, mem, a.Addrs, uint16(p.Port), w.stale())
		st.Reason = d.Reason
		if d.Retry {
			w.Resolver.Forget(p.Host)
		}
		if !d.Set.IsValid() {
			st.State = "down"
			if mem.Set == lp.Endpoint && now.Sub(mem.SetAt) < w.stale() {
				st.State = "waiting"
			}
			w.logOnce(k, st.State+" "+lp.Endpoint.String(), fmt.Sprintf("peer %s (%s): %s", short(p.PublicKey), p.Host, d.Reason))
			return
		}
		if w.DryRun {
			st.State = "would change"
			st.Reason = fmt.Sprintf("would set %s: %s", d.Set, d.Reason)
			return
		}
		if err := w.Devices.SetEndpoint(p.Interface, p.PublicKey, d.Set); err != nil {
			st.State, st.LastErr = "error", err.Error()
			w.logOnce(k, "error "+err.Error(), fmt.Sprintf("peer %s: setting %s failed: %v", short(p.PublicKey), d.Set, err))
			return
		}
		st.State, st.Endpoint = "changed", d.Set
		st.Changes++
		w.mu.Lock()
		w.memory[k] = engine.Memory{SetAt: now, Set: d.Set}
		w.mu.Unlock()
		w.logged[k] = ""
		was := "none"
		if lp.Endpoint.IsValid() {
			was = lp.Endpoint.String()
		}
		w.logf(k, fmt.Sprintf("peer %s (%s): endpoint %s -> %s: %s", short(p.PublicKey), p.Host, was, d.Set, d.Reason))
	}
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
	if n := len(w.Peers()); n != before && w.Log != nil {
		fmt.Fprintf(w.Log, "reloaded: %d peers with a name for their endpoint (was %d)\n", n, before)
	}
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
