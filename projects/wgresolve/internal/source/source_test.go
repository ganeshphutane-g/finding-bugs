package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	k1 = "xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg="
	k2 = "TrMvSoP4jYQlY6RIzBgbssQqY3vxI2Pi+y71lOWWXX0="
	k3 = "gN65BkIKy1eCE9pP1wdc8ROUtkHLF2PfAqYdyYBz6EA="
)

func write(t *testing.T, path, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func endpoints(r *Result) string {
	var out []string
	for _, p := range r.Peers {
		out = append(out, p.Interface+" "+p.PublicKey.String()[:4]+" "+p.Endpoint())
	}
	return strings.Join(out, "; ")
}

func TestWGQuick(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "wg0.conf"), `[Interface]
PrivateKey = yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=
Address = 10.0.0.2/24
DNS = 10.0.0.1  # through the tunnel

[Peer]
PublicKey = `+k1+`
Endpoint = home.example.org:51820   # dynamic DNS
AllowedIPs = 0.0.0.0/0

# a peer with a fixed address
[Peer]
PublicKey = `+k2+`
Endpoint = 203.0.113.9:51820

[peer]
publickey=`+k3+`
endpoint=[2001:db8::1]:51820
[Peer]
PublicKey = `+k3+`
Endpoint = v6only.example.org.:443
`)
	write(t, filepath.Join(d, "not a name!.conf"), "[Peer]\nPublicKey = "+k1+"\nEndpoint = x.example:1\n")
	write(t, filepath.Join(d, "bad.conf"), "[Peer]\nPublicKey = nope\nEndpoint = x.example:1\n[Peer]\nPublicKey = "+k1+"\nEndpoint = x.example\n[Peer]\nPublicKey = "+k1+"\nEndpoint = x.example:99999\n")
	var r Result
	if err := WGQuick(d, &r); err != nil {
		t.Fatal(err)
	}
	if got := endpoints(&r); got != "wg0 xTIB home.example.org:51820; wg0 gN65 v6only.example.org:443" {
		t.Fatal(got)
	}
	if r.Fixed != 2 || len(r.Problems) != 3 {
		t.Fatalf("%d %v", r.Fixed, r.Problems)
	}
	if r.Peers[0].Source != filepath.Join(d, "wg0.conf")+":6" {
		t.Fatal(r.Peers[0].Source)
	}
}

func TestNetworkd(t *testing.T) {
	etc, run := t.TempDir(), t.TempDir()
	write(t, filepath.Join(etc, "50-wg1.netdev"), `[NetDev]
Name=wg1
Kind=wireguard
# a comment; Endpoint=ignored.example:1

[WireGuard]
PrivateKeyFile=/etc/systemd/network/wg1.key

[WireGuardPeer]
PublicKey=`+k1+`
Endpoint=vpn.example.net:51820
AllowedIPs=10.1.0.0/24
`)
	// Hidden by the /etc file of the same name.
	write(t, filepath.Join(run, "50-wg1.netdev"), "[NetDev]\nName=wg1\nKind=wireguard\n[WireGuardPeer]\nPublicKey="+k2+"\nEndpoint=hidden.example:1\n")
	// A drop-in adds a peer.
	write(t, filepath.Join(run, "50-wg1.netdev.d", "peer.conf"), "[WireGuardPeer]\nPublicKey="+k3+"\nEndpoint=dropin.example:51820\n")
	// Not WireGuard.
	write(t, filepath.Join(etc, "br0.netdev"), "[NetDev]\nName=br0\nKind=bridge\n[WireGuardPeer]\nPublicKey="+k2+"\nEndpoint=no.example:1\n")
	var r Result
	if err := Networkd([]string{etc, run}, &r); err != nil {
		t.Fatal(err)
	}
	if got := endpoints(&r); got != "wg1 xTIB vpn.example.net:51820; wg1 gN65 dropin.example:51820" {
		t.Fatal(got)
	}
}

func TestNetworkManager(t *testing.T) {
	d := t.TempDir()
	write(t, filepath.Join(d, "Home VPN.nmconnection"), `[connection]
id=Home VPN
uuid=4b7a1b8e-8f0a-4a3e-9a0c-2b1f9f9d1c11
type=wireguard
interface-name=wg-home

[wireguard]
private-key=yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=

[wireguard-peer.`+k1+`]
endpoint=home.example.org:51820
allowed-ips=0.0.0.0/0;

[ipv4]
method=manual
`)
	write(t, filepath.Join(d, "wifi.nmconnection"), "[connection]\ntype=wifi\ninterface-name=wlan0\n[wireguard-peer."+k2+"]\nendpoint=x.example:1\n")
	write(t, filepath.Join(d, "notes.txt"), "[connection]\ntype=wireguard\n")
	var r Result
	if err := NetworkManager([]string{d, filepath.Join(d, "missing")}, &r); err != nil {
		t.Fatal(err)
	}
	if got := endpoints(&r); got != "wg-home xTIB home.example.org:51820" {
		t.Fatal(got)
	}
}

