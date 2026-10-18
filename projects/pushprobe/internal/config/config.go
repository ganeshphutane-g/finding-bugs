// Package config reads and checks a pushprobe file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is a whole file.
type Config struct {
	// Listen, if set, serves /healthz and /metrics (host:port).
	Listen   string   `yaml:"listen"`
	Defaults Defaults `yaml:"defaults"`
	Targets  Targets  `yaml:"targets"`
	Checks   []Check  `yaml:"checks"`
}

// Defaults apply to every check that does not set its own.
type Defaults struct {
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
	Retries  *int     `yaml:"retries"`
}

// Targets are where results go.
type Targets struct {
	Kuma  *Kuma  `yaml:"kuma"`
	Gatus *Gatus `yaml:"gatus"`
}

// Kuma is an Uptime Kuma server.
type Kuma struct {
	URL      string `yaml:"url"`
	Insecure bool   `yaml:"insecure"`
}

// Gatus is a Gatus server.
type Gatus struct {
	URL      string `yaml:"url"`
	Token    string `yaml:"token"`
	Insecure bool   `yaml:"insecure"`
}

// Check is one check and where its result goes.
type Check struct {
	Name     string   `yaml:"name"`
	Interval Duration `yaml:"interval"`
	Timeout  Duration `yaml:"timeout"`
	Retries  *int     `yaml:"retries"`

	HTTP   *HTTP   `yaml:"http"`
	TCP    *TCP    `yaml:"tcp"`
	DNS    *DNS    `yaml:"dns"`
	TLS    *TLS    `yaml:"tls"`
	Disk   *Disk   `yaml:"disk"`
	Memory *Memory `yaml:"memory"`

	Kuma  *KumaRoute  `yaml:"kuma"`
	Gatus *GatusRoute `yaml:"gatus"`

	// Line is where the check starts in the file.
	Line int `yaml:"-"`
}

// HTTP fetches a URL.
type HTTP struct {
	URL             string            `yaml:"url"`
	Method          string            `yaml:"method"`
	Headers         map[string]string `yaml:"headers"`
	ExpectStatus    []int             `yaml:"expect_status"`
	Contains        string            `yaml:"contains"`
	FollowRedirects *bool             `yaml:"follow_redirects"`
	Insecure        bool              `yaml:"insecure"`
}

// TCP connects to an address.
type TCP struct {
	Address string `yaml:"address"`
}

// DNS asks a server for a record.
type DNS struct {
	Server string   `yaml:"server"`
	Query  string   `yaml:"query"`
	Type   string   `yaml:"type"`
	Expect []string `yaml:"expect"`
}

// TLS checks a certificate.
type TLS struct {
	Address     string `yaml:"address"`
	ServerName  string `yaml:"server_name"`
	MinDaysLeft *int   `yaml:"min_days_left"`
	Insecure    bool   `yaml:"insecure"`
}

// Disk checks how full a file system is.
type Disk struct {
	Path           string  `yaml:"path"`
	MaxUsedPercent float64 `yaml:"max_used_percent"`
}

// Memory checks how much memory is in use.
type Memory struct {
	MaxUsedPercent float64 `yaml:"max_used_percent"`
}

// KumaRoute sends the result to a Kuma push monitor.
type KumaRoute struct {
	PushToken string `yaml:"push_token"`
}

// GatusRoute sends the result to a Gatus external endpoint.
type GatusRoute struct {
	// Endpoint is the endpoint's key, as Gatus shows it: group_name.
	Endpoint string `yaml:"endpoint"`
	// Token overrides the target's token.
	Token string `yaml:"token"`
}

// Built-in defaults.
const (
	DefaultInterval = 60 * time.Second
	DefaultTimeout  = 10 * time.Second
	DefaultRetries  = 1
	MinInterval     = 5 * time.Second
	DefaultMinDays  = 14
)

// Kind is the check's type.
func (c *Check) Kind() string {
	switch {
	case c.HTTP != nil:
		return "http"
	case c.TCP != nil:
		return "tcp"
	case c.DNS != nil:
		return "dns"
	case c.TLS != nil:
		return "tls"
	case c.Disk != nil:
		return "disk"
	case c.Memory != nil:
		return "memory"
	}
	return ""
}

// Mode says what a file is loaded for.
type Mode int

const (
	// ForRun needs everything, including where results go.
	ForRun Mode = iota
	// ForCheck runs the checks only: targets, routes and their variables
	// may be missing.
	ForCheck
)

// Lookup returns an environment variable.
type Lookup func(string) (string, bool)

