package cli

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ganeshphutane-g/tsroam/internal/agent"
	"github.com/ganeshphutane-g/tsroam/internal/policy"
)

// handler serves /metrics (Prometheus text format) and /healthz (200 while
// passes keep running).
func handler(a *agent.Agent, interval time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(rw http.ResponseWriter, r *http.Request) {
		at := a.Last().At
		if at.IsZero() || time.Since(at) > 3*interval+30*time.Second {
			http.Error(rw, "no pass has finished recently", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(rw, "ok")
	})
	mux.HandleFunc("/metrics", func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "text/plain; version=0.0.4")
		st := a.Last()
		changes, errs := a.Counts()
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
		help("tsroam_network", "gauge", "1 for the kind of network the machine is on.")
		for _, k := range []policy.Kind{policy.Offline, policy.Trusted, policy.Untrusted} {
			fmt.Fprintf(&b, "tsroam_network{kind=%q} %d\n", k, bit(st.Place.Kind == k))
		}
		help("tsroam_exit_node", "gauge", "1 when the configured exit node is in use.")
		fmt.Fprintf(&b, "tsroam_exit_node %d\n", bit(st.ExitNode))
		help("tsroam_kill_switch", "gauge", "1 when the kill switch is in place.")
		fmt.Fprintf(&b, "tsroam_kill_switch %d\n", bit(st.Wall))
		help("tsroam_paused", "gauge", "1 while tsroam off is in effect.")
		fmt.Fprintf(&b, "tsroam_paused %d\n", bit(st.Paused))
		help("tsroam_changes_total", "counter", "Exit-node and kill-switch changes made.")
		fmt.Fprintf(&b, "tsroam_changes_total %d\n", changes)
		help("tsroam_problem_passes_total", "counter", "Passes that could not do all they wanted.")
		fmt.Fprintf(&b, "tsroam_problem_passes_total %d\n", errs)
		if !st.At.IsZero() {
			help("tsroam_last_pass_timestamp_seconds", "gauge", "When the last pass started.")
			fmt.Fprintf(&b, "tsroam_last_pass_timestamp_seconds %d\n", st.At.Unix())
		}
		rw.Write([]byte(b.String()))
	})
	return mux
}
