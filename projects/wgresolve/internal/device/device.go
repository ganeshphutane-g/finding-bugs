// Package device reads and changes WireGuard peers through wgctrl: kernel
// WireGuard over netlink, or wireguard-go and other userspace
// implementations over their UAPI sockets.
package device

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Peer is a peer's live state.
type Peer struct {
	Endpoint      netip.AddrPort
	LastHandshake time.Time
}

// Device is an interface's live state.
type Device struct {
	// Mark is the interface's firewall mark (wg-quick sets one for full
	// tunnels): DNS queries carry it, so they bypass the tunnel too.
	Mark  int
	Peers map[wgtypes.Key]Peer
}

// Devices is what wgresolve needs from WireGuard.
type Devices interface {
	// Device reads an interface; ErrNoDevice when it is not up.
	Device(iface string) (*Device, error)
	// SetEndpoint changes one existing peer's endpoint and nothing else.
	SetEndpoint(iface string, key wgtypes.Key, ep netip.AddrPort) error
	Close() error
}

// ErrNoDevice means the interface does not exist (not up yet).
var ErrNoDevice = errors.New("no such WireGuard interface")

// WG is the real thing.
type WG struct{ c *wgctrl.Client }

// Open opens wgctrl.
func Open() (*WG, error) {
	c, err := wgctrl.New()
	if err != nil {
		return nil, err
	}
	return &WG{c: c}, nil
}

// Close closes it.
func (w *WG) Close() error { return w.c.Close() }

// Device reads an interface.
func (w *WG) Device(iface string) (*Device, error) {
	d, err := w.c.Device(iface)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoDevice
	}
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("%s: %w (wgresolve needs root: CAP_NET_ADMIN for kernel WireGuard, and access to wireguard-go's root-owned socket in /var/run/wireguard)", iface, err)
		}
		return nil, fmt.Errorf("%s: %w", iface, err)
	}
	out := map[wgtypes.Key]Peer{}
	for _, p := range d.Peers {
		var ep netip.AddrPort
		if p.Endpoint != nil {
			if a, ok := netip.AddrFromSlice(p.Endpoint.IP); ok {
				ep = netip.AddrPortFrom(a.Unmap(), uint16(p.Endpoint.Port))
			}
		}
		out[p.PublicKey] = Peer{Endpoint: ep, LastHandshake: p.LastHandshakeTime}
	}
	return &Device{Mark: d.FirewallMark, Peers: out}, nil
}

// SetEndpoint changes one peer's endpoint. UpdateOnly: a peer that was
// removed meanwhile is not created again.
func (w *WG) SetEndpoint(iface string, key wgtypes.Key, ep netip.AddrPort) error {
	addr := &net.UDPAddr{IP: ep.Addr().AsSlice(), Port: int(ep.Port())}
	return w.c.ConfigureDevice(iface, wgtypes.Config{Peers: []wgtypes.PeerConfig{{PublicKey: key, UpdateOnly: true, Endpoint: addr}}})
}
