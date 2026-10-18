package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

const good = `
defaults: {interval: 30s, retries: 2}
targets:
  kuma: {url: https://kuma.example.com}
  gatus: {url: https://status.example.com, token: "${GATUS_TOKEN}"}
checks:
  - name: nas web
    http: {url: "http://192.168.1.10:5000", expect_status: [200, 301], contains: Synology, headers: {Authorization: "Bearer ${PUSHPROBE_NAS}"}}
    kuma: {push_token: "${KUMA_NAS_TOKEN}"}
  - name: printer
    interval: 90
    tcp: {address: "192.168.1.20:9100"}
    gatus: {endpoint: office_printer}
  - name: dns
    dns: {server: 192.168.1.1, query: nas.lan, expect: [192.168.1.10]}
    kuma: {push_token: abc123}
  - name: cert
    tls: {address: "vpn.example.com:443"}
    kuma: {push_token: abc124}
`

func TestLoad(t *testing.T) {
	c, err := Load([]byte(good), env(map[string]string{"GATUS_TOKEN": "g", "KUMA_NAS_TOKEN": "tok1", "PUSHPROBE_NAS": "secret"}), ForRun)
	if err != nil {
		t.Fatal(err)
	}
	nas, printer, dns, cert := c.Checks[0], c.Checks[1], c.Checks[2], c.Checks[3]
	if nas.Kind() != "http" || nas.HTTP.Method != "GET" || nas.HTTP.Headers["Authorization"] != "Bearer secret" || nas.Kuma.PushToken != "tok1" {
		t.Errorf("nas: %+v %+v", nas, nas.HTTP)
	}
	if time.Duration(nas.Interval) != 30*time.Second || time.Duration(nas.Timeout) != 10*time.Second || *nas.Retries != 2 {
		t.Errorf("defaults: %s %s %d", nas.Interval, nas.Timeout, *nas.Retries)
	}
	if time.Duration(printer.Interval) != 90*time.Second || printer.Line != 10 {
		t.Errorf("printer: %s line %d", printer.Interval, printer.Line)
	}
	if dns.DNS.Type != "A" || *cert.TLS.MinDaysLeft != DefaultMinDays || c.Targets.Gatus.Token != "g" {
		t.Errorf("dns %s, cert %d, gatus %q", dns.DNS.Type, *cert.TLS.MinDaysLeft, c.Targets.Gatus.Token)
	}
}

func TestLoadForCheckIgnoresWhereResultsGo(t *testing.T) {
	// No tokens set: fine for a dry run, not for run.
	vars := env(map[string]string{"PUSHPROBE_NAS": "secret"})
	if _, err := Load([]byte(good), vars, ForCheck); err != nil {
		t.Fatal(err)
	}
	_, err := Load([]byte(good), vars, ForRun)
	if err == nil || !strings.Contains(err.Error(), "no value for GATUS_TOKEN, KUMA_NAS_TOKEN") {
		t.Fatalf("%v", err)
	}
	// A check's own variables are still needed.
	if _, err := Load([]byte(good), env(nil), ForCheck); err == nil || !strings.Contains(err.Error(), "PUSHPROBE_NAS") {
		t.Fatalf("%v", err)
	}
}

