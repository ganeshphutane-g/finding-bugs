// Package cli is xffdns' command line.
package cli

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ganeshphutane-g/xffdns/internal/config"
	"github.com/ganeshphutane-g/xffdns/internal/metrics"
	"github.com/ganeshphutane-g/xffdns/internal/server"
	"github.com/ganeshphutane-g/xffdns/internal/upstream"
)

// Version is set at build time.
var Version = "dev"

// Exit codes.
const (
	exitOK      = 0
	exitError   = 1
	exitUsage   = 2
	exitIgnored = 3
)

const usage = `xffdns passes DNS queries (UDP, TCP, DoT) on to AdGuard Home's DoH endpoint,
naming the real client in X-Forwarded-For.

Usage:
  xffdns run   [-c xffdns.yaml]
  xffdns check [-c xffdns.yaml | --url URL] [--ip 192.0.2.10] [--name NAME] [--client-id ID]
               [--adguard-url URL --adguard-user USER]   (password: XFFDNS_ADGUARD_PASSWORD)
  xffdns version

Exit codes: 0 fine, 1 error, 2 bad usage, 3 check saw AdGuard Home ignore X-Forwarded-For.
`

// Main runs a command and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "run":
		return run(args[1:], stderr)
	case "check":
		return check(args[1:], stdout, stderr, os.LookupEnv)
	case "version", "--version", "-version":
		fmt.Fprintln(stdout, "xffdns", Version)
		return exitOK
	case "help", "--help", "-h", "-help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprintf(stderr, "xffdns: unknown command %q\n\n%s", args[0], usage)
	return exitUsage
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("xffdns "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	return fs
}

func newUpstream(u config.Upstream) (*upstream.Client, error) {
	return upstream.New(upstream.Options{URL: u.URL, Timeout: time.Duration(u.Timeout), CAFile: u.CAFile,
		ServerName: u.ServerName, UserAgent: "xffdns/" + Version})
}

func run(args []string, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serve(ctx, args, stderr, nil)
}

// serve runs until ctx ends. ready, if set, gets the server once it listens.
func serve(ctx context.Context, args []string, stderr io.Writer, ready func(*server.Server)) int {
	fs := newFlags("run", stderr)
	path := fs.String("c", "xffdns.yaml", "the configuration file")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return exitUsage
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	conf, err := config.LoadFile(*path)
	if err != nil {
		fmt.Fprintf(stderr, "xffdns: %s: %v\n", *path, err)
		return exitError
	}
	up, err := newUpstream(conf.Upstream)
	if err != nil {
		fmt.Fprintf(stderr, "xffdns: upstream: %v\n", err)
		return exitError
	}
	defer up.Close()
	m := metrics.New()
	srv := server.New(server.Options{Upstream: up, Limits: conf.Limits, Metrics: m, Log: log})
	if err := srv.Start(conf.Listeners); err != nil {
		fmt.Fprintf(stderr, "xffdns: %v\n", err)
		return exitError
	}
	var hs *http.Server
	if conf.MetricsListen != "" {
		ln, err := net.Listen("tcp", conf.MetricsListen)
		if err != nil {
			fmt.Fprintf(stderr, "xffdns: metrics_listen: %v\n", err)
			srv.Shutdown(context.Background())
			return exitError
		}
		hs = &http.Server{Handler: m.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go hs.Serve(ln)
		log.Info("metrics", "address", ln.Addr().String())
	}
	log.Info("xffdns started", "version", Version, "upstream", conf.Upstream.URL)
	if ready != nil {
		ready(srv)
	}
	<-ctx.Done()
	log.Info("stopping: answering the queries in flight (up to 10s)")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	if hs != nil {
		hs.Shutdown(sctx)
	}
	return exitOK
}

var rcodes = map[int]string{0: "NOERROR", 1: "FORMERR", 2: "SERVFAIL", 3: "NXDOMAIN", 4: "NOTIMP", 5: "REFUSED"}

func check(args []string, stdout, stderr io.Writer, lookup func(string) (string, bool)) int {
	fs := newFlags("check", stderr)
	path := fs.String("c", "", "the configuration file (for upstream)")
	rawURL := fs.String("url", "", "AdGuard Home's DoH URL, instead of -c")
	ipFlag := fs.String("ip", "192.0.2.10", "the client address to send")
	name := fs.String("name", "xffdns-check.example.com", "the name to ask for")
	clientID := fs.String("client-id", "", "a client ID to send")
	agURL := fs.String("adguard-url", "", "AdGuard Home's web address (default: the DoH URL's)")
	agUser := fs.String("adguard-user", "", "an AdGuard Home user, to read the query log")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return exitUsage
	}
	ip, err := netip.ParseAddr(*ipFlag)
	if err != nil {
		fmt.Fprintf(stderr, "xffdns check: --ip %q is not an address\n", *ipFlag)
		return exitUsage
	}
	if *clientID != "" && !config.ValidClientID(*clientID) {
		fmt.Fprintf(stderr, "xffdns check: --client-id %q: AdGuard Home allows lowercase letters, digits and -, at most 64\n", *clientID)
		return exitUsage
	}
	var up config.Upstream
	switch {
	case *rawURL != "" && *path != "":
		fmt.Fprintln(stderr, "xffdns check: use -c or --url, not both")
		return exitUsage
	case *rawURL != "":
		up = config.Upstream{URL: *rawURL, Timeout: config.Duration(config.DefaultTimeout)}
	default:
		if *path == "" {
			*path = "xffdns.yaml"
		}
		conf, err := config.LoadFile(*path)
		if err != nil {
			fmt.Fprintf(stderr, "xffdns check: %s: %v\n", *path, err)
			return exitError
		}
		up = conf.Upstream
	}
	u, err := url.Parse(up.URL)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		fmt.Fprintf(stderr, "xffdns check: %q is not an http:// or https:// address\n", up.URL)
		return exitUsage
	}
	client, err := newUpstream(up)
	if err != nil {
		fmt.Fprintf(stderr, "xffdns check: %v\n", err)
		return exitError
	}
	defer client.Close()

	q := makeQuery(*name)
	fmt.Fprintf(stdout, "query   %s A, as %s, to %s\n", strings.TrimSuffix(*name, "."), ip, client.URL(*clientID))
	sent := time.Now()
	ans, err := client.Exchange(context.Background(), q, ip, *clientID)
	if err != nil {
		fmt.Fprintf(stdout, "failed  %v\n", err)
		if errors.Is(err, upstream.ErrStatus) && u.Scheme == "http" {
			fmt.Fprintln(stdout, "        Over plain http, AdGuard Home needs http.doh.insecure_enabled: true in AdGuardHome.yaml.")
		}
		return exitError
	}
	fmt.Fprintf(stdout, "answer  %s, %d records, in %s\n", rcodeName(ans), binary.BigEndian.Uint16(ans[6:]), time.Since(sent).Round(time.Millisecond))

	if *agUser == "" {
		fmt.Fprintln(stdout, "\nTo see whether AdGuard Home took the address, give --adguard-user (and XFFDNS_ADGUARD_PASSWORD),\nor look for the query in its query log.")
		return exitOK
	}
	pass, _ := lookup("XFFDNS_ADGUARD_PASSWORD")
	web := *agURL
	if web == "" {
		web = (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
	}
	entry, err := findInLog(web, *agUser, pass, *name, sent, up)
	if err != nil {
		fmt.Fprintf(stdout, "log     %v\n", err)
		return exitError
	}
	if entry.ClientID != "" || *clientID != "" {
		fmt.Fprintf(stdout, "log     client %s, client ID %q\n", entry.Client, entry.ClientID)
	} else {
		fmt.Fprintf(stdout, "log     client %s\n", entry.Client)
	}
	if got, err := netip.ParseAddr(entry.Client); err != nil || got.Unmap() != ip.Unmap() {
		fmt.Fprintf(stdout, "\nAdGuard Home logged the query as from %s, not %s: it does not trust xffdns's X-Forwarded-For.\n"+
			"Add the address xffdns connects from (%s, if that is it) to dns.trusted_proxies in AdGuardHome.yaml, and restart it.\n",
			entry.Client, ip, entry.Client)
		return exitIgnored
	}
	if *clientID != "" && entry.ClientID != *clientID {
		fmt.Fprintf(stdout, "\nAdGuard Home did not log the client ID %q.\n", *clientID)
		return exitIgnored
	}
	fmt.Fprintln(stdout, "\nAdGuard Home sees the address xffdns sends.")
	return exitOK
}

