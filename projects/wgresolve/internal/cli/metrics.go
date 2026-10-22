package cli

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ganeshphutane-g/wgresolve/internal/watch"
)

// handler serves /metrics (Prometheus text format) and /healthz (200 while
// rounds keep running).
func handler(w *watch.Watcher, interval time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) {
		last := w.LastRun()
		if last.IsZero() || time.Since(last) > 3*interval+10*time.Second {
			http.Error(rw, "no round finished recently", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(rw, "ok")
	})
	mux.HandleFunc("/metrics", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
		st := w.Statuses()
		var b strings.Builder
		help := func(name, typ, text string) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, text, name, typ)
		}
		labels := func(s watch.Status) string {
			return fmt.Sprintf(`interface=%q,peer=%q,name=%q`, s.Peer.Interface, s.Peer.PublicKey.String(), s.Peer.Host)
		}
		help("wgresolve_peers", "gauge", "Peers with a DNS name for their endpoint.")
		fmt.Fprintf(&b, "wgresolve_peers %d\n", len(st))
		help("wgresolve_peer_up", "gauge", "1 when the peer's handshake is fresh.")
		for _, s := range st {
			up := 0
			if s.State == "ok" {
				up = 1
			}
			fmt.Fprintf(&b, "wgresolve_peer_up{%s} %d\n", labels(s), up)
		}
		help("wgresolve_peer_handshake_age_seconds", "gauge", "Seconds since the peer's last handshake (absent if never).")
		for _, s := range st {
			if !s.Handshake.IsZero() {
				fmt.Fprintf(&b, "wgresolve_peer_handshake_age_seconds{%s} %.0f\n", labels(s), time.Since(s.Handshake).Seconds())
			}
		}
		help("wgresolve_endpoint_changes_total", "counter", "Endpoint changes made.")
		for _, s := range st {
			fmt.Fprintf(&b, "wgresolve_endpoint_changes_total{%s} %d\n", labels(s), s.Changes)
		}
		help("wgresolve_resolve_errors_total", "counter", "Failed lookups of the peer's name.")
		for _, s := range st {
			fmt.Fprintf(&b, "wgresolve_resolve_errors_total{%s} %d\n", labels(s), s.DNSErrors)
		}
		help("wgresolve_last_round_timestamp_seconds", "gauge", "When the last round finished.")
		fmt.Fprintf(&b, "wgresolve_last_round_timestamp_seconds %d\n", w.LastRun().Unix())
		rw.Write([]byte(b.String()))
	})
	return mux
}
