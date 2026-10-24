package policy

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/tsroam/internal/config"
	"github.com/ganeshphutane-g/tsroam/internal/sys"
)

// fake is a machine with interfaces, each on one connected network, and a
// default route on one of them.
type fake struct {
	nets     map[string]netip.Prefix // interface: its connected network
	def      string                  // the default route's interface ("": none)
	multi    bool
	gateway  netip.Addr
	mac      net.HardwareAddr
	probeErr error
	routeErr error
	asked    []string
}

func (f *fake) DefaultRoute() (string, int, netip.Addr, bool, error) {
	if f.routeErr != nil {
		return "", 0, netip.Addr{}, false, f.routeErr
	}
	if f.multi {
		return "", 0, netip.Addr{}, true, nil
	}
	return f.def, 2, f.gateway, f.def != "", nil
}

func (f *fake) Connected(p netip.Prefix) (string, int, netip.Prefix, error) {
	for name, n := range f.nets {
		if n.Contains(p.Addr()) && n.Bits() <= p.Bits() {
			return name, 2, n, nil
		}
	}
	return "", 0, netip.Prefix{}, nil
}

func (f *fake) DefaultGateway(int) (netip.Addr, bool) { return f.gateway, f.gateway.IsValid() }

func (f *fake) GatewayMAC(_ context.Context, iface string, addr netip.Addr) (net.HardwareAddr, error) {
	f.asked = append(f.asked, "mac "+iface)
	if f.mac == nil {
		return nil, sys.ErrNoAnswer
	}
	return f.mac, nil
}

func (f *fake) Probe(_ context.Context, iface string, ap netip.AddrPort) error {
	f.asked = append(f.asked, "probe "+iface)
	return f.probeErr
}

var (
	homeMAC = net.HardwareAddr{0x3c, 0x7c, 0x3f, 0x12, 0x34, 0x56}
	homeNet = netip.MustParsePrefix("192.168.1.0/24")
)

func atHome() *fake {
	return &fake{nets: map[string]netip.Prefix{"wlan0": homeNet}, def: "wlan0", gateway: netip.MustParseAddr("192.168.1.1"), mac: homeMAC}
}

func trusted(t *testing.T, lines ...string) []config.Network {
	var out []config.Network
	for _, l := range lines {
		n, err := config.Parse(l, "test")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

const home = "trusted 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56"

func TestHome(t *testing.T) {
	p := Look(context.Background(), atHome(), trusted(t, "trusted 10.0.0.0/8 probe 10.0.0.1:22", home))
	if p.Kind != Trusted || p.Network == nil || p.Network.Prefix != homeNet || p.Iface != "wlan0" || p.Err != nil {
		t.Fatalf("%+v", p)
	}
	if len(p.Verdicts) != 2 || p.Verdicts[0].On || !p.Verdicts[1].On {
		t.Errorf("verdicts %+v", p.Verdicts)
	}
}

func TestCafeWithTheSameAddresses(t *testing.T) {
	f := atHome()
	f.mac = net.HardwareAddr{2, 0, 0, 0, 0, 9}
	p := Look(context.Background(), f, trusted(t, home))
	if p.Kind != Untrusted || !strings.Contains(p.Reason, "not a trusted one") {
		t.Fatalf("%+v", p)
	}
}

func TestNoTrustedNetworks(t *testing.T) {
	if p := Look(context.Background(), atHome(), nil); p.Kind != Untrusted {
		t.Fatalf("%+v", p)
	}
}

func TestOffline(t *testing.T) {
	f := atHome()
	f.def = ""
	if p := Look(context.Background(), f, trusted(t, home)); p.Kind != Offline {
		t.Fatalf("%+v", p)
	}
}

func TestMultipathIsUntrusted(t *testing.T) {
	f := atHome()
	f.multi = true
	if p := Look(context.Background(), f, trusted(t, home)); p.Kind != Untrusted || !strings.Contains(p.Reason, "several paths") {
		t.Fatalf("%+v", p)
	}
}

func TestTrustedOnAnotherInterface(t *testing.T) {
	// Home Ethernet is plugged in, but the default route is the café Wi-Fi.
	f := atHome()
	f.nets = map[string]netip.Prefix{"eth0": homeNet, "wlan0": netip.MustParsePrefix("10.9.0.0/24")}
	f.def = "wlan0"
	p := Look(context.Background(), f, trusted(t, home))
	if p.Kind != Untrusted || !strings.Contains(p.Reason, "trusted 192.168.1.0/24 is on eth0") {
		t.Fatalf("%+v", p)
	}
	if len(f.asked) != 0 {
		t.Errorf("fingerprinted another interface: %v", f.asked)
	}
	if !strings.Contains(p.Verdicts[0].Reason, "traffic leaves on wlan0") {
		t.Errorf("verdict %+v", p.Verdicts[0])
	}
}

func TestErrorsAreUntrusted(t *testing.T) {
	f := atHome()
	f.routeErr = errors.New("netlink: permission denied")
	if p := Look(context.Background(), f, trusted(t, home)); p.Kind != Untrusted || p.Err == nil {
		t.Fatalf("%+v", p)
	}
}

func TestProbeOnlyAfterTheMAC(t *testing.T) {
	f := atHome()
	f.mac = net.HardwareAddr{2, 0, 0, 0, 0, 9}
	Look(context.Background(), f, trusted(t, home+" probe 192.168.1.5:22"))
	if strings.Join(f.asked, ",") != "mac wlan0" {
		t.Errorf("asked %v", f.asked)
	}
	f = atHome()
	f.probeErr = errors.New("connection refused")
	if p := Look(context.Background(), f, trusted(t, home+" probe 192.168.1.5:22")); p.Kind != Untrusted {
		t.Errorf("a failing probe: %+v", p)
	}
}
