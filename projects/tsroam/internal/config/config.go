// Package config reads tsroam's config: the exit node, the kill switch, and
// the trusted networks, each a prefix and the fingerprint that says the
// machine is really on that network and not on another one with the same
// addresses.
//
//	exit-node 100.64.0.1
//	kill-switch on
//	allow-lan off
//	trusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56
//	trusted 10.10.0.0/16 probe 10.10.0.5:22
//	trusted 192.168.50.0/24 gateway 192.168.50.254 gateway-mac 00:11:22:33:44:55 probe 192.168.50.10:443
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
)

// Network is one trusted network.
type Network struct {
	Prefix netip.Prefix
	// GatewayMAC: the gateway's MAC must be this (nil: not checked).
	GatewayMAC net.HardwareAddr
	// Gateway is the address whose MAC is checked; invalid means the main
	// table's default gateway on the LAN's interface.
	Gateway netip.Addr
	// Probe: a TCP connection to it, made from the LAN's interface, must
	// succeed (invalid: not checked).
	Probe netip.AddrPort
	// Source is where it was configured ("file:3", "--trusted").
	Source string
}

func (l Network) String() string {
	s := "trusted " + l.Prefix.String()
	if l.Gateway.IsValid() {
		s += " gateway " + l.Gateway.String()
	}
	if l.GatewayMAC != nil {
		s += " gateway-mac " + l.GatewayMAC.String()
	}
	if l.Probe.IsValid() {
		s += " probe " + l.Probe.String()
	}
	return s
}

// Parse reads one "trusted ..." line (the leading "trusted" may be left
// out, as in a --trusted flag).
func Parse(line, source string) (Network, error) {
	f := strings.Fields(line)
	if len(f) > 0 && f[0] == "trusted" {
		f = f[1:]
	}
	if len(f) == 0 {
		return Network{}, fmt.Errorf("%s: a trusted network needs a prefix, such as 192.168.1.0/24", source)
	}
	p, err := netip.ParsePrefix(f[0])
	if err != nil {
		return Network{}, fmt.Errorf("%s: %q is not a prefix such as 192.168.1.0/24", source, f[0])
	}
	if p.Masked() != p {
		return Network{}, fmt.Errorf("%s: %s has host bits set; did you mean %s?", source, p, p.Masked())
	}
	if !p.Addr().Is4() {
		return Network{}, fmt.Errorf("%s: %s: only IPv4 networks are supported for now", source, p)
	}
	if p.Bits() == 0 {
		return Network{}, fmt.Errorf("%s: %s is everything, not a network", source, p)
	}
	l := Network{Prefix: p, Source: source}
	rest := f[1:]
	for len(rest) > 0 {
		if len(rest) < 2 {
			return Network{}, fmt.Errorf("%s: %q needs a value", source, rest[0])
		}
		k, v := rest[0], rest[1]
		rest = rest[2:]
		switch k {
		case "gateway-mac":
			mac, err := net.ParseMAC(v)
			if err != nil || len(mac) != 6 {
				return Network{}, fmt.Errorf("%s: %q is not a MAC address such as 3c:7c:3f:12:34:56", source, v)
			}
			if l.GatewayMAC != nil {
				return Network{}, fmt.Errorf("%s: gateway-mac given twice", source)
			}
			l.GatewayMAC = mac
		case "gateway":
			a, err := netip.ParseAddr(v)
			if err != nil || !p.Contains(a) {
				return Network{}, fmt.Errorf("%s: gateway %q must be an address in %s", source, v, p)
			}
			if l.Gateway.IsValid() {
				return Network{}, fmt.Errorf("%s: gateway given twice", source)
			}
			l.Gateway = a
		case "probe":
			ap, err := netip.ParseAddrPort(v)
			if err != nil || ap.Port() == 0 || !p.Contains(ap.Addr()) {
				return Network{}, fmt.Errorf("%s: probe %q must be address:port with the address in %s", source, v, p)
			}
			if l.Probe.IsValid() {
				return Network{}, fmt.Errorf("%s: probe given twice", source)
			}
			l.Probe = ap
		default:
			return Network{}, fmt.Errorf("%s: unknown %q (use gateway-mac, gateway or probe)", source, k)
		}
	}
	if l.GatewayMAC == nil && !l.Probe.IsValid() {
		return Network{}, fmt.Errorf("%s: %s has no fingerprint: give gateway-mac and/or probe, so another network with the same addresses is not taken for it (tsroam learn %s prints them)", source, p, p)
	}
	if l.Gateway.IsValid() && l.GatewayMAC == nil {
		return Network{}, fmt.Errorf("%s: gateway is only used with gateway-mac", source)
	}
	return l, nil
}

