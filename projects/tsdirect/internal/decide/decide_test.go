package decide

import (
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/tsdirect/internal/config"
)

func TestDecide(t *testing.T) {
	home, _ := config.Parse("lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56", "t")
	both, _ := config.Parse("lan 192.168.1.0/24 gateway-mac 3c:7c:3f:12:34:56 probe 192.168.1.5:22", "t")
	mac := func(s string) net.HardwareAddr { m, _ := net.ParseMAC(s); return m }
	yes, no := true, false
	gw := netip.MustParseAddr("192.168.1.1")
	at := Observation{Iface: "wlan0", Connected: netip.MustParsePrefix("192.168.1.0/24"), Gateway: gw, GatewayMAC: mac("3c:7c:3f:12:34:56")}
	for _, c := range []struct {
		name string
		l    config.LAN
		o    func(Observation) Observation
		on   bool
		why  string
	}{
		{"at home", home, func(o Observation) Observation { return o }, true, "on 192.168.1.0/24 via wlan0"},
		{"not on it", home, func(o Observation) Observation { return Observation{} }, false, "not on"},
		{"a café with the same prefix", home, func(o Observation) Observation { o.GatewayMAC = mac("aa:bb:cc:dd:ee:ff"); return o }, false, "another network"},
		{"gateway silent", home, func(o Observation) Observation { o.GatewayMAC = nil; return o }, false, "does not answer"},
		{"no gateway", home, func(o Observation) Observation { o.Gateway, o.GatewayMAC = netip.Addr{}, nil; return o }, false, "no gateway"},
		{"only part of it connected", home, func(o Observation) Observation { o.Connected = netip.MustParsePrefix("192.168.1.0/25"); return o }, false, "only partly"},
		{"another route takes part of it", home, func(o Observation) Observation { o.Conflict = "the main table also sends 192.168.1.0/25 to wlan1, not eth0"; return o }, false, "would not keep it all on"},
		{"a wider connected network is fine", home, func(o Observation) Observation { o.Connected = netip.MustParsePrefix("192.168.0.0/16"); return o }, true, "via wlan0"},
		{"probe answers", both, func(o Observation) Observation { o.ProbeOK = &yes; return o }, true, "probe 192.168.1.5:22 answers"},
		{"probe fails", both, func(o Observation) Observation { o.ProbeOK, o.ProbeErr = &no, "connection refused"; return o }, false, "probe 192.168.1.5:22 failed"},
		{"probe not tried", both, func(o Observation) Observation { return o }, false, "not tried"},
	} {
		v := Decide(c.l, c.o(at))
		if v.On != c.on || !strings.Contains(v.Reason, c.why) {
			t.Errorf("%s: %+v", c.name, v)
		}
	}
}