func makeQuery(name string) []byte {
	m := make([]byte, 12, 64)
	binary.BigEndian.PutUint16(m, uint16(time.Now().UnixNano()))
	m[2], m[5] = 1, 1 // RD, one question
	for _, l := range strings.Split(strings.TrimSuffix(name, "."), ".") {
		if l == "" || len(l) > 63 {
			continue
		}
		m = append(m, byte(len(l)))
		m = append(m, l...)
	}
	return append(m, 0, 0, 1, 0, 1)
}

func rcodeName(ans []byte) string {
	rc := int(ans[3] & 0xF)
	if n, ok := rcodes[rc]; ok {
		return n
	}
	return fmt.Sprintf("rcode %d", rc)
}

type logEntry struct {
	Client   string `json:"client"`
	ClientID string `json:"client_id"`
	Time     string `json:"time"`
	Question struct {
		Name string `json:"name"`
	} `json:"question"`
}

// findInLog looks for the query in AdGuard Home's query log, for a few
// seconds.
func findInLog(web, user, pass, name string, sent time.Time, up config.Upstream) (*logEntry, error) {
	// The same TLS settings as the DoH client, and never an environment proxy.
	c, err := upstream.HTTPClient(upstream.Options{CAFile: up.CAFile, ServerName: up.ServerName})
	if err != nil {
		return nil, err
	}
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	for try := 0; try < 6; try++ {
		if try > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		req, err := http.NewRequest("GET", strings.TrimSuffix(web, "/")+"/control/querylog?limit=20&search="+url.QueryEscape(name), nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(user, pass)
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		var body struct {
			Data []logEntry `json:"data"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("AdGuard Home refused the user (%s); set XFFDNS_ADGUARD_PASSWORD", resp.Status)
		}
		if resp.StatusCode != http.StatusOK || err != nil {
			return nil, fmt.Errorf("reading the query log: %s %v", resp.Status, err)
		}
		for _, e := range body.Data {
			t, err := time.Parse(time.RFC3339Nano, e.Time)
			if err == nil && !t.Before(sent.Add(-2*time.Second)) && strings.TrimSuffix(strings.ToLower(e.Question.Name), ".") == name {
				return &e, nil
			}
		}
	}
	return nil, errors.New("the query is not in AdGuard Home's query log (is the log on, or the name excluded from it?)")
}
