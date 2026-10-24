// Package cli is tsroam's command line.
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
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ganeshphutane-g/tsroam/internal/agent"
	"github.com/ganeshphutane-g/tsroam/internal/config"
	"github.com/ganeshphutane-g/tsroam/internal/killswitch"
	"github.com/ganeshphutane-g/tsroam/internal/policy"
	"github.com/ganeshphutane-g/tsroam/internal/sys"
	"github.com/ganeshphutane-g/tsroam/internal/tailscale"
)

// Version is set at build time.
var Version = "dev"

const (
	exitOK        = 0
	exitError     = 1
	exitUsage     = 2
	exitUntrusted = 4
)

// DefaultConfig is where tsroam is configured.
const DefaultConfig = "/etc/tsroam.conf"

const usage = `tsroam turns a Tailscale exit node on whenever this Linux machine is not on
one of its trusted networks, and off on them. A trusted network is told by
its addresses and a fingerprint: the gateway's MAC, a TCP probe, or both, so
a café on the same 192.168.1.0/24 is not taken for home. With the kill
switch on, nothing but tailnet traffic (and DHCP) leaves the machine while
the exit node is meant to be on, even if tailscaled stops or crashes.

Usage:
  tsroam run     [flags]           keep it so, until stopped
  tsroam check   [flags]           one look: where the machine is, what run would do
  tsroam learn   [PREFIX] [flags]  print this network's "trusted" line for the config
  tsroam off     [--for 10m]       stand down until the network changes (a captive portal)
  tsroam cleanup [flags]           remove the kill switch and any pause
  tsroam version

` + DefaultConfig + `:
  exit-node 100.64.0.1      # an IP or a node name, as tailscale set --exit-node takes
  kill-switch on            # default off
  allow-lan off             # on: the local network stays reachable on untrusted networks
  trusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56
  trusted 10.10.0.0/16 probe 10.10.0.5:22

Run "tsroam run -h" for the flags.

Exit codes: 0 fine, 1 error (check: also when tsroam could not do all it
wanted), 2 bad usage, 4 (check) on an untrusted network.
`

// Deps are what the commands use from outside; tests replace them.
type Deps struct {
	System  func() policy.System
	Tailnet func(bin, socket string) agent.Tailnet
	Wall    func() agent.Wall
	Watch   func(ctx context.Context) (<-chan struct{}, error)
	Signals func(chan<- os.Signal)
}