// Load reads a file, fills ${VAR} references and checks it. Every problem is
// reported, each with its line where there is one.
func Load(data []byte, lookup Lookup, mode Mode) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the file is empty")
		}
		return nil, fmt.Errorf("the file cannot be read: %s", tidyYAMLError(err))
	}
	var root yaml.Node
	_ = yaml.Unmarshal(data, &root)
	for i, line := range checkLines(&root) {
		if i < len(c.Checks) {
			c.Checks[i].Line = line
		}
	}

	var problems []string
	missing := map[string]bool{}
	expandAll(&c, lookup, mode, missing)
	if len(missing) > 0 {
		var names []string
		for n := range missing {
			names = append(names, n)
		}
		sort.Strings(names)
		problems = append(problems, "no value for "+strings.Join(names, ", ")+" (set them in the environment)")
	}
	problems = append(problems, c.validate(mode)...)
	if len(problems) > 0 {
		return nil, fmt.Errorf("the file has problems:\n  - %s", strings.Join(problems, "\n  - "))
	}
	c.applyDefaults()
	return &c, nil
}

var yamlLine = regexp.MustCompile(`yaml: (unmarshal errors:\n\s*)?`)

func tidyYAMLError(err error) string {
	return strings.TrimSpace(yamlLine.ReplaceAllString(err.Error(), ""))
}

// checkLines finds the line of each item under checks:.
func checkLines(root *yaml.Node) []int {
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil
	}
	m := root.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "checks" && m.Content[i+1].Kind == yaml.SequenceNode {
			var out []int
			for _, item := range m.Content[i+1].Content {
				out = append(out, item.Line)
			}
			return out
		}
	}
	return nil
}

func (c *Config) applyDefaults() {
	for i := range c.Checks {
		ch := &c.Checks[i]
		if ch.Interval == 0 {
			ch.Interval = c.Defaults.Interval
		}
		if ch.Interval == 0 {
			ch.Interval = Duration(DefaultInterval)
		}
		if ch.Timeout == 0 {
			ch.Timeout = c.Defaults.Timeout
		}
		if ch.Timeout == 0 {
			ch.Timeout = Duration(min(DefaultTimeout, time.Duration(ch.Interval)/2))
		}
		if ch.Retries == nil {
			ch.Retries = c.Defaults.Retries
		}
		if ch.Retries == nil {
			n := DefaultRetries
			ch.Retries = &n
		}
		if ch.HTTP != nil && ch.HTTP.Method == "" {
			ch.HTTP.Method = "GET"
		}
		if ch.DNS != nil && ch.DNS.Type == "" {
			ch.DNS.Type = "A"
		}
		if ch.TLS != nil && ch.TLS.MinDaysLeft == nil {
			n := DefaultMinDays
			ch.TLS.MinDaysLeft = &n
		}
	}
}

func (c *Config) validate(mode Mode) []string {
	var problems []string
	add := func(ch *Check, f string, a ...any) {
		where := "checks"
		if ch != nil {
			where = fmt.Sprintf("line %d: check %q", ch.Line, ch.Name)
		}
		problems = append(problems, where+": "+fmt.Sprintf(f, a...))
	}
	if len(c.Checks) == 0 {
		problems = append(problems, "there are no checks")
	}
	if c.Defaults.Interval != 0 && time.Duration(c.Defaults.Interval) < MinInterval {
		problems = append(problems, fmt.Sprintf("defaults: interval must be at least %s", MinInterval))
	}
	if c.Listen != "" {
		// (An empty host listens everywhere.)
		if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
			problems = append(problems, fmt.Sprintf("listen: %q: expected host:port, such as 127.0.0.1:9469", c.Listen))
		}
	}
	if c.Defaults.Retries != nil && *c.Defaults.Retries < 0 {
		problems = append(problems, "defaults: retries must not be negative")
	}
	if mode == ForRun {
		if t := c.Targets.Kuma; t != nil {
			if err := checkURL(t.URL); err != nil {
				problems = append(problems, "targets.kuma.url: "+err.Error())
			}
		}
		if t := c.Targets.Gatus; t != nil {
			if err := checkURL(t.URL); err != nil {
				problems = append(problems, "targets.gatus.url: "+err.Error())
			}
		}
	}
	seen := map[string]int{}
	for i := range c.Checks {
		ch := &c.Checks[i]
		if strings.TrimSpace(ch.Name) == "" {
			problems = append(problems, fmt.Sprintf("line %d: a check has no name", ch.Line))
		} else if first, ok := seen[ch.Name]; ok {
			add(ch, "the name is used twice (also on line %d)", first)
		} else {
			seen[ch.Name] = ch.Line
		}
		kinds := 0
		for _, set := range []bool{ch.HTTP != nil, ch.TCP != nil, ch.DNS != nil, ch.TLS != nil, ch.Disk != nil, ch.Memory != nil} {
			if set {
				kinds++
			}
		}
		switch {
		case kinds == 0:
			add(ch, "say what to check: http, tcp, dns, tls, disk or memory")
		case kinds > 1:
			add(ch, "has more than one of http, tcp, dns, tls, disk and memory; use one check for each")
		}
		interval, timeout := time.Duration(ch.Interval), time.Duration(ch.Timeout)
		if interval == 0 {
			interval = time.Duration(c.Defaults.Interval)
		}
		if timeout == 0 {
			timeout = time.Duration(c.Defaults.Timeout)
		}
		if ch.Interval != 0 && interval < MinInterval {
			add(ch, "interval must be at least %s", MinInterval)
		}
		if interval != 0 && timeout != 0 && timeout >= interval {
			add(ch, "the timeout (%s) must be shorter than the interval (%s)", timeout, interval)
		}
		if ch.Retries != nil && *ch.Retries < 0 {
			add(ch, "retries must not be negative")
		}
		for _, p := range ch.checkProblems() {
			add(ch, "%s", p)
		}
		if mode == ForRun {
			if ch.Kuma == nil && ch.Gatus == nil {
				add(ch, "the result goes nowhere: add kuma: {push_token: ...} or gatus: {endpoint: ...}")
			}
			if ch.Kuma != nil {
				if c.Targets.Kuma == nil {
					add(ch, "reports to Kuma, but targets has no kuma")
				}
				if !pushToken.MatchString(ch.Kuma.PushToken) {
					add(ch, "kuma.push_token must be the push monitor's token (letters and digits, from its push URL)")
				}
			}
			if ch.Gatus != nil {
				if c.Targets.Gatus == nil {
					add(ch, "reports to Gatus, but targets has no gatus")
				} else if ch.Gatus.Token == "" && c.Targets.Gatus.Token == "" {
					add(ch, "gatus needs a token (targets.gatus.token or the check's gatus.token)")
				}
				if !gatusKey.MatchString(ch.Gatus.Endpoint) {
					add(ch, "gatus.endpoint must be the external endpoint's key, such as office_printer")
				}
			}
		}
	}
	return problems
}

