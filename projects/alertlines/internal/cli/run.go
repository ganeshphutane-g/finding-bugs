package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/ganeshphutane-g/alertlines/internal/config"
	"github.com/ganeshphutane-g/alertlines/internal/proxy"
)

// runProxy is `alertlines run`.
func runProxy(ctx context.Context, env Env, c *config.Config) error {
	logf := func(f string, a ...any) {
		fmt.Fprintf(env.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(f, a...))
	}
	e, err := NewEnricher(c)
	if err != nil {
		return err
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{}
	s := &proxy.Server{Enricher: e, Alertmanagers: c.Alertmanagers, Client: &http.Client{Transport: t},
		Timeout: time.Duration(c.Timeout), Tenant: c.Loki.Tenant, Tenants: c.Tenants, Log: logf,
		Username: c.Auth.Username, Password: c.Auth.Password, BearerToken: c.Auth.BearerToken}
	if c.Auth.Username == "" && c.Auth.BearerToken == "" {
		if host, _, _ := net.SplitHostPort(c.Listen); host == "" || host == "0.0.0.0" || host == "::" {
			logf("note: anyone who can reach %s can send alerts through alertlines and have it read Loki; set auth (and send it from the ruler) or listen on a private address", c.Listen)
		}
	}
	l, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", c.Listen, err)
	}
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	var ams []string
	for _, a := range c.Alertmanagers {
		ams = append(ams, config.RedactURL(a))
	}
	logf("alertlines %s listening on %s: lines from %s, alerts on to %s", Version, l.Addr(), config.RedactURL(c.Loki.URL), strings.Join(ams, ", "))
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		srv.Shutdown(sctx) // the batches under way go on
		logf("stopped")
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