// Main runs tsroam and returns its exit code.
func Main(args []string, stdout, stderr io.Writer, deps Deps) int {
	if deps.System == nil {
		deps.System = func() policy.System { return &sys.Linux{Skip: sys.IsTailscale} }
	}
	if deps.Tailnet == nil {
		deps.Tailnet = func(bin, socket string) agent.Tailnet { return &tailscale.CLI{Bin: bin, Socket: socket} }
	}
	if deps.Wall == nil {
		deps.Wall = func() agent.Wall { return &killswitch.Switch{} }
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
		fmt.Fprintln(stdout, "tsroam", version())
		return exitOK
	case "run", "check", "off", "cleanup":
		return command(args[0], args[1:], stdout, stderr, deps)
	case "learn":
		return learn(args[1:], stdout, stderr, deps)
	}
	fmt.Fprintf(stderr, "tsroam: unknown command %q\n\n%s", args[0], usage)
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

func newFlags(name string, stderr io.Writer, args string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: tsroam %s%s\n\nFlags:\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}

// markFlag is VALUE/MASK, as Tailscale's fwmark is written (0x80000/0xff0000).
type markFlag struct{ mark, mask uint32 }

func (m *markFlag) String() string { return fmt.Sprintf("%#x/%#x", m.mark, m.mask) }
func (m *markFlag) Set(v string) error {
	a, b, ok := strings.Cut(v, "/")
	mark, err1 := strconv.ParseUint(a, 0, 32)
	mask := uint64(0xffffffff)
	var err2 error
	if ok {
		mask, err2 = strconv.ParseUint(b, 0, 32)
	}
	if err1 != nil || err2 != nil || mask == 0 || mark&^mask != 0 {
		return errors.New("VALUE/MASK such as 0x80000/0xff0000, with the value inside the mask")
	}
	m.mark, m.mask = uint32(mark), uint32(mask)
	return nil
}

type options struct {
	config, tailscale, socket, tun, stateDir, listen, exitNode string
	mark                                                       markFlag
	interval, pauseFor                                         time.Duration
}

func (o *options) pauseFile() string { return filepath.Join(o.stateDir, "pause") }

func (o *options) load() (config.Config, error) {
	c, err := config.ReadFile(o.config)
	if err != nil {
		return config.Config{}, err
	}
	if o.exitNode != "" {
		if err := config.CheckExitNode(o.exitNode); err != nil {
			return config.Config{}, err
		}
		c.ExitNode = o.exitNode
	}
	if c.ExitNode == "" {
		return config.Config{}, fmt.Errorf("no exit node: add an \"exit-node\" line to %s or give --exit-node", o.config)
	}
	return c, nil
}

func command(name string, args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := newFlags(name, stderr, " [flags]")
	o := options{mark: markFlag{0x80000, 0xff0000}}
	fs.StringVar(&o.config, "config", DefaultConfig, "the config file")
	fs.StringVar(&o.stateDir, "state-dir", "/run/tsroam", "where the pause made by \"tsroam off\" is kept")
	if name != "cleanup" {
		fs.StringVar(&o.tailscale, "tailscale", "tailscale", "the tailscale command")
		fs.StringVar(&o.socket, "socket", "", "tailscaled's socket (default: the tailscale command's)")
	}
	if name == "run" || name == "check" {
		fs.StringVar(&o.exitNode, "exit-node", "", "the exit node (default: the config's)")
		fs.StringVar(&o.tun, "tun", "tailscale0", "Tailscale's interface")
		fs.Var(&o.mark, "ts-mark", "the firewall mark tailscaled puts on its own packets, VALUE/MASK")
	}
	if name == "run" {
		fs.DurationVar(&o.interval, "interval", 15*time.Second, "how often to look, besides on network changes")
		fs.StringVar(&o.listen, "listen", "", "serve /metrics and /healthz here, e.g. 127.0.0.1:9588 (default: off)")
	}
	if name == "off" {
		fs.DurationVar(&o.pauseFor, "for", 10*time.Minute, "the longest the pause lasts (it also ends when the network changes)")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "tsroam: unexpected %q\n", fs.Arg(0))
		return exitUsage
	}
	if name == "run" && o.interval < time.Second {
		fmt.Fprintf(stderr, "tsroam: --interval %s: at least 1s\n", o.interval)
		return exitUsage
	}
	if name == "off" && (o.pauseFor < time.Minute || o.pauseFor > 24*time.Hour) {
		fmt.Fprintf(stderr, "tsroam: --for %s: from 1m to 24h\n", o.pauseFor)
		return exitUsage
	}
	if (name == "run" || name == "check") && (o.tun == "" || len(o.tun) > 15) {
		fmt.Fprintf(stderr, "tsroam: --tun %q is not an interface name\n", o.tun)
		return exitUsage
	}
	switch name {
	case "cleanup":
		return cleanup(o, stdout, stderr, deps)
	case "off":
		return off(o, stdout, stderr, deps)
	}
	c, err := o.load()
	if err != nil {
		fmt.Fprintln(stderr, "tsroam:", err)
		return exitError
	}
	a := &agent.Agent{
		Sys: deps.System(), TS: deps.Tailnet(o.tailscale, o.socket), Wall: deps.Wall(), Config: c,
		Rules:     killswitch.Rules{Tun: o.tun, Mark: o.mark.mark, Mask: o.mark.mask},
		PauseFile: o.pauseFile(), DryRun: name == "check",
	}
	if name == "check" {
		return check(a, stdout)
	}
	return run(a, o, stdout, stderr, deps)
}

func check(a *agent.Agent, stdout io.Writer) int {
	st := a.Pass(context.Background())
	p := st.Place
	fmt.Fprintf(stdout, "network:     %s", p.Kind)
	if p.Iface != "" {
		fmt.Fprintf(stdout, " (default route on %s", p.Iface)
		if p.Gateway.IsValid() {
			fmt.Fprintf(stdout, " via %s", p.Gateway)
		}
		fmt.Fprint(stdout, ")")
	}
	fmt.Fprintf(stdout, "\n             %s\n", p.Reason)
	for i, v := range p.Verdicts {
		fmt.Fprintf(stdout, "trusted:     %s (%s): %s\n", a.Config.Trusted[i].Prefix, a.Config.Trusted[i].Source, v.Reason)
	}
	want := map[policy.Kind]string{
		policy.Offline:   "nothing changes while offline",
		policy.Trusted:   "exit node off on arrival, kill switch off",
		policy.Untrusted: "exit node " + a.Config.ExitNode + " on",
	}[p.Kind]
	if p.Kind == policy.Untrusted && a.Config.KillSwitch {
		want += ", kill switch on"
	}
	if st.Paused {
		want = "nothing: paused by tsroam off"
	}
	fmt.Fprintf(stdout, "exit node:   %s\n", onOff(st.ExitNode, a.Config.ExitNode+" in use", "not "+a.Config.ExitNode))
	fmt.Fprintf(stdout, "kill switch: %s\n", onOff(st.Wall, "on", "off"))
	fmt.Fprintf(stdout, "run would:   %s\n", want)
	if st.Note != "" {
		fmt.Fprintf(stdout, "note:        %s\n", st.Note)
	}
	switch {
	case p.Err != nil, st.Note != "":
		return exitError
	case p.Kind == policy.Untrusted:
		return exitUntrusted
	}
	return exitOK
}

