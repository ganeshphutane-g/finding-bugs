// Package config reads xffdns' file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole file.
type Config struct {
	Upstream  Upstream   `yaml:"upstream"`
	Listeners []Listener `yaml:"listeners"`
	Limits    Limits     `yaml:"limits"`
	// MetricsListen, if set, serves /metrics and /healthz (host:port).
	MetricsListen string `yaml:"metrics_listen"`
}

// Upstream is AdGuard Home's DoH endpoint.
type Upstream struct {
	// URL is http(s)://host:port/dns-query. Plain http needs
	// http.doh.insecure_enabled in AdGuard Home.
	URL     string   `yaml:"url"`
	Timeout Duration `yaml:"timeout"`
	// CAFile and ServerName are for https with a private CA, or a name
	// other than the URL's host.
	CAFile     string `yaml:"ca_file"`
	ServerName string `yaml:"server_name"`
}

// Listener is one address xffdns takes queries on.
type Listener struct {
	// Name is for logs and metrics; it defaults to protocol/address.
	Name     string `yaml:"name"`
	Protocol string `yaml:"protocol"` // udp, tcp or dot
	Address  string `yaml:"address"`
	// CertFile and KeyFile are for dot; they are read again when they change.
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// ServerName, for dot, turns an SNI of <id>.<server_name> into client ID <id>.
	ServerName string `yaml:"server_name"`
	// ClientID, if set, is the client ID of every query on this listener.
	ClientID string `yaml:"client_id"`
	// ProxyProtocol makes a PROXY header required, from ProxyFrom only.
	ProxyProtocol bool     `yaml:"proxy_protocol"`
	ProxyFrom     []string `yaml:"proxy_from"`

	// Trusted is ProxyFrom, parsed.
	Trusted []netip.Prefix `yaml:"-"`
}

// Limits keep xffdns and AdGuard Home from being overrun.
type Limits struct {
	// UDPQPS is queries per second per client subnet over UDP (0: none).
	UDPQPS *int `yaml:"udp_qps"`
	// SubnetV4 and SubnetV6 are the subnet sizes UDPQPS counts by.
	SubnetV4 int `yaml:"subnet_v4"`
	SubnetV6 int `yaml:"subnet_v6"`
	// Clients is how many client subnets the limiter keeps track of.
	Clients            int      `yaml:"clients"`
	MaxConns           int      `yaml:"max_conns"`
	MaxInflightPerConn int      `yaml:"max_inflight_per_conn"`
	MaxInflight        int      `yaml:"max_inflight"`
	IdleTimeout        Duration `yaml:"idle_timeout"`
	HandshakeTimeout   Duration `yaml:"handshake_timeout"`
}

// Defaults.
const (
	DefaultTimeout   = 4 * time.Second
	DefaultUDPQPS    = 50
	DefaultIdle      = 30 * time.Second
	DefaultHandshake = 5 * time.Second
)

func (c *Config) applyDefaults() {
	if c.Upstream.Timeout == 0 {
		c.Upstream.Timeout = Duration(DefaultTimeout)
	}
	l := &c.Limits
	if l.UDPQPS == nil {
		n := DefaultUDPQPS
		l.UDPQPS = &n
	}
	def := func(p *int, v int) {
		if *p == 0 {
			*p = v
		}
	}
	def(&l.SubnetV4, 24)
	def(&l.SubnetV6, 56)
	def(&l.Clients, 10000)
	def(&l.MaxConns, 1000)
	def(&l.MaxInflightPerConn, 32)
	def(&l.MaxInflight, 2000)
	if l.IdleTimeout == 0 {
		l.IdleTimeout = Duration(DefaultIdle)
	}
	if l.HandshakeTimeout == 0 {
		l.HandshakeTimeout = Duration(DefaultHandshake)
	}
	for i := range c.Listeners {
		ls := &c.Listeners[i]
		ls.Protocol = strings.ToLower(ls.Protocol)
		if ls.Name == "" {
			ls.Name = ls.Protocol + "/" + ls.Address
		}
		ls.ServerName = strings.TrimSuffix(strings.ToLower(ls.ServerName), ".")
	}
}

