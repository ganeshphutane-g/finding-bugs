// Package cli is tsdirect's command line.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/ganeshphutane-g/tsdirect/internal/agent"
	"github.com/ganeshphutane-g/tsdirect/internal/config"
	"github.com/ganeshphutane-g/tsdirect/internal/sys"
)

// Version is set at build time.
var Version = "dev"

const (
	exitOK       = 0
	exitError    = 1
	exitUsage    = 2
	exitMismatch = 3
)

// DefaultConfig is where the LANs are configured.
const DefaultConfig = "/etc/tsdirect.conf"

// tailscalePriority is where Tailscale's own rules start on Linux; a rule
// must come before it to win.
const tailscalePriority = 5210

const usage = `tsdirect keeps a Linux machine's LAN traffic on the LAN while Tailscale's
accepted subnet routes cover that LAN, and only while it really is on that
LAN: the network's addresses are directly connected and its fingerprint (the
gateway's MAC, a TCP probe, or both) matches. Then it adds
"ip rule to PREFIX lookup main priority 5200", which comes before Tailscale's
rules; otherwise it removes it.

Usage:
  tsdirect run     [flags]          keep the rules right, until stopped
  tsdirect check   [flags]          one look: each LAN and what run would do
  tsdirect learn   PREFIX [flags]   print this network's "lan" line for the config
  tsdirect cleanup [flags]          remove every rule tsdirect added
  tsdirect version

LANs come from ` + DefaultConfig + ` ("lan" lines) and --lan flags:
  lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56
  lan 10.10.0.0/16 probe 10.10.0.5:22

Run "tsdirect run -h" for the flags.

Exit codes: 0 fine, 1 error, 2 bad usage, 3 (check) the machine is on a
LAN's addresses but the fingerprint does not match.
`

// Deps are what the commands use from outside; tests replace them.
type Deps struct {
	System  func(proto uint8) (agent.System, error)
	Watch   func(ctx context.Context) (<-chan struct{}, error)
	Signals func(chan<- os.Signal)
}

