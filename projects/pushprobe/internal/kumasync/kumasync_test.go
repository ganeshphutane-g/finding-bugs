package kumasync

import (
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
	"github.com/ganeshphutane-g/pushprobe/internal/kuma"
)

func load(t *testing.T, file string, vars map[string]string) *config.Config {
	t.Helper()
	c, err := config.Load([]byte(file), func(k string) (string, bool) { v, ok := vars[k]; return v, ok }, config.ForSync)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const file = `
defaults: {interval: 60s, timeout: 10s, retries: 1}
targets: {kuma: {url: "https://kuma.example.com", group: Office}}
checks:
  - {name: nas, tcp: {address: "x:1"}, kuma: {push_token: "${KUMA_NAS}"}}
  - {name: printer, tcp: {address: "x:2"}, kuma: {push_token: "${KUMA_PRINTER}"}}
  - {name: router, tcp: {address: "x:3"}, kuma: {push_token: fixedtoken1}}
  - {name: web, tcp: {address: "x:4"}, kuma: {push_token: "${KUMA_WEB}"}}
`

func TestHeartbeatInterval(t *testing.T) {
	one, zero := 1, 0
	if n := HeartbeatInterval(config.Check{Interval: config.Duration(60 * time.Second), Timeout: config.Duration(10 * time.Second), Retries: &one}, 2*time.Second); n != 60+20+2+10 {
		t.Errorf("%d", n)
	}
	if n := HeartbeatInterval(config.Check{Interval: config.Duration(5 * time.Second), Timeout: config.Duration(time.Second), Retries: &zero}, 2*time.Second); n != 20 {
		t.Errorf("minimum: %d", n)
	}
}

func TestMakePlan(t *testing.T) {
	c := load(t, file, map[string]string{"KUMA_WEB": "webtoken"})
	monitors := []kuma.Object{
		{"id": 1.0, "type": "group", "name": "Office"},
		// printer is there already, by name in the group, with its own token.
		{"id": 2.0, "type": "push", "name": "printer", "parent": 1.0, "pushToken": "printertoken", "interval": 92.0},
		// web is found by its token, wherever it is, under its own name.
		{"id": 3.0, "type": "push", "name": "Web (site A)", "pushToken": "webtoken", "interval": 60.0},
		// Not push monitors, or another group: not matched.
		{"id": 4.0, "type": "http", "name": "nas", "parent": 1.0},
		{"id": 5.0, "type": "push", "name": "nas", "parent": 9.0, "pushToken": "other"},
	}
	n := 0
	p, err := MakePlan(c, monitors, 2*time.Second, func() string { n++; return "new" + string(rune('0'+n)) })
	if err != nil {
		t.Fatal(err)
	}
	if p.GroupID != 1 || len(p.Actions) != 4 {
		t.Fatalf("%+v", p)
	}
	nas, printer, router, web := p.Actions[0], p.Actions[1], p.Actions[2], p.Actions[3]
	if nas.Op != "create" || !nas.NewToken || nas.Token != "new1" || nas.Var != "KUMA_NAS" || nas.Interval != 92 {
		t.Errorf("nas %+v", nas)
	}
	if printer.Op != "ok" || printer.Token != "printertoken" || printer.ID != 2 || printer.NewToken {
		t.Errorf("printer %+v", printer)
	}
	if router.Op != "create" || router.NewToken || router.Token != "fixedtoken1" {
		t.Errorf("router %+v", router)
	}
	if web.Op != "update" || web.ID != 3 || web.Name != "Web (site A)" || strings.Join(web.Changes, ";") != "heartbeat interval 60s -> 92s" {
		t.Errorf("web %+v", web)
	}
	saves := func(a Action) bool { return a.Var == "KUMA_NAS" || a.Var == "KUMA_PRINTER" }
	lines := strings.Join(Describe(p, saves), "\n")
	for _, want := range []string{`+ push monitor "nas" (heartbeat every 92s, a new token, token saved as KUMA_NAS)`,
		`= push monitor "printer", token saved as KUMA_PRINTER`, `+ push monitor "router" (heartbeat every 92s, the file's token)`,
		`~ push monitor "Web (site A)": heartbeat interval 60s -> 92s`} {
		if !strings.Contains(lines, want) {
			t.Errorf("plan lacks %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "KUMA_WEB") {
		t.Errorf("a token not saved this time is said to be:\n%s", lines)
	}
	if strings.Contains(lines, "new1") || strings.Contains(lines, "printertoken") {
		t.Errorf("a token is shown:\n%s", lines)
	}
}

func TestMakePlanNewGroupAndClashes(t *testing.T) {
	c := load(t, file, nil)
	p, err := MakePlan(c, nil, 2*time.Second, NewToken)
	if err != nil || p.GroupID != 0 || !strings.HasPrefix(Describe(p, func(Action) bool { return false })[0], `+ group "Office"`) {
		t.Fatalf("%v %+v", err, p)
	}
	// A monitor in the group with the name but another token gets the file's.
	c = load(t, file, map[string]string{"KUMA_NAS": "mine"})
	p, _ = MakePlan(c, []kuma.Object{{"id": 1.0, "type": "group", "name": "Office"},
		{"id": 2.0, "type": "push", "name": "nas", "parent": 1.0, "pushToken": "theirs", "interval": 92.0}}, 2*time.Second, NewToken)
	if a := p.Actions[0]; a.Op != "update" || a.Token != "mine" || !strings.Contains(a.Changes[0], "push token") {
		t.Errorf("%+v", a)
	}
	// Two checks with one token.
	c = load(t, "targets: {kuma: {url: 'https://k'}}\nchecks:\n  - {name: a, tcp: {address: 'x:1'}, kuma: {push_token: same1}}\n  - {name: b, tcp: {address: 'x:2'}, kuma: {push_token: same1}}\n", nil)
	if _, err := MakePlan(c, nil, 2*time.Second, NewToken); err == nil || !strings.Contains(err.Error(), "same push token") {
		t.Errorf("%v", err)
	}
	if tok := NewToken(); len(tok) != 26 || NewToken() == tok {
		t.Errorf("token %q", tok)
	}
}
