// Package plan works out what the daemon will try for a pull, in order:
// for docker.io its mirrors and then Docker Hub, for anything else the
// registry itself; each with the scheme, proxy and certificates the daemon
// would use.
package plan

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ganeshphutane-g/pullwhy/internal/engine"
	"github.com/ganeshphutane-g/pullwhy/internal/ref"
	"golang.org/x/net/http/httpproxy"
)

// HubHost is where Docker Hub's registry API is.
const HubHost = "registry-1.docker.io"

// CertsRoot is where the daemon looks for registry certificates.
var CertsRoot = "/etc/docker/certs.d"

// Endpoint is one place the daemon will try.
type Endpoint struct {
	// Base is scheme://host[:port][/path], without a trailing slash.
	Base string
	Host string // host[:port], as the daemon names it (certs.d uses this)
	// Mirror is set for registry-mirrors entries.
	Mirror bool
	// Insecure: the daemon accepts plain HTTP and unverified TLS here
	// (insecure-registries, or an address in InsecureRegistryCIDRs).
	Insecure bool
	// InsecureWhy says which setting made it insecure.
	InsecureWhy string
	// Proxy is the proxy the daemon uses for Base (nil: direct).
	Proxy *url.URL
	// CertsDir is the certs.d directory the daemon reads for this endpoint
	// (CertsPath), and CertFiles what is in it, when it exists (only seen
	// when the daemon is on this machine).
	CertsDir  string
	CertFiles []string
	// CertsPath is where the daemon would read certificates for this
	// endpoint, whether or not it exists ("" when it reads none: Docker Hub
	// on the classic store).
	CertsPath string
	// ProxyMasked: the daemon hides the proxy's password (as "xxxxx"), so
	// probes through it cannot log in to it.
	ProxyMasked bool
}

// Plan is the whole of it.
type Plan struct {
	Ref       *ref.Ref
	Endpoints []Endpoint
	// Containerd is the daemon's image store: containerd (true) or classic.
	Containerd bool
	// CertsRoot is the daemon's certs.d (rootless daemons keep their own).
	CertsRoot string
}

// Resolver looks up host addresses (net.DefaultResolver.LookupNetIP in use).
type Resolver func(ctx context.Context, host string) ([]netip.Addr, error)

// Make builds the plan for r from the daemon's /info.
func Make(ctx context.Context, r *ref.Ref, info *engine.Info, lookup Resolver, local bool) (*Plan, error) {
	p := &Plan{Ref: r, Containerd: info.ContainerdStore(), CertsRoot: CertsRoot}
	if info.Rootless() {
		p.CertsRoot = RootlessCertsRoot()
	}
	proxy := ProxyFunc(info)
	var cidrs []netip.Prefix
	for _, c := range info.RegistryConfig.InsecureRegistryCIDRs {
		if pfx, err := netip.ParsePrefix(c); err == nil {
			cidrs = append(cidrs, pfx)
		}
	}
	add := func(base string, mirror bool) error {
		u, err := url.Parse(strings.TrimSuffix(base, "/"))
		if err != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("the daemon's endpoint %q is not an http(s) URL", base)
		}
		e := Endpoint{Base: u.String(), Host: u.Host, Mirror: mirror}
		hub := u.Host == HubHost
		// As the daemon decides: Docker Hub is always verified; a registry
		// with its own entry (insecure-registries by name) uses that entry;
		// only others are matched against the insecure ranges.
		key := u.Host
		if hub {
			key = ref.DockerHub
		}
		if ic, ok := info.RegistryConfig.IndexConfigs[key]; ok {
			if !ic.Secure {
				e.Insecure, e.InsecureWhy = true, "insecure-registries lists "+u.Host
			}
		} else if why := inCIDRs(ctx, u.Hostname(), cidrs, lookup); why != "" && !hub {
			e.Insecure, e.InsecureWhy = true, why
		}
		if pu, err := proxy(u); err == nil && pu != nil {
			e.Proxy = pu
			if pw, ok := pu.User.Password(); ok && pw == "xxxxx" {
				e.ProxyMasked = true
			}
		}
		// Docker Hub's certificates: the containerd store reads
		// certs.d/docker.io; the classic store reads none (both tested).
		switch {
		case hub && p.Containerd:
			e.CertsPath = filepath.Join(p.CertsRoot, ref.DockerHub)
		case hub:
			e.CertsPath = ""
		default:
			e.CertsPath = filepath.Join(p.CertsRoot, u.Host)
		}
		if local && e.CertsPath != "" {
			dir := e.CertsPath
			if entries, err := os.ReadDir(dir); err == nil {
				e.CertsDir = dir
				for _, f := range entries {
					e.CertFiles = append(e.CertFiles, f.Name())
				}
				sort.Strings(e.CertFiles)
			}
		}
		p.Endpoints = append(p.Endpoints, e)
		return nil
	}
	if r.Registry == ref.DockerHub {
		mirrors := info.RegistryConfig.Mirrors
		if ic, ok := info.RegistryConfig.IndexConfigs[ref.DockerHub]; ok && len(ic.Mirrors) > 0 {
			mirrors = ic.Mirrors
		}
		for _, m := range mirrors {
			if err := add(m, true); err != nil {
				return nil, err
			}
		}
		if err := add("https://"+HubHost, false); err != nil {
			return nil, err
		}
		return p, nil
	}
	// The daemon tries HTTPS first; for an insecure registry it then also
	// tries plain HTTP.
	if err := add("https://"+r.Registry, false); err != nil {
		return nil, err
	}
	if p.Endpoints[0].Insecure {
		if err := add("http://"+r.Registry, false); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// inCIDRs says why host is insecure by address, or "".
func inCIDRs(ctx context.Context, host string, cidrs []netip.Prefix, lookup Resolver) string {
	if len(cidrs) == 0 {
		return ""
	}
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		addrs = []netip.Addr{a}
	} else if lookup != nil {
		addrs, _ = lookup(ctx, host)
	}
	for _, a := range addrs {
		for _, c := range cidrs {
			if c.Contains(a.Unmap()) {
				return fmt.Sprintf("%s is in the daemon's insecure range %s", a.Unmap(), c)
			}
		}
	}
	return ""
}

// RootlessCertsRoot is a rootless daemon's certs.d:
// $XDG_CONFIG_HOME/docker/certs.d, or ~/.config/docker/certs.d.
func RootlessCertsRoot() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "docker", "certs.d")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "docker", "certs.d")
}

// ProxyFunc picks the daemon's proxy for a URL, by the same rules as the
// daemon (Go's proxy environment: never for localhost and loopback).
func ProxyFunc(info *engine.Info) func(*url.URL) (*url.URL, error) {
	pc := &httpproxy.Config{HTTPProxy: info.HTTPProxy, HTTPSProxy: info.HTTPSProxy, NoProxy: info.NoProxy}
	return pc.ProxyFunc()
}

// DefaultResolver is the system's.
func DefaultResolver(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}