var ref = regexp.MustCompile(`\$\$\{|\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads and checks a file.
func Load(data []byte, lookup func(string) (string, bool)) (*Config, error) {
	c, err := Parse(data, lookup)
	if err != nil {
		return nil, err
	}
	if problems := c.Check(); len(problems) > 0 {
		return nil, fmt.Errorf("the file has problems:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return c, nil
}

// LoadFile reads and checks a file from disk, with the process environment.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Load(data, os.LookupEnv)
}

// Parse reads a file without checking it (see Check). ${NAME} is filled from
// lookup, for names starting with XFFDNS_ only.
func Parse(data []byte, lookup func(string) (string, bool)) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("the file cannot be read: %s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var problems []string
	fill := func(s *string) {
		*s = ref.ReplaceAllStringFunc(*s, func(m string) string {
			if m == "$${" {
				return "${"
			}
			name := m[2 : len(m)-1]
			if !strings.HasPrefix(name, "XFFDNS_") {
				problems = append(problems, fmt.Sprintf("${%s}: only XFFDNS_* variables can be used", name))
				return ""
			}
			v, ok := lookup(name)
			if !ok || v == "" {
				problems = append(problems, fmt.Sprintf("no value for %s", name))
			}
			return v
		})
	}
	fill(&c.Upstream.URL)
	fill(&c.Upstream.CAFile)
	fill(&c.Upstream.ServerName)
	fill(&c.MetricsListen)
	for i := range c.Listeners {
		l := &c.Listeners[i]
		for _, s := range []*string{&l.Address, &l.CertFile, &l.KeyFile, &l.ServerName, &l.ClientID} {
			fill(s)
		}
	}
	c.applyDefaults()
	if len(problems) > 0 {
		return nil, fmt.Errorf("the file has problems:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return &c, nil
}

// Check lists what is wrong, and parses proxy_from.
func (c *Config) Check() []string {
	var out []string
	add := func(f string, a ...any) { out = append(out, fmt.Sprintf(f, a...)) }
	if u, err := url.Parse(c.Upstream.URL); c.Upstream.URL == "" {
		add("upstream.url: missing, such as http://127.0.0.1:80/dns-query")
	} else if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		add("upstream.url: %q is not an http:// or https:// address with no user, query or fragment", c.Upstream.URL)
	} else if u.Scheme == "http" && (c.Upstream.CAFile != "" || c.Upstream.ServerName != "") {
		add("upstream: ca_file and server_name are for https:// only")
	}
	if t := time.Duration(c.Upstream.Timeout); t > 30*time.Second {
		add("upstream.timeout: at most 30s (clients give up long before)")
	}
	if c.MetricsListen != "" && !hostPort(c.MetricsListen) {
		add("metrics_listen: %q: expected host:port, such as 127.0.0.1:9354", c.MetricsListen)
	}
	if len(c.Listeners) == 0 {
		add("listeners: name at least one")
	}
	names := map[string]bool{}
	for i := range c.Listeners {
		l := &c.Listeners[i]
		at := fmt.Sprintf("listeners[%d] (%s)", i, l.Name)
		if names[l.Name] {
			add("%s: the name is used twice", at)
		}
		names[l.Name] = true
		switch l.Protocol {
		case "udp", "tcp":
			if l.CertFile != "" || l.KeyFile != "" || l.ServerName != "" {
				add("%s: cert_file, key_file and server_name are for dot only", at)
			}
		case "dot":
			if l.CertFile == "" || l.KeyFile == "" {
				add("%s: dot needs cert_file and key_file", at)
			}
			if l.ServerName != "" && !validHost(l.ServerName) {
				add("%s: server_name %q is not a host name", at, l.ServerName)
			}
			if l.ServerName != "" && l.ClientID != "" {
				add("%s: use server_name (client IDs from SNI) or client_id, not both", at)
			}
		default:
			add("%s: protocol %q: use udp, tcp or dot", at, l.Protocol)
		}
		if !hostPort(l.Address) {
			add("%s: address %q: expected host:port, such as :853", at, l.Address)
		}
		if l.ClientID != "" && !ValidClientID(l.ClientID) {
			add("%s: client_id %q: AdGuard Home allows lowercase letters, digits and -, at most 64", at, l.ClientID)
		}
		l.Trusted = nil
		for _, s := range l.ProxyFrom {
			p, err := parsePrefix(s)
			if err != nil {
				add("%s: proxy_from %q is not an address or CIDR", at, s)
				continue
			}
			l.Trusted = append(l.Trusted, p)
		}
		if l.ProxyProtocol && len(l.ProxyFrom) == 0 {
			add("%s: proxy_protocol needs proxy_from: the addresses of your proxies (anyone else could claim any address)", at)
		}
		if !l.ProxyProtocol && len(l.ProxyFrom) > 0 {
			add("%s: proxy_from is set but proxy_protocol is not", at)
		}
	}
	lm := c.Limits
	if *lm.UDPQPS < 0 || lm.SubnetV4 < 1 || lm.SubnetV4 > 32 || lm.SubnetV6 < 1 || lm.SubnetV6 > 128 ||
		lm.Clients < 1 || lm.MaxConns < 1 || lm.MaxInflightPerConn < 1 || lm.MaxInflight < 1 {
		add("limits: udp_qps must be 0 or more; subnet_v4 1-32, subnet_v6 1-128; the rest at least 1")
	}
	return out
}

func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func hostPort(s string) bool {
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n >= 0 && n < 65536 // 0: any free port
}

func validHost(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if l == "" || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
			return false
		}
		for _, c := range l {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// ValidClientID reports whether AdGuard Home takes s as a client ID: 1 to 64
// lowercase letters, digits and hyphens.
func ValidClientID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Duration is 30s, 5m or a number of seconds.
type Duration time.Duration

// UnmarshalYAML reads a duration.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if secs, err := strconv.Atoi(n.Value); err == nil && secs >= 0 {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	v, err := time.ParseDuration(n.Value)
	if err != nil || v <= 0 {
		return fmt.Errorf("line %d: %q is not a duration such as 4s or 1m", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}
