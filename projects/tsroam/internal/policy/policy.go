// Package policy says where the machine is: offline, on one of its trusted
// networks, or anywhere else, which is untrusted. The network that counts
// is the one the default route leaves on.
package policy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/ganeshphutane-g/tsroam/internal/config"
	"github.com/ganeshphutane-g/tsroam/internal/netid"
	"github.com/ganeshphutane-g/tsroam/internal/sys"
)

// System is what the policy needs from the machine (sys.Linux; a fake in
// tests).
type System interface {
	DefaultRoute() (iface string, index int, gateway netip.Addr, ok bool, err error)
	Connected(p netip.Prefix) (iface string, index int, connected netip.Prefix, err error)
	DefaultGateway(index int) (netip.Addr, bool)
	GatewayMAC(ctx context.Context, iface string, addr netip.Addr) (net.HardwareAddr, error)
	Probe(ctx context.Context, iface string, ap netip.AddrPort) error
}

// Kind is where the machine is.
type Kind string

const (
	// Offline: no default route; nothing to protect, nothing changes.
	Offline Kind = "offline"
	// Trusted: the default route leaves on a trusted network.
	Trusted Kind = "trusted"
	// Untrusted: anywhere else, or it could not be told.
	Untrusted Kind = "untrusted"
)

// Place is the answer, with what led to it.
type Place struct {
	Kind Kind
	// Iface is the default route's interface ("" offline, or multipath).
	Iface string
	// Network is the trusted network matched (when Trusted).
	Network *config.Network
	// Reason says why, in a sentence.
	Reason string
	// Verdicts is each trusted network's verdict, in config order.
	Verdicts []netid.Verdict
	// Err: something could not be looked at. The place is Untrusted then
	// (unless offline), and the caller should say so.
	Err error
}

// Look decides where the machine is.
func Look(ctx context.Context, s System, trusted []config.Network) Place {
	iface, _, _, ok, err := s.DefaultRoute()
	if err != nil {
		return Place{Kind: Untrusted, Reason: fmt.Sprintf("could not read the default route: %v", err), Err: err}
	}
	if !ok {
		return Place{Kind: Offline, Reason: "no default route"}
	}
	if iface == "" {
		return Place{Kind: Untrusted, Reason: "the default route has several paths; tsroam cannot tell which network traffic leaves on"}
	}
	p := Place{Kind: Untrusted, Iface: iface}
	var errs []error
	var elsewhere string
	for i := range trusted {
		n := &trusted[i]
		o, err := Observe(ctx, s, *n, iface)
		v := netid.Decide(*n, o)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n.Prefix, err))
			v = netid.Verdict{Kind: netid.Error, Reason: fmt.Sprintf("could not look at %s: %v", n.Prefix, err)}
		}
		if err == nil && o.Iface != "" && o.Iface != iface {
			v = netid.Verdict{Kind: netid.Absent, Reason: fmt.Sprintf("%s is on %s, but traffic leaves on %s", n.Prefix, o.Iface, iface)}
		}
		if v.Kind == netid.GatewayError {
			errs = append(errs, errors.New(v.Reason))
		}
		p.Verdicts = append(p.Verdicts, v)
		switch {
		case v.On && o.Iface == iface:
			if p.Network == nil {
				p.Kind, p.Network = Trusted, n
				p.Reason = fmt.Sprintf("trusted network %s (%s): %s", n.Prefix, n.Source, v.Reason)
			}
		case o.Iface != "" && o.Iface != iface && elsewhere == "":
			elsewhere = fmt.Sprintf(" (trusted %s is on %s)", n.Prefix, o.Iface)
		}
	}
	p.Err = errors.Join(errs...)
	if p.Kind == Untrusted {
		p.Reason = fmt.Sprintf("the network on %s is not a trusted one%s", iface, elsewhere)
		if p.Err != nil {
			p.Reason += fmt.Sprintf("; also: %v", p.Err)
		}
	}
	return p
}

// Observe looks at the machine for one trusted network. Its fingerprint is
// only checked on the interface only (the default route's), and a step is
// only taken when the earlier ones passed: no probe goes out on a
// network whose gateway is not the expected one.
func Observe(ctx context.Context, s System, n config.Network, only string) (netid.Observation, error) {
	var o netid.Observation
	iface, index, connected, err := s.Connected(n.Prefix)
	var conflict *sys.ConflictError
	if errors.As(err, &conflict) {
		o.Iface, o.Connected, o.Conflict = conflict.Iface, conflict.Connected, conflict.Why
		return o, nil
	}
	if err != nil {
		return o, fmt.Errorf("reading routes: %w", err)
	}
	if iface == "" {
		return o, nil
	}
	o.Iface, o.Connected = iface, connected
	if iface != only || connected.Bits() > n.Prefix.Bits() {
		return o, nil
	}
	if n.GatewayMAC != nil {
		gw, ok := n.Gateway, n.Gateway.IsValid()
		if !ok {
			gw, ok = s.DefaultGateway(index)
		}
		if !ok {
			return o, nil
		}
		o.Gateway = gw
		mac, err := s.GatewayMAC(ctx, iface, gw)
		switch {
		case errors.Is(err, sys.ErrNoAnswer):
			return o, nil
		case err != nil:
			o.GatewayErr = err.Error()
			return o, nil
		}
		o.GatewayMAC = mac
		if !bytes.Equal(mac, n.GatewayMAC) {
			return o, nil
		}
	}
	if n.Probe.IsValid() {
		err := s.Probe(ctx, iface, n.Probe)
		ok := err == nil
		o.ProbeOK = &ok
		if err != nil {
			o.ProbeErr = err.Error()
		}
	}
	return o, nil
}