func TestProblems(t *testing.T) {
	for name, c := range map[string]struct{ file, want string }{
		"unknown key":      {"checks:\n  - name: a\n    tcp: {adress: x:1}\n", "line 3: field adress not found"},
		"bad duration":     {"checks:\n  - name: a\n    interval: soon\n    tcp: {address: x:1}\n", `line 3: "soon" is not a duration`},
		"no kind":          {"checks:\n  - name: a\n    kuma: {push_token: x}\n", `line 2: check "a": say what to check`},
		"two kinds":        {"checks:\n  - name: a\n    tcp: {address: x:1}\n    disk: {path: /, max_used_percent: 90}\n", "more than one"},
		"twice":            {"checks:\n  - {name: a, tcp: {address: x:1}}\n  - {name: a, tcp: {address: x:2}}\n", "used twice (also on line 2)"},
		"no name":          {"checks:\n  - tcp: {address: x:1}\n", "line 2: a check has no name"},
		"bad address":      {"checks:\n  - {name: a, tcp: {address: nohost}}\n", `tcp.address: "nohost": expected host:port`},
		"bad url":          {"checks:\n  - {name: a, http: {url: 'ftp://x'}}\n", "not an http:// or https:// address"},
		"timeout":          {"checks:\n  - {name: a, interval: 10s, timeout: 10s, tcp: {address: x:1}}\n", "must be shorter than the interval"},
		"short interval":   {"checks:\n  - {name: a, interval: 1s, timeout: 500ms, tcp: {address: x:1}}\n", "at least 5s"},
		"nowhere":          {"targets: {kuma: {url: 'http://k'}}\nchecks:\n  - {name: a, tcp: {address: x:1}}\n", "the result goes nowhere"},
		"no target":        {"checks:\n  - {name: a, tcp: {address: x:1}, kuma: {push_token: abc}}\n", "targets has no kuma"},
		"bad token":        {"targets: {kuma: {url: 'http://k'}}\nchecks:\n  - {name: a, tcp: {address: x:1}, kuma: {push_token: 'http://k/api/push/abc'}}\n", "push_token must be"},
		"no gatus token":   {"targets: {gatus: {url: 'http://g'}}\nchecks:\n  - {name: a, tcp: {address: x:1}, gatus: {endpoint: a_b}}\n", "gatus needs a token"},
		"other variable":   {"checks:\n  - {name: a, http: {url: 'http://x/${HOME}'}}\n", "HOME (only PUSHPROBE_*"},
		"empty":            {"", "the file is empty"},
		"no checks":        {"defaults: {interval: 30s}\n", "there are no checks"},
		"percent":          {"checks:\n  - {name: a, disk: {path: /, max_used_percent: 120}}\n", "between 0 and 100"},
		"dns type":         {"checks:\n  - {name: a, dns: {query: x, type: SRV}}\n", `dns.type "SRV"`},
		"negative retries": {"checks:\n  - {name: a, retries: -1, tcp: {address: x:1}}\n", "retries must not be negative"},
	} {
		_, err := Load([]byte(c.file), env(nil), ForRun)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDollarEscape(t *testing.T) {
	c, err := Load([]byte("checks:\n  - {name: a, http: {url: 'http://x/', contains: 'cost $${PRICE}'}}\n"), env(nil), ForCheck)
	if err != nil || c.Checks[0].HTTP.Contains != "cost ${PRICE}" {
		t.Fatalf("%v %v", err, c)
	}
}

func TestLoadForSync(t *testing.T) {
	file := `
targets:
  kuma: {url: "https://kuma.example.com", group: Office}
  gatus: {url: "https://g", token: "${GATUS_TOKEN}"}
checks:
  - {name: a, tcp: {address: "x:1"}, kuma: {push_token: "${KUMA_A}"}}
  - {name: b, tcp: {address: "x:2"}, kuma: {push_token: "${KUMA_B}"}}
  - {name: c, tcp: {address: "x:3"}, kuma: {push_token: literal123}}
  - {name: d, tcp: {address: "x:4"}, gatus: {endpoint: office_d}}
`
	c, err := Load([]byte(file), env(map[string]string{"KUMA_B": "known"}), ForSync)
	if err != nil {
		t.Fatal(err)
	}
	a, b, lit := c.Checks[0].Kuma, c.Checks[1].Kuma, c.Checks[2].Kuma
	if a.PushToken != "" || a.TokenVar() != "KUMA_A" || b.PushToken != "known" || b.TokenVar() != "KUMA_B" || lit.PushToken != "literal123" || lit.TokenVar() != "" {
		t.Fatalf("%+v %+v %+v", a, b, lit)
	}
	if c.Targets.Kuma.Group != "Office" {
		t.Fatalf("group %q", c.Targets.Kuma.Group)
	}
	// Only a whole ${NAME} may be left for sync; other variables are needed.
	_, err = Load([]byte("targets: {kuma: {url: 'https://k'}}\nchecks:\n  - {name: a, tcp: {address: 'x:1'}, kuma: {push_token: 'pre-${KUMA_A}'}}\n"), env(nil), ForSync)
	if err == nil || !strings.Contains(err.Error(), "no value for KUMA_A") {
		t.Fatalf("%v", err)
	}
	if _, err = Load([]byte("targets: {kuma: {url: 'https://k'}}\nchecks:\n  - {name: a, tcp: {address: 'x:1'}, kuma: {push_token: '${HOME}'}}\n"), env(nil), ForSync); err == nil || !strings.Contains(err.Error(), "HOME (only") {
		t.Fatalf("%v", err)
	}
	if _, err = Load([]byte("checks:\n  - {name: a, tcp: {address: 'x:1'}}\n"), env(nil), ForSync); err == nil || !strings.Contains(err.Error(), "nothing to set up") {
		t.Fatalf("%v", err)
	}
}

func TestEnvFile(t *testing.T) {
	vars, err := ParseEnv([]byte("# tokens\nKUMA_A=abc\nexport KUMA_B=\"x y\"\n\nGATUS_T='q'\n"))
	if err != nil || vars["KUMA_A"] != "abc" || vars["KUMA_B"] != "x y" || vars["GATUS_T"] != "q" {
		t.Fatalf("%v %v", vars, err)
	}
	if _, err := ParseEnv([]byte("KUMA_A=1\nnonsense\n")); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("%v", err)
	}
	f := t.TempDir() + "/pushprobe.env"
	os.WriteFile(f, []byte("# mine\nKUMA_A=old\nOTHER=1\n"), 0o644)
	if err := UpdateEnv(f, map[string]string{"KUMA_A": "new", "KUMA_C": "c"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(f)
	if string(data) != "# mine\nKUMA_A=new\nOTHER=1\nKUMA_C=c\n" {
		t.Fatalf("%q", data)
	}
	if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}
