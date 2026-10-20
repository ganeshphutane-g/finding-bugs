package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoad(t *testing.T) {
	c, err := Load([]byte(`
upstream:
  url: "http://127.0.0.1:${XFFDNS_PORT}/dns-query"
listeners:
  - name: dot
    protocol: DoT
    address: ":853"
    cert_file: /c.pem
    key_file: /k.pem
    server_name: Dns.Example.com.
    proxy_protocol: true
    proxy_from: ["172.18.0.0/16", "${XFFDNS_PROXY}", "2001:db8::/64"]
  - protocol: udp
    address: "192.168.1.2:53"
    client_id: lan-1
limits: {udp_qps: 0, idle_timeout: 10}
`), env(map[string]string{"XFFDNS_PORT": "3080", "XFFDNS_PROXY": "10.0.0.7"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.URL != "http://127.0.0.1:3080/dns-query" || time.Duration(c.Upstream.Timeout) != DefaultTimeout {
		t.Fatalf("%+v", c.Upstream)
	}
	d := c.Listeners[0]
	if d.Protocol != "dot" || d.ServerName != "dns.example.com" || len(d.Trusted) != 3 || d.Trusted[1].String() != "10.0.0.7/32" {
		t.Fatalf("%+v", d)
	}
	if c.Listeners[1].Name != "udp/192.168.1.2:53" {
		t.Fatal(c.Listeners[1].Name)
	}
	if *c.Limits.UDPQPS != 0 || time.Duration(c.Limits.IdleTimeout) != 10*time.Second || c.Limits.MaxConns != 1000 || c.Limits.SubnetV6 != 56 {
		t.Fatalf("%+v", c.Limits)
	}
	// udp_qps left out: the default.
	c, err = Load([]byte("upstream: {url: 'https://agh/dns-query'}\nlisteners: [{protocol: tcp, address: ':53'}]\n"), env(nil))
	if err != nil || *c.Limits.UDPQPS != DefaultUDPQPS {
		t.Fatal(c, err)
	}
}

func TestProblems(t *testing.T) {
	for _, c := range []struct{ file, want string }{
		{"upstream: {url: x}\nlisteners: [{protocol: udp, address: ':53'}]", "upstream.url"},
		{"upstream: {url: 'http://a:b@agh/dns-query'}\nlisteners: [{protocol: udp, address: ':53'}]", "no user"},
		{"upstream: {url: 'http://agh/dns-query', ca_file: /ca}\nlisteners: [{protocol: udp, address: ':53'}]", "https:// only"},
		{"upstream: {url: 'http://agh/dns-query', timeout: 1m}\nlisteners: [{protocol: udp, address: ':53'}]", "at most 30s"},
		{"upstream: {url: 'http://agh/dns-query'}", "name at least one"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: doh, address: ':53'}]", "use udp, tcp or dot"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: udp, address: '53'}]", "expected host:port"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: dot, address: ':853'}]", "needs cert_file"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: udp, address: ':53', cert_file: x}]", "dot only"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: udp, address: ':53', client_id: Kid}]", "lowercase"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: udp, address: ':53', proxy_protocol: true}]", "needs proxy_from"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: udp, address: ':53', proxy_from: [10.0.0.1]}]", "proxy_protocol is not"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: udp, address: ':53', proxy_protocol: true, proxy_from: [10.0.0.300]}]", "not an address"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{name: a, protocol: udp, address: ':53'}, {name: a, protocol: tcp, address: ':53'}]", "used twice"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: dot, address: ':853', cert_file: c, key_file: k, server_name: 'a..b'}]", "not a host name"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: udp, address: ':53'}]\nlimits: {udp_qps: -1}", "udp_qps"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: udp, address: ':53'}]\nmetrics_listen: 9354", "metrics_listen"},
		{"upstream: {url: 'http://agh/dns-query', bogus: 1}", "bogus"},
		{"upstream: {url: '${HOME}'}", "only XFFDNS_"},
		{"upstream: {url: '${XFFDNS_NOPE}'}", "no value for XFFDNS_NOPE"},
		{"upstream: {timeout: soon}", "not a duration"},
		{"upstream: {timeout: 0}", "not a duration"},
		{"upstream: {url: 'http://127.0.0.1:80'}\nlisteners: [{protocol: udp, address: ':53'}]", "has no path"},
		{"upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: udp, address: ':53', proxy_protocol: true, proxy_from: ['::ffff:0:0/80']}]", "not an address"},
	} {
		_, err := Load([]byte(c.file), env(nil))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s\n got %v\nwant %q", c.file, err, c.want)
		}
	}
}

func TestMappedPrefixes(t *testing.T) {
	c, err := Load([]byte("upstream: {url: 'http://agh/dns-query'}\nlisteners: [{protocol: tcp, address: ':53', proxy_protocol: true, "+
		"proxy_from: ['::ffff:10.0.0.0/104', '::ffff:192.0.2.1', '2001:db8::/32']}]"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range c.Listeners[0].Trusted {
		got = append(got, p.String())
	}
	if strings.Join(got, " ") != "10.0.0.0/8 192.0.2.1/32 2001:db8::/32" {
		t.Fatal(got)
	}
}

func TestClientID(t *testing.T) {
	for s, ok := range map[string]bool{"kid": true, "living-room-tv": true, "a1": true, "": false, "Kid": false, "a_b": false,
		strings.Repeat("a", 64): true, strings.Repeat("a", 65): false, "a.b": false} {
		if ValidClientID(s) != ok {
			t.Errorf("%q: %v", s, !ok)
		}
	}
}