// Config is all of tsroam's settings.
type Config struct {
	// ExitNode is what "tailscale set --exit-node" is given: an IP or a
	// node name.
	ExitNode string
	// KillSwitch: while the exit node is meant to be on, nothing but
	// tailnet traffic (and DHCP) leaves the machine.
	KillSwitch bool
	// AllowLAN: on untrusted networks, the local network stays reachable
	// directly (printers, the captive portal's own address).
	AllowLAN bool
	Trusted  []Network
}

// Read reads a config file: "exit-node", "kill-switch", "allow-lan" and
// "trusted" lines, # comments, blank lines.
func Read(r io.Reader, name string) (Config, error) {
	var c Config
	sc := bufio.NewScanner(r)
	no := 0
	seen := map[string]int{}
	for sc.Scan() {
		no++
		line, _, _ := strings.Cut(sc.Text(), "#")
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		where := fmt.Sprintf("%s:%d", name, no)
		if f[0] != "trusted" {
			if prev, ok := seen[f[0]]; ok {
				return Config{}, fmt.Errorf("%s: %s given twice (also line %d)", where, f[0], prev)
			}
			seen[f[0]] = no
		}
		switch f[0] {
		case "trusted":
			n, err := Parse(line, where)
			if err != nil {
				return Config{}, err
			}
			c.Trusted = append(c.Trusted, n)
		case "exit-node":
			if len(f) != 2 {
				return Config{}, fmt.Errorf("%s: exit-node takes one value: a node's Tailscale IP or name", where)
			}
			if err := CheckExitNode(f[1]); err != nil {
				return Config{}, fmt.Errorf("%s: %v", where, err)
			}
			c.ExitNode = f[1]
		case "kill-switch", "allow-lan":
			if len(f) != 2 || f[1] != "on" && f[1] != "off" {
				return Config{}, fmt.Errorf("%s: %s takes on or off", where, f[0])
			}
			if f[0] == "kill-switch" {
				c.KillSwitch = f[1] == "on"
			} else {
				c.AllowLAN = f[1] == "on"
			}
		default:
			return Config{}, fmt.Errorf("%s: unknown %q (use exit-node, kill-switch, allow-lan or trusted)", where, f[0])
		}
	}
	if err := sc.Err(); err != nil {
		return Config{}, err
	}
	return c, Check(c.Trusted)
}

// CheckExitNode refuses what cannot be a node: tsroam passes it to the
// tailscale CLI, so it must not look like a flag or hold spaces.
func CheckExitNode(v string) error {
	if v == "" || strings.HasPrefix(v, "-") || strings.ContainsAny(v, " \t=") {
		return fmt.Errorf("exit-node %q is not a node's Tailscale IP or name", v)
	}
	return nil
}

// ReadFile reads a config file. A missing one is an error: tsroam without
// its config would run with no kill switch and no trusted networks.
func ReadFile(path string) (Config, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("%s does not exist (tsroam learn prints its first trusted line)", path)
	}
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	return Read(f, path)
}

// Check refuses overlapping trusted networks: a network must match one
// line at most, so the verdicts cannot contradict each other.
func Check(nets []Network) error {
	for i, a := range nets {
		for _, b := range nets[i+1:] {
			if a.Prefix.Overlaps(b.Prefix) {
				return fmt.Errorf("%s and %s overlap (%s, %s)", a.Prefix, b.Prefix, a.Source, b.Source)
			}
		}
	}
	return nil
}