func TestFlagAndDuplicates(t *testing.T) {
	var r Result
	if err := Flag("wg0="+k1+"=a.example:51820", &r); err != nil {
		t.Fatal(err)
	}
	// The same peer from a file with another endpoint: the first wins, with a
	// problem noted.
	r.Add("wg0", k1, "b.example:51820", "/etc/wireguard/wg0.conf:3")
	if len(r.Peers) != 1 || r.Peers[0].Host != "a.example" || len(r.Problems) != 1 {
		t.Fatalf("%+v", r)
	}
	if err := Flag("wg0="+k1+"=a.example:51820", &r); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"wg0", "wg0=" + k1, "wg0==a.example:1", "wg0=" + k1 + "=", "wg0=nope=a.example:1", "bad name!=" + k1 + "=a.example:1", "wg0=" + k1 + "=a.example"} {
		if err := Flag(bad, &r); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if err := Flag("wg0="+k2+"=a.ex ample:1", &r); err == nil {
		t.Error("space in host accepted")
	}
}

func TestUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	d := t.TempDir()
	write(t, filepath.Join(d, "wg0.conf"), "[Peer]\n")
	os.Chmod(filepath.Join(d, "wg0.conf"), 0)
	var r Result
	if err := WGQuick(d, &r); err != nil || len(r.Problems) != 1 {
		t.Fatal(err, r.Problems)
	}
}

func TestBadFilesDoNotHideOthers(t *testing.T) {
	d := t.TempDir()
	os.Symlink(filepath.Join(d, "gone"), filepath.Join(d, "a0.conf"))
	os.Mkdir(filepath.Join(d, "b0.conf"), 0o755)
	write(t, filepath.Join(d, "c0.conf"), "[Peer]\nPublicKey = "+k1+"\n"+strings.Repeat("#", 2<<20)+"\n")
	write(t, filepath.Join(d, "wg0.conf"), "[Peer]\nPublicKey = "+k2+"\nEndpoint = ok.example:51820\n")
	var r Result
	if err := WGQuick(d, &r); err != nil {
		t.Fatal(err)
	}
	if got := endpoints(&r); got != "wg0 TrMv ok.example:51820" || len(r.Problems) != 3 {
		t.Fatalf("%s %v", got, r.Problems)
	}
}

func TestSourceDetails(t *testing.T) {
	// networkd: a drop-in configuring the same peer again wins.
	etc := t.TempDir()
	write(t, filepath.Join(etc, "wg.netdev"), "[NetDev]\nName=wg2\nKind=wireguard\n[WireGuardPeer]\nPublicKey="+k1+"\nEndpoint=old.example:1\n")
	write(t, filepath.Join(etc, "wg.netdev.d", "x.conf"), "[WireGuardPeer]\nPublicKey="+k1+"\nEndpoint=new.example:1\n")
	var r Result
	Networkd([]string{etc}, &r)
	if got := endpoints(&r); got != "wg2 xTIB new.example:1" {
		t.Fatal(got)
	}
	// NetworkManager: a keyfile without the suffix counts, a backup does
	// not, and one without interface-name is reported.
	nm := t.TempDir()
	kf := "[connection]\ntype=wireguard\ninterface-name=wg3\n[wireguard-peer." + k2 + "]\nendpoint=legacy.example:51820\n"
	write(t, filepath.Join(nm, "Legacy VPN"), kf)
	write(t, filepath.Join(nm, "Legacy VPN~"), kf)
	write(t, filepath.Join(nm, "noiface.nmconnection"), "[connection]\ntype=wireguard\n[wireguard-peer."+k3+"]\nendpoint=x.example:1\n")
	r = Result{}
	NetworkManager([]string{nm}, &r)
	if got := endpoints(&r); got != "wg3 TrMv legacy.example:51820" || len(r.Problems) != 1 {
		t.Fatal(got, r.Problems)
	}
	// Keys are case-sensitive there.
	write(t, filepath.Join(etc, "case.netdev"), "[NetDev]\nname=wg4\nKind=wireguard\n[WireGuardPeer]\nPublicKey="+k3+"\nEndpoint=c.example:1\n")
	r = Result{}
	Networkd([]string{etc}, &r)
	if strings.Contains(endpoints(&r), "wg4") {
		t.Fatal("name= read as Name=")
	}
}
