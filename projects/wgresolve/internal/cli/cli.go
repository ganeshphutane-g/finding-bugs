// Package cli is wgresolve's command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ganeshphutane-g/wgresolve/internal/device"
	"github.com/ganeshphutane-g/wgresolve/internal/engine"
	"github.com/ganeshphutane-g/wgresolve/internal/resolve"
	"github.com/ganeshphutane-g/wgresolve/internal/source"
	"github.com/ganeshphutane-g/wgresolve/internal/watch"
)

// Version is set at build time.
var Version = "dev"

const (
	exitOK        = 0
	exitError     = 1
	exitUsage     = 2
	exitAttention = 3
)

const usage = `wgresolve keeps WireGuard peers whose endpoint is a DNS name connected when
the address behind the name changes: a peer that has gone quiet is pointed at
what its name resolves to now. A working peer is never touched.

Usage:
  wgresolve run   [flags]   watch and fix, until stopped
  wgresolve check [flags]   one pass: show every peer and what run would do
  wgresolve version

Peers come from wg-quick confs, systemd-networkd .netdev files and
NetworkManager keyfiles, and from --peer flags.

Run "wgresolve run -h" for the flags.

Exit codes: 0 fine, 1 error, 2 bad usage, 3 (check) a peer needs a change or
its name does not resolve.
`

// Deps are what the commands use from outside; tests replace them.
type Deps struct {
	OpenDevices func() (device.Devices, error)
	Signals     func(chan<- os.Signal)
}

// Main runs wgresolve and returns its exit code.
func Main(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.OpenDevices == nil {
		deps.OpenDevices = func() (device.Devices, error) { return device.Open() }
	}
	if deps.Signals == nil {
		deps.Signals = func(c chan<- os.Signal) { signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP) }
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return exitOK
	case "version", "--version":
		fmt.Fprintln(stdout, "wgresolve", version())
		return exitOK
	case "run", "check":
		return command(args[0], args[1:], stdout, stderr, deps)
	}
	fmt.Fprintf(stderr, "wgresolve: unknown command %q\n\n%s", args[0], usage)
	return exitUsage
}

func version() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return Version
}

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

type options struct {
	wgQuick, resolvConf, prefer, family, listen string
	networkd, nm, peers, resolvers              list
	noFiles                                     bool
	interval, stale, timeout, minTTL            time.Duration
}

func command(name string, args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var o options
	fs.StringVar(&o.wgQuick, "wg-quick-dir", "/etc/wireguard", "where wg-quick confs are (\"\" to skip)")
	fs.Var(&o.networkd, "networkd-dir", "a systemd-networkd directory (repeat; default: networkd's own list)")
	fs.Var(&o.nm, "nm-dir", "a NetworkManager keyfile directory (repeat; default: NetworkManager's own)")
	fs.BoolVar(&o.noFiles, "no-files", false, "read no config files; use only --peer")
	fs.Var(&o.peers, "peer", "IFACE=PUBLICKEY=host:port, a peer to watch (repeat)")
	fs.Var(&o.resolvers, "resolver", "a DNS server to ask, host[:port] (repeat; default: those in --resolv-conf)")
	fs.StringVar(&o.resolvConf, "resolv-conf", "/etc/resolv.conf", "where the system's DNS servers are listed")
	fs.StringVar(&o.prefer, "prefer", "ipv4", `which addresses to try first: "ipv4" or "ipv6"`)
	fs.StringVar(&o.family, "family", "", `only "ipv4" or "ipv6" addresses (default: both)`)
	fs.DurationVar(&o.interval, "interval", 30*time.Second, "how often to look at the peers")
	fs.DurationVar(&o.stale, "stale", engine.DefaultStale, "how old a handshake may be before the peer counts as not working")
	fs.DurationVar(&o.timeout, "dns-timeout", 3*time.Second, "how long to wait for each DNS server")
	fs.DurationVar(&o.minTTL, "min-ttl", resolve.MinTTL, "keep an answer at least this long, whatever its TTL")
	if name == "run" {
		fs.StringVar(&o.listen, "listen", "", "serve /metrics and /healthz here, e.g. 127.0.0.1:9586 (default: off)")
	}
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: wgresolve %s [flags]\n\nFlags:\n", name)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "wgresolve: unexpected %q\n", fs.Arg(0))
		return exitUsage
	}
	if err := o.validate(); err != nil {
		fmt.Fprintln(stderr, "wgresolve:", err)
		return exitUsage
	}
	// Flags are checked before anything else, so a typo is a usage error.
	var flagPeers source.Result
	for _, p := range o.peers {
		if err := source.Flag(p, &flagPeers); err != nil {
			fmt.Fprintln(stderr, "wgresolve:", err)
			return exitUsage
		}
	}
	res, err := o.resolver()
	if err != nil {
		fmt.Fprintln(stderr, "wgresolve:", err)
		return exitError
	}
	dev, err := deps.OpenDevices()
	if err != nil {
		fmt.Fprintln(stderr, "wgresolve:", err)
		return exitError
	}
	defer dev.Close()
	w := &watch.Watcher{Load: o.loader(), Resolver: res, Devices: dev, Stale: o.stale, DryRun: name == "check"}
	if err := w.Reload(); err != nil {
		fmt.Fprintln(stderr, "wgresolve:", err)
		return exitError
	}
	if name == "check" {
		return check(w, stdout)
	}
	return run(w, o, stdout, stderr, deps)
}

