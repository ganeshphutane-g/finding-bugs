// Package source finds WireGuard peers whose endpoint is a DNS name, in the
// files the usual tools are configured with: wg-quick confs,
// systemd-networkd .netdev files and NetworkManager keyfiles.
package source

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Peer is a peer configured with a name for its endpoint.
type Peer struct {
	Interface string
	PublicKey wgtypes.Key
	Host      string
	Port      int
	// Source is where it was found ("/etc/wireguard/wg0.conf:12", "--peer").
	Source string
}

// Endpoint is host:port as configured.
func (p Peer) Endpoint() string { return net.JoinHostPort(p.Host, strconv.Itoa(p.Port)) }

// Problem is something in a file that could not be used; the rest of the
// file still is.
type Problem struct {
	Source string
	Msg    string
}

func (p Problem) String() string { return p.Source + ": " + p.Msg }

// Result is what was found.
type Result struct {
	Peers []Peer
	// Fixed counts peers whose endpoint is an IP address (left alone).
	Fixed    int
	Problems []Problem
}

func (r *Result) problem(src, format string, a ...any) {
	r.Problems = append(r.Problems, Problem{Source: src, Msg: fmt.Sprintf(format, a...)})
}

// Add records a peer if its endpoint is a name. A peer already found (the
// same interface and key) is kept as first found.
func (r *Result) Add(iface, key, endpoint, src string) {
	if iface == "" || key == "" || endpoint == "" {
		return
	}
	k, err := wgtypes.ParseKey(key)
	if err != nil {
		r.problem(src, "public key %q is not a WireGuard key", key)
		return
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		r.problem(src, "endpoint %q is not host:port", endpoint)
		return
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		r.problem(src, "endpoint %q has no valid port", endpoint)
		return
	}
	if _, err := netip.ParseAddr(host); err == nil {
		r.Fixed++
		return
	}
	host = strings.TrimSuffix(host, ".")
	if !validHost(host) {
		r.problem(src, "endpoint %q has no valid host name", endpoint)
		return
	}
	for _, p := range r.Peers {
		if p.Interface == iface && p.PublicKey == k {
			if p.Endpoint() != net.JoinHostPort(host, port) {
				r.problem(src, "peer %s on %s is also configured in %s, with %s; using that", short(key), iface, p.Source, p.Endpoint())
			}
			return
		}
	}
	r.Peers = append(r.Peers, Peer{Interface: iface, PublicKey: k, Host: host, Port: n, Source: src})
}

func short(key string) string {
	if len(key) > 8 {
		return key[:8] + "…"
	}
	return key
}

// validHost: letters, digits, hyphens and dots, as DNS names are.
func validHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// entry is one key = value line of an INI-like file.
type entry struct {
	key, value string
	no         int
}

// section is one [name] block and its entries, in file order. A repeated
// name ([Peer] twice) is two sections.
type section struct {
	name    string
	no      int
	entries []entry
}

// exact is get with the key's case as written (networkd, NetworkManager).
func (s section) exact(key string) string {
	v := ""
	for _, e := range s.entries {
		if e.key == key {
			v = e.value
		}
	}
	return v
}

// get looks a key up ignoring case (wg-quick).
func (s section) get(key string) string {
	v := ""
	for _, e := range s.entries {
		if strings.EqualFold(e.key, key) {
			v = e.value // the last one wins, as in all three tools
		}
	}
	return v
}

// readINI reads sections and key = value lines. inlineComments: a # starts
// a comment anywhere on a line (wg-quick); otherwise only at its start.
func readINI(r io.Reader, inlineComments bool) ([]section, error) {
	out := []section{{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	no := 0
	for sc.Scan() {
		no++
		s := sc.Text()
		if inlineComments {
			s, _, _ = strings.Cut(s, "#")
		}
		s = strings.TrimSpace(s)
		if s == "" || s[0] == '#' || s[0] == ';' {
			continue
		}
		if s[0] == '[' && s[len(s)-1] == ']' {
			out = append(out, section{name: strings.TrimSpace(s[1 : len(s)-1]), no: no})
			continue
		}
		k, v, ok := strings.Cut(s, "=")
		if !ok {
			continue
		}
		cur := &out[len(out)-1]
		cur.entries = append(cur.entries, entry{key: strings.TrimSpace(k), value: strings.TrimSpace(v), no: no})
	}
	return out, sc.Err()
}

// parseFile reads an INI-like file; a missing file is nothing, an
// unreadable one a problem.
func parseFile(path string, inline bool, r *Result) ([]section, bool, error) {
	// Only regular files (through symlinks): a FIFO would block the loop.
	st, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if _, lerr := os.Lstat(path); lerr == nil {
			r.problem(path, "is a symlink to nothing")
		}
		return nil, false, nil
	}
	if err == nil && !st.Mode().IsRegular() {
		r.problem(path, "is not a regular file")
		return nil, false, nil
	}
	if err == nil && st.Size() > 1<<20 {
		r.problem(path, "is larger than 1 MiB; not read")
		return nil, false, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			r.problem(path, "cannot be read (wgresolve needs to run as root)")
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()
	secs, err := readINI(f, inline)
	if err != nil {
		r.problem(path, "%v", err)
		return nil, false, nil
	}
	return secs, true, nil
}
