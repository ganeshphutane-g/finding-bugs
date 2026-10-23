package cli

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ganeshphutane-g/tsdirect/internal/agent"
)

// handler serves /metrics (Prometheus text format) and /healthz (200 while
// passes keep running).
func handler(a *agent.Agent, interval time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) {
		last := a.LastPass()
		if last.IsZero() || time.Since(last) > 3*interval+30*time.Second {
			http.Error(rw, "no pass has finished recently", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(rw, "ok")
	})
	mux.HandleFunc("/metrics", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
		st := a.Statuses()
		var b strings.Builder
		help := func(name, typ, text string) {
			fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, text, name, typ)
		}
		bit := func(v bool) int {
			if v {
				return 1
			}
			return 0
		}
		help("tsdirect_lans", "gauge", "LANs configured.")
		fmt.Fprintf(&b, "tsdirect_lans %d\n", len(st))
		help("tsdirect_lan_connected", "gauge", "1 when the LAN's addresses are directly connected.")
		for _, s := range st {
			fmt.Fprintf(&b, "tsdirect_lan_connected{lan=%q} %d\n", s.LAN.Prefix.String(), bit(s.Observation.Iface != ""))
		}
		help("tsdirect_lan_direct", "gauge", "1 when the LAN is confirmed (addresses and fingerprint).")
		for _, s := range st {
			fmt.Fprintf(&b, "tsdirect_lan_direct{lan=%q} %d\n", s.LAN.Prefix.String(), bit(s.Verdict.On))
		}
		help("tsdirect_lan_rule", "gauge", "1 when the LAN's rule is in place.")
		for _, s := range st {
			fmt.Fprintf(&b, "tsdirect_lan_rule{lan=%q} %d\n", s.LAN.Prefix.String(), bit(s.Rule))
		}
		help("tsdirect_rule_changes_total", "counter", "Rules added or removed for the LAN.")
		for _, s := range st {
			fmt.Fprintf(&b, "tsdirect_rule_changes_total{lan=%q} %d\n", s.LAN.Prefix.String(), s.Changes)
		}
		help("tsdirect_errors_total", "counter", "Failed steps (reading routes, changing rules).")
		fmt.Fprintf(&b, "tsdirect_errors_total %d\n", a.Errors())
		if last := a.LastPass(); !last.IsZero() {
			help("tsdirect_last_pass_timestamp_seconds", "gauge", "When the last pass finished.")
			fmt.Fprintf(&b, "tsdirect_last_pass_timestamp_seconds %d\n", last.Unix())
		}
		rw.Write([]byte(b.String()))
	})
	return mux
}