func (o *options) validate() error {
	if o.prefer != "ipv4" && o.prefer != "ipv6" {
		return fmt.Errorf("--prefer %q: use ipv4 or ipv6", o.prefer)
	}
	if o.family != "" && o.family != "ipv4" && o.family != "ipv6" {
		return fmt.Errorf("--family %q: use ipv4 or ipv6", o.family)
	}
	if o.interval < time.Second {
		return fmt.Errorf("--interval %s: at least 1s", o.interval)
	}
	if o.stale < 10*time.Second {
		return fmt.Errorf("--stale %s: at least 10s (WireGuard retries a handshake every 5 s)", o.stale)
	}
	if o.minTTL < time.Second || o.minTTL > resolve.MaxTTL {
		return fmt.Errorf("--min-ttl %s: between 1s and 1h", o.minTTL)
	}
	if o.timeout <= 0 {
		return fmt.Errorf("--dns-timeout %s: above zero", o.timeout)
	}
	if o.noFiles && len(o.peers) == 0 {
		return errors.New("--no-files needs at least one --peer")
	}
	return nil
}

func (o *options) resolver() (*resolve.Resolver, error) {
	var r *resolve.Resolver
	if len(o.resolvers) > 0 {
		r = &resolve.Resolver{}
		for _, s := range o.resolvers {
			if _, _, err := net.SplitHostPort(s); err != nil {
				s = net.JoinHostPort(strings.Trim(s, "[]"), "53")
			}
			r.Servers = append(r.Servers, s)
		}
		// Search domains still come from resolv.conf when it can be read.
		if rc, err := resolve.FromResolvConf(o.resolvConf); err == nil {
			r.Search = rc.Search
		}
	} else {
		var err error
		if r, err = resolve.FromResolvConf(o.resolvConf); err != nil {
			return nil, fmt.Errorf("%v (or give --resolver)", err)
		}
	}
	r.Prefer, r.Family, r.Timeout, r.MinTTL = o.prefer, o.family, o.timeout, o.minTTL
	return r, nil
}

func (o *options) loader() watch.Load {
	return func() (*source.Result, error) {
		r := &source.Result{}
		for _, p := range o.peers {
			if err := source.Flag(p, r); err != nil {
				return nil, err
			}
		}
		if o.noFiles {
			return r, nil
		}
		if o.wgQuick != "" {
			if err := source.WGQuick(o.wgQuick, r); err != nil {
				return nil, err
			}
		}
		nd := []string(o.networkd)
		if len(nd) == 0 {
			nd = source.NetworkdDirs
		}
		if err := source.Networkd(nd, r); err != nil {
			return nil, err
		}
		nm := []string(o.nm)
		if len(nm) == 0 {
			nm = source.NMDirs
		}
		if err := source.NetworkManager(nm, r); err != nil {
			return nil, err
		}
		return r, nil
	}
}

func check(w *watch.Watcher, stdout io.Writer) int {
	w.Round(context.Background())
	for _, p := range w.Problems() {
		fmt.Fprintf(stdout, "note: %s\n", p)
	}
	st := w.Statuses()
	if len(st) == 0 {
		fmt.Fprintln(stdout, "No WireGuard peers with a DNS name for their endpoint were found.")
		return exitOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "INTERFACE\tPEER\tNAME\tENDPOINT\tHANDSHAKE\tSTATE")
	code := exitOK
	for _, s := range st {
		ep := "none"
		if s.Endpoint.IsValid() {
			ep = s.Endpoint.String()
		}
		hs := "never"
		if !s.Handshake.IsZero() {
			hs = time.Since(s.Handshake).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Peer.Interface, s.Peer.PublicKey.String()[:8]+"…", s.Peer.Endpoint(), ep, hs, s.State)
		if s.State == "would change" || s.State == "dns error" {
			code = exitAttention
		}
	}
	tw.Flush()
	fmt.Fprintln(stdout)
	for _, s := range st {
		fmt.Fprintf(stdout, "%s %s (%s): %s", s.Peer.Interface, s.Peer.PublicKey.String()[:8]+"…", s.Peer.Source, s.Reason)
		if len(s.Addrs) > 0 {
			var a []string
			for _, x := range s.Addrs {
				a = append(a, x.String())
			}
			fmt.Fprintf(stdout, " [%s resolves to %s]", s.Peer.Host, strings.Join(a, ", "))
		}
		fmt.Fprintln(stdout)
	}
	return code
}

// stampWriter starts each line with the time, for logs that are not
// journald's (which stamps them itself).
type stampWriter struct {
	w   io.Writer
	now func() time.Time
}

func (s stampWriter) Write(b []byte) (int, error) {
	_, err := fmt.Fprintf(s.w, "%s %s", s.now().Format("2006-01-02T15:04:05Z07:00"), b)
	return len(b), err
}

func run(w *watch.Watcher, o options, stdout, stderr io.Writer, deps Deps) int {
	if os.Getenv("JOURNAL_STREAM") == "" {
		stdout = stampWriter{stdout, time.Now}
	}
	w.Log = stdout
	for _, p := range w.Problems() {
		fmt.Fprintf(stdout, "note: %s\n", p)
	}
	fmt.Fprintf(stdout, "wgresolve %s: watching %d peers with a name for their endpoint, every %s\n", version(), len(w.Peers()), o.interval)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if o.listen != "" {
		ln, err := net.Listen("tcp", o.listen)
		if err != nil {
			fmt.Fprintln(stderr, "wgresolve:", err)
			return exitError
		}
		srv := &http.Server{Handler: handler(w, o.interval), ReadHeaderTimeout: 5 * time.Second}
		go srv.Serve(ln)
		defer srv.Close()
	}
	sig := make(chan os.Signal, 4)
	deps.Signals(sig)
	reload := make(chan struct{}, 1)
	go func() {
		for s := range sig {
			if s == syscall.SIGHUP {
				select {
				case reload <- struct{}{}:
				default:
				}
				continue
			}
			cancel()
			return
		}
	}()
	w.Run(ctx, o.interval, 5*time.Minute, reload)
	return exitOK
}
