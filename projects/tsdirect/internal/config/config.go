// Package config reads tsdirect's LANs: a prefix, and the fingerprint that
// says the machine is really on that LAN and not on another network with the
// same addresses.
//
//	lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56
//	lan 10.10.0.0/16 probe 10.10.0.5:22
//	lan 192.168.50.0/24 gateway 192.168.50.254 gateway-mac 00:11:22:33:44:55 probe 192.168.50.10:443
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

// LAN is one network to keep local.
type LAN struct {
	Prefix netip.Prefix
	// GatewayMAC: the gateway's MAC must be this (nil: not checked).
	GatewayMAC net.HardwareAddr
	// Gateway is the address whose MAC is checked; invalid means the main
	// table's default gateway on the LAN's interface.
	Gateway netip.Addr
	// Probe: a TCP connection to it, made from the LAN's interface, must
	// succeed (invalid: not checked).
	Probe netip.AddrPort
	// Source is where it was configured ("file:3", "--lan").
	Source string
}

func (l LAN) String() string {
	s := "lan " + l.Prefix.String()
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

// Parse reads one "lan ..." line (without the leading "lan" when from a
// flag is fine too).
func Parse(line, source string) (LAN, error) {
	f := strings.Fields(line)
	if len(f) > 0 && f[0] == "lan" {
		f = f[1:]
	}
	if len(f) == 0 {
		return LAN{}, fmt.Errorf("%s: a LAN needs a prefix, such as 192.168.1.0/24", source)
	}
	p, err := netip.ParsePrefix(f[0])
	if err != nil {
		return LAN{}, fmt.Errorf("%s: %q is not a prefix such as 192.168.1.0/24", source, f[0])
	}
	if p.Masked() != p {
		return LAN{}, fmt.Errorf("%s: %s has host bits set; did you mean %s?", source, p, p.Masked())
	}
	if p.Bits() == 0 {
		return LAN{}, fmt.Errorf("%s: %s is everything, not a LAN", source, p)
	}
	l := LAN{Prefix: p, Source: source}
	rest := f[1:]
	for len(rest) > 0 {
		if len(rest) < 2 {
			return LAN{}, fmt.Errorf("%s: %q needs a value", source, rest[0])
		}
		k, v := rest[0], rest[1]
		rest = rest[2:]
		switch k {
		case "gateway-mac":
			mac, err := net.ParseMAC(v)
			if err != nil || len(mac) != 6 {
				return LAN{}, fmt.Errorf("%s: %q is not a MAC address such as 3c:7c:3f:12:34:56", source, v)
			}
			if l.GatewayMAC != nil {
				return LAN{}, fmt.Errorf("%s: gateway-mac given twice", source)
			}
			l.GatewayMAC = mac
		case "gateway":
			a, err := netip.ParseAddr(v)
			if err != nil || !p.Contains(a) {
				return LAN{}, fmt.Errorf("%s: gateway %q must be an address in %s", source, v, p)
			}
			l.Gateway = a
		case "probe":
			ap, err := netip.ParseAddrPort(v)
			if err != nil || ap.Port() == 0 || !p.Contains(ap.Addr()) {
				return LAN{}, fmt.Errorf("%s: probe %q must be address:port with the address in %s", source, v, p)
			}
			l.Probe = ap
		default:
			return LAN{}, fmt.Errorf("%s: unknown %q (use gateway-mac, gateway or probe)", source, k)
		}
	}
	if l.GatewayMAC == nil && !l.Probe.IsValid() {
		return LAN{}, fmt.Errorf("%s: %s has no fingerprint: give gateway-mac and/or probe, so another network with the same addresses is not taken for it (tsdirect learn %s prints them)", source, p, p)
	}
	if l.Gateway.IsValid() && l.GatewayMAC == nil {
		return LAN{}, fmt.Errorf("%s: gateway is only used with gateway-mac", source)
	}
	return l, nil
}

// Read reads a config file: "lan ..." lines, # comments, blank lines.
func Read(r io.Reader, name string) ([]LAN, error) {
	var out []LAN
	sc := bufio.NewScanner(r)
	no := 0
	for sc.Scan() {
		no++
		line, _, _ := strings.Cut(sc.Text(), "#")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "lan ") {
			return nil, fmt.Errorf("%s:%d: expected \"lan PREFIX ...\"", name, no)
		}
		l, err := Parse(line, fmt.Sprintf("%s:%d", name, no))
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, Check(out)
}

// ReadFile reads a config file; a missing one is no LANs.
func ReadFile(path string) ([]LAN, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f, path)
}

// Check refuses overlapping LANs: each address must belong to one LAN, so
// the rules cannot contradict each other.
func Check(lans []LAN) error {
	for i, a := range lans {
		for _, b := range lans[i+1:] {
			if a.Prefix.Overlaps(b.Prefix) {
				return fmt.Errorf("%s and %s overlap (%s, %s)", a.Prefix, b.Prefix, a.Source, b.Source)
			}
		}
	}
	return nil
}