var (
	pushToken = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	gatusKey  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,200}$`)
)

func (ch *Check) checkProblems() []string {
	var out []string
	switch {
	case ch.HTTP != nil:
		h := ch.HTTP
		if err := checkURL(h.URL); err != nil {
			out = append(out, "http.url: "+err.Error())
		}
		for _, s := range h.ExpectStatus {
			if s < 100 || s > 599 {
				out = append(out, fmt.Sprintf("http.expect_status: %d is not an HTTP status", s))
			}
		}
		switch strings.ToUpper(h.Method) {
		case "", "GET", "HEAD", "POST", "PUT", "OPTIONS":
		default:
			out = append(out, fmt.Sprintf("http.method %q: use GET, HEAD, POST, PUT or OPTIONS", h.Method))
		}
	case ch.TCP != nil:
		if err := checkAddress(ch.TCP.Address); err != nil {
			out = append(out, "tcp.address: "+err.Error())
		}
	case ch.DNS != nil:
		d := ch.DNS
		if d.Query == "" {
			out = append(out, "dns.query: the name to look up is missing")
		}
		switch strings.ToUpper(d.Type) {
		case "", "A", "AAAA", "CNAME", "MX", "TXT", "NS":
		default:
			out = append(out, fmt.Sprintf("dns.type %q: use A, AAAA, CNAME, MX, TXT or NS", d.Type))
		}
		if d.Server != "" {
			server := d.Server
			if _, _, err := net.SplitHostPort(server); err != nil {
				server = net.JoinHostPort(server, "53")
			}
			if err := checkAddress(server); err != nil {
				out = append(out, "dns.server: "+err.Error())
			}
		}
	case ch.TLS != nil:
		if err := checkAddress(ch.TLS.Address); err != nil {
			out = append(out, "tls.address: "+err.Error())
		}
		if m := ch.TLS.MinDaysLeft; m != nil && *m < 0 {
			out = append(out, "tls.min_days_left must not be negative")
		}
	case ch.Disk != nil:
		if !strings.HasPrefix(ch.Disk.Path, "/") && !(runtime.GOOS == "windows" && len(ch.Disk.Path) >= 2 && ch.Disk.Path[1] == ':') {
			out = append(out, "disk.path must be an absolute path")
		}
		if p := ch.Disk.MaxUsedPercent; p <= 0 || p > 100 {
			out = append(out, "disk.max_used_percent must be between 0 and 100")
		}
	case ch.Memory != nil:
		if p := ch.Memory.MaxUsedPercent; p <= 0 || p > 100 {
			out = append(out, "memory.max_used_percent must be between 0 and 100")
		}
	}
	return out
}

func checkURL(s string) error {
	if s == "" {
		return errors.New("missing")
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an http:// or https:// address", s)
	}
	return nil
}

func checkAddress(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("%q: expected host:port", s)
	}
	return nil
}