func onOff(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
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

func run(a *agent.Agent, o options, stdout, stderr io.Writer, deps Deps) int {
	if os.Getenv("JOURNAL_STREAM") == "" {
		stdout = stampWriter{stdout, time.Now}
	}
	a.Log = stdout
	sig := make(chan os.Signal, 4)
	deps.Signals(sig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := deps.Watch(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "tsroam: watching for network changes:", err)
		return exitError
	}
	if o.listen != "" {
		ln, err := net.Listen("tcp", o.listen)
		if err != nil {
			fmt.Fprintln(stderr, "tsroam:", err)
			return exitError
		}
		srv := &http.Server{Handler: handler(a, o.interval), ReadHeaderTimeout: 5 * time.Second}
		go srv.Serve(ln)
		defer srv.Close()
	}
	n := len(a.Config.Trusted)
	fmt.Fprintf(stdout, "tsroam %s: exit node %s except on %d trusted %s, kill switch %s, looking every %s and on network changes\n",
		version(), a.Config.ExitNode, n, plural(n, "network", "networks"), onOff(a.Config.KillSwitch, "on", "off"), o.interval)
	merged := make(chan struct{}, 1)
	poke := func() {
		select {
		case merged <- struct{}{}:
		default:
		}
	}
	// Without events a move to another network would only be seen at the
	// next interval: stop and let systemd restart it. The kill switch
	// stays (an error exit is not a clean stop).
	var lost atomic.Bool
	go func() {
		for range events {
			poke()
		}
		if ctx.Err() == nil {
			lost.Store(true)
			fmt.Fprintln(stderr, "tsroam: network change events stopped (the kernel dropped some); stopping")
			cancel()
		}
	}()
	go func() {
		for s := range sig {
			if s == syscall.SIGHUP {
				c, err := o.load()
				if err != nil {
					fmt.Fprintf(stdout, "reload: %v (keeping the config as it was)\n", err)
					continue
				}
				a.SetConfig(c)
				fmt.Fprintf(stdout, "reload: exit node %s, %d trusted %s\n", c.ExitNode, len(c.Trusted), plural(len(c.Trusted), "network", "networks"))
				poke()
				continue
			}
			cancel()
			return
		}
	}()
	a.Run(ctx, o.interval, merged)
	if lost.Load() {
		return exitError
	}
	if err := a.Stop(); err != nil {
		fmt.Fprintln(stderr, "tsroam:", err)
		return exitError
	}
	fmt.Fprintln(stdout, "stopped")
	return exitOK
}

// off stands down on this network: the kill switch goes, the exit node is
// turned off, and run forces neither until the network changes or the
// pause runs out.
func off(o options, stdout, stderr io.Writer, deps Deps) int {
	c, _ := config.ReadFile(o.config) // the trusted networks, for the network's key
	p := policy.Look(context.Background(), deps.System(), c.Trusted)
	until := time.Now().Add(o.pauseFor)
	if err := agent.WritePause(o.pauseFile(), agent.Pause{Key: p.Key(), Until: until}); err != nil {
		fmt.Fprintln(stderr, "tsroam:", err)
		return exitError
	}
	code := exitOK
	if err := deps.Wall().Off(); err != nil {
		fmt.Fprintln(stderr, "tsroam:", err)
		code = exitError
	}
	if err := deps.Tailnet(o.tailscale, o.socket).SetExitNode(context.Background(), "", false); err != nil {
		fmt.Fprintln(stderr, "tsroam:", err)
		code = exitError
	}
	fmt.Fprintf(stdout, "kill switch and exit node off on this network (%s), until it changes or %s\n", p.Kind, until.Format("15:04"))
	return code
}

func cleanup(o options, stdout, stderr io.Writer, deps Deps) int {
	code := exitOK
	if err := deps.Wall().Off(); err != nil {
		fmt.Fprintln(stderr, "tsroam:", err)
		code = exitError
	}
	if err := os.Remove(o.pauseFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stderr, "tsroam:", err)
		code = exitError
	}
	if code == exitOK {
		fmt.Fprintln(stdout, "kill switch removed (if it was there); the exit node is left as it is")
	}
	return code
}

