// Package config reads alertlines' file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole file.
type Config struct {
	// Listen is where the ruler sends alerts (host:port).
	Listen string `yaml:"listen"`
	// Alertmanagers get every alert.
	Alertmanagers []string `yaml:"alertmanagers"`
	Loki          Loki     `yaml:"loki"`
	Lines         Lines    `yaml:"lines"`
	// Range is how far back to look when the expression does not say.
	Range Duration `yaml:"range"`
	// Timeout bounds the wait for Loki per batch of alerts.
	Timeout Duration `yaml:"timeout"`
	// Cache keeps the lines of a firing alert this long, as the ruler sends
	// it again at every evaluation.
	Cache      Duration `yaml:"cache"`
	Redact     Redact   `yaml:"redact"`
	Annotation string   `yaml:"annotation"`
}

// Loki is where lines come from.
type Loki struct {
	URL         string `yaml:"url"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	BearerToken string `yaml:"bearer_token"`
	Insecure    bool   `yaml:"insecure"`
	// Tenant, if set, is used instead of the ruler's X-Scope-OrgID.
	Tenant string `yaml:"tenant"`
}

// Lines are the limits.
type Lines struct {
	Max          int      `yaml:"max"`
	MaxLineBytes int      `yaml:"max_line_bytes"`
	MaxBytes     int      `yaml:"max_bytes"`
	Labels       []string `yaml:"labels"`
}

// Redact says what is hidden.
type Redact struct {
	Builtin  *bool    `yaml:"builtin"`
	Patterns []string `yaml:"patterns"`
}

// Defaults.
const (
	DefaultListen  = ":9097"
	DefaultTimeout = 3 * time.Second
	DefaultCache   = time.Minute
	DefaultRange   = 5 * time.Minute
)

// Default is the configuration with nothing set.
func Default() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.Timeout == 0 {
		c.Timeout = Duration(DefaultTimeout)
	}
	if c.Cache == 0 {
		c.Cache = Duration(DefaultCache)
	}
	if c.Range == 0 {
		c.Range = Duration(DefaultRange)
	}
	if c.Lines.Max == 0 {
		c.Lines.Max = 10
	}
	if c.Lines.MaxLineBytes == 0 {
		c.Lines.MaxLineBytes = 300
	}
	if c.Lines.MaxBytes == 0 {
		c.Lines.MaxBytes = 4000
	}
	if c.Annotation == "" {
		c.Annotation = "log_lines"
	}
	if c.Redact.Builtin == nil {
		on := true
		c.Redact.Builtin = &on
	}
}

var ref = regexp.MustCompile(`\$\$\{|\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads and checks a file. needAlertmanager is for running the proxy.
func Load(data []byte, lookup func(string) (string, bool), needAlertmanager bool) (*Config, error) {
	c, err := Parse(data, lookup)
	if err != nil {
		return nil, err
	}
	if problems := c.Check(needAlertmanager); len(problems) > 0 {
		return nil, fmt.Errorf("the file has problems:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return c, nil
}

// Parse reads a file without checking it (see Check). ${NAME} is filled from
// lookup, for names starting with ALERTLINES_ only.
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
			if !strings.HasPrefix(name, "ALERTLINES_") {
				problems = append(problems, fmt.Sprintf("${%s}: only ALERTLINES_* variables can be used", name))
				return ""
			}
			v, ok := lookup(name)
			if !ok || v == "" {
				problems = append(problems, fmt.Sprintf("no value for %s", name))
			}
			return v
		})
	}
	for _, s := range []*string{&c.Listen, &c.Loki.URL, &c.Loki.Username, &c.Loki.Password, &c.Loki.BearerToken, &c.Loki.Tenant, &c.Annotation} {
		fill(s)
	}
	for i := range c.Alertmanagers {
		fill(&c.Alertmanagers[i])
	}
	c.applyDefaults()
	if len(problems) > 0 {
		return nil, fmt.Errorf("the file has problems:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return &c, nil
}

// Check lists what is wrong.
func (c *Config) Check(needAlertmanager bool) []string {
	var out []string
	if err := checkURL(c.Loki.URL); err != nil {
		out = append(out, "loki.url: "+err.Error())
	}
	if needAlertmanager {
		if len(c.Alertmanagers) == 0 {
			out = append(out, "alertmanagers: name at least one, such as http://alertmanager:9093")
		}
		for _, a := range c.Alertmanagers {
			if err := checkURL(a); err != nil {
				out = append(out, "alertmanagers: "+err.Error())
			}
		}
		if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
			out = append(out, fmt.Sprintf("listen: %q: expected host:port, such as :9097", c.Listen))
		}
	}
	if c.Lines.Max < 0 || c.Lines.Max > 100 {
		out = append(out, "lines.max must be between 1 and 100")
	}
	if c.Lines.MaxLineBytes < 20 || c.Lines.MaxBytes < 100 {
		out = append(out, "lines.max_line_bytes must be at least 20 and lines.max_bytes at least 100")
	}
	if time.Duration(c.Timeout) > 30*time.Second {
		out = append(out, "timeout: at most 30s (alerts wait that long)")
	}
	if !validName(c.Annotation) {
		out = append(out, fmt.Sprintf("annotation %q: use a name such as log_lines", c.Annotation))
	}
	if c.Loki.Username != "" && c.Loki.BearerToken != "" {
		out = append(out, "loki: use username and password, or bearer_token, not both")
	}
	return out
}

func validName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func checkURL(s string) error {
	if s == "" {
		return errors.New("missing")
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q is not an http:// or https:// address", RedactURL(s))
	}
	return nil
}

// RedactURL hides a password in a URL.
func RedactURL(s string) string {
	if u, err := url.Parse(s); err == nil && u.User != nil {
		return u.Redacted()
	}
	// (A URL that does not parse may still hold one.)
	if i, at := strings.Index(s, "://"), strings.LastIndex(s, "@"); i >= 0 && at > i {
		return s[:i+3] + "xxxxx" + s[at:]
	}
	return s
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
		return fmt.Errorf("line %d: %q is not a duration such as 3s or 5m", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}
