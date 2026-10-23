// Package decide says whether a LAN's rule should be in place: only while
// the machine is on a network that has the LAN's addresses and the LAN's
// fingerprint. Anything unknown counts as "not this LAN": a wrong yes would
// send private traffic onto a stranger's network.
package decide

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/ganeshphutane-g/tsdirect/internal/config"
)

// Observation is what the machine sees for one LAN now.
type Observation struct {
	// Iface is the interface with a directly connected route covering the
	// whole prefix ("" if none, or if it is a Tailscale interface).
	Iface string
	// Connected is that route's prefix.
	Connected netip.Prefix
	// Gateway is the address whose MAC was looked at, and GatewayMAC what
	// was found (nil: no answer, not looked at, or no gateway).
	Gateway    netip.Addr
	GatewayMAC net.HardwareAddr
	// GatewayErr: asking failed for another reason than silence (no
	// Ethernet address, no permission).
	GatewayErr string
	// Conflict: other main-table routes would take part of the prefix
	// elsewhere ("" if none).
	Conflict string
	// ProbeOK: the probe connected (nil: not tried).
	ProbeOK  *bool
	ProbeErr string
}

// Verdict is the decision.
type Verdict struct {
	On bool
	// Kind is which rule decided, without the details (an error's text)
	// that Reason has: it says whether the verdict changed.
	Kind   Kind
	Reason string
}

// Kind is which rule decided.
type Kind string

// The kinds of verdict.
const (
	Direct          Kind = "direct"
	Absent          Kind = "absent"
	Partial         Kind = "partial"
	Conflict        Kind = "conflict"
	NoGateway       Kind = "no gateway"
	GatewayError    Kind = "gateway error"
	GatewaySilent   Kind = "gateway silent"
	GatewayMismatch Kind = "gateway mismatch"
	ProbeUntried    Kind = "probe not tried"
	ProbeFailed     Kind = "probe failed"
	// Error: the machine could not be looked at (set by the caller).
	Error Kind = "error"
)

// Decide applies the rules.
func Decide(l config.LAN, o Observation) Verdict {
	if o.Iface == "" {
		return Verdict{Kind: Absent, Reason: fmt.Sprintf("not on %s (no interface has it as a connected network)", l.Prefix)}
	}
	if !o.Connected.IsValid() || o.Connected.Bits() > l.Prefix.Bits() || !o.Connected.Contains(l.Prefix.Addr()) {
		return Verdict{Kind: Partial, Reason: fmt.Sprintf("%s is only partly on %s (%s): configure the network's own prefix", l.Prefix, o.Iface, o.Connected)}
	}
	if o.Conflict != "" {
		return Verdict{Kind: Conflict, Reason: fmt.Sprintf("on %s via %s, but %s: sending %s to the main table would not keep it all on %s", l.Prefix, o.Iface, o.Conflict, l.Prefix, o.Iface)}
	}
	if l.GatewayMAC != nil {
		switch {
		case !o.Gateway.IsValid():
			return Verdict{Kind: NoGateway, Reason: fmt.Sprintf("on a %s network on %s, but it has no gateway to check", l.Prefix, o.Iface)}
		case o.GatewayMAC == nil && o.GatewayErr != "":
			return Verdict{Kind: GatewayError, Reason: fmt.Sprintf("on a %s network on %s, but its gateway %s could not be asked: %s", l.Prefix, o.Iface, o.Gateway, o.GatewayErr)}
		case o.GatewayMAC == nil:
			return Verdict{Kind: GatewaySilent, Reason: fmt.Sprintf("on a %s network on %s, but its gateway %s does not answer", l.Prefix, o.Iface, o.Gateway)}
		case !bytes.Equal(o.GatewayMAC, l.GatewayMAC):
			return Verdict{Kind: GatewayMismatch, Reason: fmt.Sprintf("on a %s network on %s, but its gateway %s is %s, not %s: another network", l.Prefix, o.Iface, o.Gateway, o.GatewayMAC, l.GatewayMAC)}
		}
	}
	if l.Probe.IsValid() {
		switch {
		case o.ProbeOK == nil:
			return Verdict{Kind: ProbeUntried, Reason: fmt.Sprintf("on a %s network on %s; the probe %s was not tried", l.Prefix, o.Iface, l.Probe)}
		case !*o.ProbeOK:
			return Verdict{Kind: ProbeFailed, Reason: fmt.Sprintf("on a %s network on %s, but the probe %s failed (%s): another network", l.Prefix, o.Iface, l.Probe, o.ProbeErr)}
		}
	}
	what := []string{}
	if l.GatewayMAC != nil {
		what = append(what, "gateway "+o.Gateway.String()+" is "+l.GatewayMAC.String())
	}
	if l.Probe.IsValid() {
		what = append(what, "probe "+l.Probe.String()+" answers")
	}
	return Verdict{On: true, Kind: Direct, Reason: fmt.Sprintf("on %s via %s (%s)", l.Prefix, o.Iface, strings.Join(what, ", "))}
}