// Main runs tsdirect and returns its exit code.
func Main(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.System == nil {
		deps.System = func(proto uint8) (agent.System, error) {
			return &sys.Linux{Proto: proto, Skip: sys.IsTailscale}, nil
		}
	}
	if deps.Watch == nil {
		deps.Watch = (&sys.Linux{}).Watch
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
		fmt.Fprintln(stdout, "tsdirect", version())
		return exitOK
	case "run", "check":
		return command(args[0], args[1:], stdout, stderr, deps)
	case "learn":
		return learn(args[1:], stdout, stderr, deps)
	case "cleanup":
		return cleanup(args[1:], stdout, stderr, deps)
	}
	fmt.Fprintf(stderr, "tsdirect: unknown command %q\n\n%s", args[0], usage)
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

// protoFlag is --rule-proto: the FRA_PROTOCOL value that marks tsdirect's
// rules.
type protoFlag uint8

func (p *protoFlag) String() string { return fmt.Sprintf("0x%x", uint8(*p)) }
func (p *protoFlag) Set(v string) error {
	n, err := strconv.ParseUint(v, 0, 8)
	if err != nil {
		return errors.New("a number from 5 to 255, such as 0xd1")
	}
	// 0-4 are the kernel's and iproute2's own (unspec, redirect, kernel,
	// boot, static): cleanup would remove rules that are not tsdirect's.
	if n < 5 {
		return errors.New("0 to 4 are used by the kernel and ip itself; pick 5 to 255")
	}
	*p = protoFlag(n)
	return nil
}

const defaultProto = 0xd1

func newFlags(name string, stderr io.Writer, args string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: tsdirect %s%s\n\nFlags:\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}

type options struct {
	config, listen string
	lans           list
	proto          protoFlag
	priority       int
	interval       time.Duration
}

// load reads the LANs: --lan flags, then the config file.
func (o *options) load() ([]config.LAN, error) {
	var lans []config.LAN
	for _, s := range o.lans {
		l, err := config.Parse(s, "--lan")
		if err != nil {
			return nil, err
		}
		lans = append(lans, l)
	}
	if o.config != "" {
		fromFile, err := config.ReadFile(o.config)
		if err != nil {
			return nil, err
		}
		lans = append(lans, fromFile...)
	}
	if err := config.Check(lans); err != nil {
		return nil, err
	}
	if len(lans) == 0 {
		return nil, fmt.Errorf("no LANs: add \"lan\" lines to %s or give --lan (tsdirect learn PREFIX prints one)", o.config)
	}
	return lans, nil
}

func command(name string, args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := newFlags(name, stderr, " [flags]")
	o := options{proto: defaultProto}
	fs.StringVar(&o.config, "config", DefaultConfig, "the config file (\"\" for none; a missing file is no LANs)")
	fs.Var(&o.lans, "lan", `a LAN, as in the config: "PREFIX gateway-mac MAC" and/or "probe HOST:PORT" (repeat)`)
	fs.Var(&o.proto, "rule-proto", "the protocol value that marks tsdirect's rules")
	fs.IntVar(&o.priority, "priority", 5200, "the rules' priority (before Tailscale's, which start at 5210)")
	if name == "run" {
		fs.DurationVar(&o.interval, "interval", 15*time.Second, "how often to look, besides on network changes")
		fs.StringVar(&o.listen, "listen", "", "serve /metrics and /healthz here, e.g. 127.0.0.1:9587 (default: off)")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tsdirect: unexpected %q\n", fs.Arg(0))
		return exitUsage
	}
	if o.priority < 1 || o.priority > 32765 {
		fmt.Fprintf(stderr, "tsdirect: --priority %d: from 1 to 32765 (32766 is the main table's own rule)\n", o.priority)
		return exitUsage
	}
	if name == "run" && o.interval < time.Second {
		fmt.Fprintf(stderr, "tsdirect: --interval %s: at least 1s\n", o.interval)
		return exitUsage
	}
	if o.priority >= tailscalePriority {
		fmt.Fprintf(stderr, "tsdirect: warning: --priority %d comes after Tailscale's rules (from %d): its routes still win\n", o.priority, tailscalePriority)
	}
	lans, err := o.load()
	if err != nil {
		fmt.Fprintln(stderr, "tsdirect:", err)
		return exitError
	}
	// Signals first: a reload (SIGHUP) while starting must not kill it.
	var sig chan os.Signal
	if name == "run" {
		sig = make(chan os.Signal, 4)
		deps.Signals(sig)
	}
	s, err := deps.System(uint8(o.proto))
	if err != nil {
		fmt.Fprintln(stderr, "tsdirect:", err)
		return exitError
	}
	a := &agent.Agent{Sys: s, LANs: lans, Priority: o.priority, DryRun: name == "check"}
	if name == "check" {
		return check(a, stdout, stderr)
	}
	return run(a, o, stdout, stderr, deps, sig)
}

func check(a *agent.Agent, stdout, stderr io.Writer) int {
	code := exitOK
	if err := a.Reconcile(context.Background()); err != nil {
		fmt.Fprintln(stderr, "tsdirect:", err)
		code = exitError
	}
	st := a.Statuses()
	if len(st) == 0 {
		return code
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "LAN\tINTERFACE\tSTATE\tRULE\tSOURCE")
	for _, s := range st {
		iface, state := s.Observation.Iface, "not here"
		if iface == "" {
			iface = "-"
		}
		switch {
		case s.Verdict.On:
			state = "direct"
		case s.Observation.Iface != "":
			state = "not confirmed"
			if code == exitOK {
				code = exitMismatch
			}
		}
		rule := "no"
		if s.Rule {
			rule = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.LAN.Prefix, iface, state, rule, s.LAN.Source)
	}
	tw.Flush()
	fmt.Fprintln(stdout)
	for _, s := range st {
		fmt.Fprintf(stdout, "%s: %s", s.LAN.Prefix, s.Verdict.Reason)
		switch {
		case s.Verdict.On && !s.Rule:
			fmt.Fprint(stdout, " [run would add the rule]")
		case !s.Verdict.On && s.Rule:
			fmt.Fprint(stdout, " [run would remove the rule]")
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

func run(a *agent.Agent, o options, stdout, stderr io.Writer, deps Deps, sig chan os.Signal) int {
	if os.Getenv("JOURNAL_STREAM") == "" {
		stdout = stampWriter{stdout, time.Now}
	}
	a.Log = stdout
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := deps.Watch(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "tsdirect: watching for network changes:", err)
		return exitError
	}
	if o.listen != "" {
		ln, err := net.Listen("tcp", o.listen)
		if err != nil {
			fmt.Fprintln(stderr, "tsdirect:", err)
			return exitError
		}
		srv := &http.Server{Handler: handler(a, o.interval), ReadHeaderTimeout: 5 * time.Second}
		go srv.Serve(ln)
		defer srv.Close()
	}
	n := len(a.LANs)
	fmt.Fprintf(stdout, "tsdirect %s: %d %s, rules at priority %d marked protocol %s, looking every %s and on network changes\n",
		version(), n, plural(n, "LAN", "LANs"), o.priority, o.proto.String(), o.interval)
	// A reload is a pass of its own, so it rides on the event channel.
	merged := make(chan struct{}, 1)
	poke := func() {
		select {
		case merged <- struct{}{}:
		default:
		}
	}
	go func() {
		for range events {
			poke()
		}
	}()
	go func() {
		for s := range sig {
			if s == syscall.SIGHUP {
				lans, err := o.load()
				if err != nil {
					fmt.Fprintf(stdout, "reload: %v (keeping the LANs as they were)\n", err)
					continue
				}
				a.SetLANs(lans)
				fmt.Fprintf(stdout, "reload: %d %s\n", len(lans), plural(len(lans), "LAN", "LANs"))
				poke()
				continue
			}
			cancel()
			return
		}
	}()
	a.Run(ctx, o.interval, merged)
	removed, err := a.Cleanup()
	fmt.Fprintf(stdout, "stopping: removed %d %s\n", removed, plural(removed, "rule", "rules"))
	if err != nil {
		fmt.Fprintln(stderr, "tsdirect:", err)
		return exitError
	}
	return exitOK
}

func learn(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := newFlags("learn", stderr, " PREFIX [flags]")
	var gateway, probe string
	fs.StringVar(&gateway, "gateway", "", "ask this address for its MAC instead of the default gateway (an address on the LAN that is always there)")
	fs.StringVar(&probe, "probe", "", "also require a TCP connection to HOST:PORT on the LAN; it is tried now")
	var prefix string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		prefix, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if prefix == "" && fs.NArg() == 1 {
		prefix = fs.Arg(0)
	} else if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tsdirect: unexpected %q\n", fs.Arg(0))
		return exitUsage
	}
	if prefix == "" {
		fs.Usage()
		return exitUsage
	}
	p, err := netip.ParsePrefix(prefix)
	if err != nil || p.Masked() != p || p.Bits() == 0 {
		fmt.Fprintf(stderr, "tsdirect: %q is not a network prefix such as 192.168.1.0/24\n", prefix)
		return exitUsage
	}
	s, err := deps.System(defaultProto)
	if err != nil {
		fmt.Fprintln(stderr, "tsdirect:", err)
		return exitError
	}
	iface, index, connected, err := s.Connected(p)
	if err != nil {
		fmt.Fprintln(stderr, "tsdirect:", err)
		return exitError
	}
	if iface == "" {
		fmt.Fprintf(stderr, "tsdirect: this machine is not on %s now (no interface other than Tailscale's has it as a connected network); run learn while on the LAN\n", p)
		return exitError
	}
	if connected.Bits() > p.Bits() {
		fmt.Fprintf(stderr, "tsdirect: %s is only partly on %s (%s is); learn %s instead\n", p, iface, connected, connected)
		return exitError
	}
	line := "lan " + p.String()
	var gw netip.Addr
	if gateway != "" {
		if gw, err = netip.ParseAddr(gateway); err != nil || !p.Contains(gw) {
			fmt.Fprintf(stderr, "tsdirect: --gateway %q: an address in %s\n", gateway, p)
			return exitUsage
		}
		line += " gateway " + gw.String()
	} else {
		var ok bool
		if gw, ok = s.DefaultGateway(index, p.Addr().Is6()); !ok {
			fmt.Fprintf(stderr, "tsdirect: %s has no default gateway; give --gateway ADDR (an address on the LAN that is always there) or --probe HOST:PORT\n", iface)
			if probe == "" {
				return exitError
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if gw.IsValid() {
		mac, err := s.GatewayMAC(ctx, iface, gw)
		if err != nil {
			fmt.Fprintf(stderr, "tsdirect: asking %s on %s for its MAC: %v\n", gw, iface, err)
			return exitError
		}
		line += " gateway-mac " + mac.String()
		fmt.Fprintf(stderr, "%s is on %s; its gateway %s is %s\n", p, iface, gw, mac)
	}
	if probe != "" {
		ap, err := netip.ParseAddrPort(probe)
		if err != nil || !p.Contains(ap.Addr()) || ap.Port() == 0 {
			fmt.Fprintf(stderr, "tsdirect: --probe %q: address:port with the address in %s\n", probe, p)
			return exitUsage
		}
		if err := s.Probe(ctx, iface, ap); err != nil {
			fmt.Fprintf(stderr, "tsdirect: the probe %s from %s failed: %v\n", ap, iface, err)
			return exitError
		}
		line += " probe " + ap.String()
		fmt.Fprintf(stderr, "the probe %s answers from %s\n", ap, iface)
	}
	if _, err := config.Parse(line, "learn"); err != nil {
		fmt.Fprintln(stderr, "tsdirect:", err)
		return exitError
	}
	fmt.Fprintln(stdout, line)
	return exitOK
}

func cleanup(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := newFlags("cleanup", stderr, " [flags]")
	proto := protoFlag(defaultProto)
	fs.Var(&proto, "rule-proto", "the protocol value that marks tsdirect's rules")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tsdirect: unexpected %q\n", fs.Arg(0))
		return exitUsage
	}
	s, err := deps.System(uint8(proto))
	if err != nil {
		fmt.Fprintln(stderr, "tsdirect:", err)
		return exitError
	}
	a := &agent.Agent{Sys: s}
	n, err := a.Cleanup()
	fmt.Fprintf(stdout, "removed %d %s\n", n, plural(n, "rule", "rules"))
	if err != nil {
		fmt.Fprintln(stderr, "tsdirect:", err)
		return exitError
	}
	return exitOK
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
