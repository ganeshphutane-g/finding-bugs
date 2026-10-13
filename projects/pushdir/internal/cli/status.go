package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/ganeshphutane-g/pushdir/internal/push"
)

// serveStatus serves /status (JSON) and /metrics (Prometheus text format).
func serveStatus(addr string, p *push.Pusher) (func(), string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("--listen %s: %w", addr, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(p.Status())
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		s := p.Status()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		gauge := func(name, help string, v float64) {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
		}
		counter := func(name, help string, v int64) {
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n%s %d\n", name, help, name, name, v)
		}
		counter("pushdir_files_sent_total", "Files sent to the remote.", s.Sent)
		counter("pushdir_files_deleted_total", "Files deleted on the remote.", s.Deleted)
		counter("pushdir_batches_total", "Batches sent.", s.Batches)
		counter("pushdir_failed_batches_total", "Batches that failed (and were retried).", s.FailedBatches)
		counter("pushdir_full_resyncs_total", "Full checks of the folder.", s.Resyncs)
		gauge("pushdir_pending_files", "Changed files not sent yet.", float64(s.Pending))
		gauge("pushdir_pending_deletes", "Deletions not applied yet.", float64(s.PendingDeletes))
		gauge("pushdir_watched_folders", "Folders being watched.", float64(s.WatchedFolders))
		paused := 0.0
		if s.DeletionsPaused != "" {
			paused = 1
		}
		gauge("pushdir_deletions_paused", "1 when deletions are paused by a safety check.", paused)
		if !s.LastSuccess.IsZero() {
			gauge("pushdir_last_success_timestamp_seconds", "When the last batch or full check succeeded.", float64(s.LastSuccess.Unix()))
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: time.Minute}
	go srv.Serve(ln)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, ln.Addr().String(), nil
}
