package source

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var ifaceName = regexp.MustCompile(`^[a-zA-Z0-9_=+.-]{1,15}$`)

// WGQuick reads wg-quick confs in dir (*.conf; the interface is named after
// the file, as wg-quick names it).
func WGQuick(dir string, r *Result) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if errors.Is(err, fs.ErrPermission) {
		r.problem(dir, "cannot be read (wgresolve needs to run as root)")
		return nil
	}
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".conf") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(files)
	for _, f := range files {
		iface := strings.TrimSuffix(filepath.Base(f), ".conf")
		if !ifaceName.MatchString(iface) {
			continue
		}
		secs, ok, err := parseFile(f, true, r)
		if err != nil {
			return err
		}
		if !ok {
			continue // reported; the other files still count
		}
		for _, s := range secs {
			if strings.EqualFold(s.name, "Peer") {
				r.Add(iface, s.get("PublicKey"), s.get("Endpoint"), fmt.Sprintf("%s:%d", f, s.no))
			}
		}
	}
	return nil
}

// NetworkdDirs are where systemd-networkd looks, highest priority first.
var NetworkdDirs = []string{"/etc/systemd/network", "/run/systemd/network", "/usr/local/lib/systemd/network", "/usr/lib/systemd/network"}

// Networkd reads .netdev files of Kind=wireguard, as networkd does: a file
// in an earlier directory hides one of the same name in a later one, and
// drop-ins (<name>.netdev.d/*.conf, from every directory) are read after
// the file, in name order.
func Networkd(dirs []string, r *Result) error {
	main := map[string]string{}
	dropins := map[string]map[string]string{}
	for _, d := range dirs {
		files, _ := filepath.Glob(filepath.Join(d, "*.netdev"))
		for _, f := range files {
			if _, seen := main[filepath.Base(f)]; !seen {
				main[filepath.Base(f)] = f
			}
		}
		dd, _ := filepath.Glob(filepath.Join(d, "*.netdev.d", "*.conf"))
		for _, f := range dd {
			unit := strings.TrimSuffix(filepath.Base(filepath.Dir(f)), ".d")
			if dropins[unit] == nil {
				dropins[unit] = map[string]string{}
			}
			if _, seen := dropins[unit][filepath.Base(f)]; !seen {
				dropins[unit][filepath.Base(f)] = f
			}
		}
	}
	names := make([]string, 0, len(main))
	for n := range main {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		paths := []string{main[n]}
		var dn []string
		for k := range dropins[n] {
			dn = append(dn, k)
		}
		sort.Strings(dn)
		for _, k := range dn {
			paths = append(paths, dropins[n][k])
		}
		if err := netdev(paths, r); err != nil {
			return err
		}
	}
	return nil
}

func netdev(paths []string, r *Result) error {
	type src struct {
		s    section
		file string
	}
	var all []src
	for _, p := range paths {
		secs, ok, err := parseFile(p, false, r)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		for _, s := range secs {
			all = append(all, src{s, p})
		}
	}
	var iface, kind string
	for _, x := range all {
		if x.s.name == "NetDev" {
			if v := x.s.get("Name"); v != "" {
				iface = v
			}
			if v := x.s.get("Kind"); v != "" {
				kind = v
			}
		}
	}
	if kind != "wireguard" || iface == "" {
		return nil
	}
	for _, x := range all {
		if x.s.name == "WireGuardPeer" {
			r.Add(iface, x.s.get("PublicKey"), x.s.get("Endpoint"), fmt.Sprintf("%s:%d", x.file, x.s.no))
		}
	}
	return nil
}

// NMDirs are where NetworkManager keeps keyfiles.
var NMDirs = []string{"/etc/NetworkManager/system-connections", "/run/NetworkManager/system-connections"}

// NetworkManager reads keyfiles of type wireguard. Peers are sections named
// [wireguard-peer.<public key>]; the interface is interface-name, as
// NetworkManager requires for WireGuard.
func NetworkManager(dirs []string, r *Result) error {
	for _, d := range dirs {
		files, err := os.ReadDir(d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if errors.Is(err, fs.ErrPermission) {
			r.problem(d, "cannot be read (wgresolve needs to run as root)")
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range files {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".nmconnection") {
				continue
			}
			p := filepath.Join(d, e.Name())
			secs, ok, err := parseFile(p, false, r)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			var iface, typ string
			for _, s := range secs {
				if s.name == "connection" {
					iface, typ = s.get("interface-name"), s.get("type")
				}
			}
			if typ != "wireguard" {
				continue
			}
			for _, s := range secs {
				if key, ok := strings.CutPrefix(s.name, "wireguard-peer."); ok {
					r.Add(iface, key, s.get("endpoint"), fmt.Sprintf("%s:%d", p, s.no))
				}
			}
		}
	}
	return nil
}

// Flag reads a --peer value: IFACE=PUBKEY=host:port.
func Flag(v string, r *Result) error {
	iface, rest, ok := strings.Cut(v, "=")
	if !ok || !ifaceName.MatchString(iface) {
		return fmt.Errorf("--peer %q: use IFACE=PUBLICKEY=host:port", v)
	}
	// A key is base64 and may end in "="; the endpoint follows the last "=".
	i := strings.LastIndex(rest, "=")
	if i < 0 {
		return fmt.Errorf("--peer %q: use IFACE=PUBLICKEY=host:port", v)
	}
	if rest[:i] == "" || rest[i+1:] == "" {
		return fmt.Errorf("--peer %q: use IFACE=PUBLICKEY=host:port", v)
	}
	before := len(r.Problems)
	r.Add(iface, rest[:i], rest[i+1:], "--peer")
	if len(r.Problems) > before {
		p := r.Problems[len(r.Problems)-1]
		r.Problems = r.Problems[:before]
		return fmt.Errorf("--peer %q: %s", v, p.Msg)
	}
	return nil
}
