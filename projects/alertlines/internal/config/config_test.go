package config

import (
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func TestLoad(t *testing.T) {
	c, err := Load([]byte(`
alertmanagers: ["http://am:9093"]
loki: {url: "http://loki:3100", username: probe, password: "${ALERTLINES_LOKI_PASSWORD}"}
lines: {max: 20, labels: [pod]}
timeout: 5
`), env(map[string]string{"ALERTLINES_LOKI_PASSWORD": "pw"}), true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Loki.Password != "pw" || c.Lines.Max != 20 || c.Lines.MaxLineBytes != 300 || time.Duration(c.Timeout) != 5*time.Second ||
		c.Listen != DefaultListen || c.Annotation != "log_lines" || !*c.Redact.Builtin || time.Duration(c.Cache) != time.Minute {
		t.Fatalf("%+v", c)
	}
}

func TestProblems(t *testing.T) {
	for file, want := range map[string]string{
		"loki: {url: http://l}\n":                                                          "alertmanagers: name at least one",
		"alertmanagers: [ftp://x]\nloki: {url: http://l}\n":                                `alertmanagers: "ftp://x" is not an http`,
		"alertmanagers: [http://a]\n":                                                      "loki.url: missing",
		"alertmanagers: [http://a]\nloki: {url: http://l, usrname: x}\n":                   "field usrname not found",
		"alertmanagers: [http://a]\nloki: {url: 'http://${HOME}'}\n":                       "only ALERTLINES_* variables",
		"alertmanagers: [http://a]\nloki: {url: http://l}\ntimeout: 2m\n":                  "at most 8s",
		"alertmanagers: [http://a]\nloki: {url: http://l}\ntimeout: soon\n":                `"soon" is not a duration`,
		"alertmanagers: [http://a]\nloki: {url: http://l}\nannotation: 'log lines'\n":      "use a name such as log_lines",
		"alertmanagers: [http://a]\nloki: {url: http://l, username: u, bearer_token: t}\n": "not both",
		"alertmanagers: [http://a]\nloki: {url: 'http://u:secret@l x'}\n":                  "loki.url",
	} {
		_, err := Load([]byte(file), env(nil), true)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", file, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("a password in an error: %v", err)
		}
	}
	// check needs no Alertmanager.
	if _, err := Load([]byte("loki: {url: http://l}\n"), env(nil), false); err != nil {
		t.Errorf("check: %v", err)
	}
}