func learn(args []string, stdout, stderr io.Writer, deps Deps) int {
	fs := newFlags("learn", stderr, " [PREFIX] [flags]")
	var gateway, probe string
	fs.StringVar(&gateway, "gateway", "", "ask this address for its MAC instead of the default gateway")
	fs.StringVar(&probe, "probe", "", "also require a TCP connection to ADDR:PORT on the network; it is tried now")
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
		fmt.Fprintf(stderr, "tsroam: unexpected %q\n", fs.Arg(0))
		return exitUsage
	}
	s := deps.System()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var p netip.Prefix
	if prefix == "" {
		// The network the default route is on.
		iface, _, gw, ok, err := s.DefaultRoute()
		if err != nil || !ok || iface == "" || !gw.IsValid() {
			fmt.Fprintln(stderr, "tsroam: no default route through a gateway to learn from; give the network's PREFIX")
			return exitError
		}
		_, _, c, err := s.Connected(netip.PrefixFrom(gw, 32))
		if err != nil || !c.IsValid() {
			fmt.Fprintf(stderr, "tsroam: the gateway %s is not on a connected network; give the network's PREFIX\n", gw)
			return exitError
		}
		p = c
	} else {
		var err error
		p, err = netip.ParsePrefix(prefix)
		if err != nil || p.Masked() != p || p.Bits() == 0 || !p.Addr().Is4() {
			fmt.Fprintf(stderr, "tsroam: %q is not an IPv4 network prefix such as 192.168.1.0/24\n", prefix)
			return exitUsage
		}
	}
	var ap netip.AddrPort
	if probe != "" {
		var err error
		ap, err = netip.ParseAddrPort(probe)
		if err != nil || !p.Contains(ap.Addr()) || ap.Port() == 0 {
			fmt.Fprintf(stderr, "tsroam: --probe %q: address:port with the address in %s\n", probe, p)
			return exitUsage
		}
	}
	var gw netip.Addr
	if gateway != "" {
		var err error
		if gw, err = netip.ParseAddr(gateway); err != nil || !p.Contains(gw) {
			fmt.Fprintf(stderr, "tsroam: --gateway %q: an address in %s\n", gateway, p)
			return exitUsage
		}
	}
	iface, index, connected, err := s.Connected(p)
	if err != nil {
		fmt.Fprintln(stderr, "tsroam:", err)
		return exitError
	}
	if iface == "" {
		fmt.Fprintf(stderr, "tsroam: this machine is not on %s now; run learn while on it\n", p)
		return exitError
	}
	if connected.Bits() > p.Bits() {
		fmt.Fprintf(stderr, "tsroam: %s is only partly on %s (%s is); learn %s instead\n", p, iface, connected, connected)
		return exitError
	}
	line := "trusted " + p.String()
	if gw.IsValid() {
		line += " gateway " + gw.String()
	} else {
		var ok bool
		if gw, ok = s.DefaultGateway(index); !ok && probe == "" {
			fmt.Fprintf(stderr, "tsroam: %s has no default gateway; give --gateway ADDR or --probe ADDR:PORT\n", iface)
			return exitError
		}
	}
	if gw.IsValid() {
		mac, err := s.GatewayMAC(ctx, iface, gw)
		if err != nil {
			fmt.Fprintf(stderr, "tsroam: asking %s on %s for its MAC: %v\n", gw, iface, err)
			return exitError
		}
		line += " gateway-mac " + mac.String()
		fmt.Fprintf(stderr, "%s is on %s; its gateway %s is %s\n", p, iface, gw, mac)
	}
	if probe != "" {
		if err := s.Probe(ctx, iface, ap); err != nil {
			fmt.Fprintf(stderr, "tsroam: the probe %s from %s failed: %v\n", ap, iface, err)
			return exitError
		}
		line += " probe " + ap.String()
		fmt.Fprintf(stderr, "the probe %s answers from %s\n", ap, iface)
	}
	if _, err := config.Parse(line, "learn"); err != nil {
		fmt.Fprintln(stderr, "tsroam:", err)
		return exitError
	}
	fmt.Fprintln(stdout, line)
	return exitOK
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
