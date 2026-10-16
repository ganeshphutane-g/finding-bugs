package cli

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/kumafile/internal/testkit"
)

// kf runs kumafile against a server and returns the exit code and output.
func kf(t *testing.T, s *testkit.Server, extra map[string]string, args ...string) (int, string) {
	t.Helper()
	e, out, errb := testEnv(t, s, extra)
	code := Run(context.Background(), e, args)
	return code, out.String() + errb.String()
}

// body is a file without its header (which names the instance and time).
func body(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(l, "#") {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}

func TestApplyCopiesAnInstance(t *testing.T) {
	a := testkit.Start(t)
	b := testkit.Start(t)
	seed(t, a)
	dir := t.TempDir()
	fa, fb := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")
	if code, out := kf(t, a, nil, "export", "-o", fa); code != 0 {
		t.Fatal(out)
	}
	code, out := kf(t, b, nil, "apply", "-y", fa)
	if code != 0 || !strings.Contains(out, "Done: the instance matches the file.") {
		t.Fatalf("apply (exit %d):\n%s\n%s", code, out, b.Logs())
	}
	if code, out := kf(t, b, nil, "export", "-o", fb, "--secrets", filepath.Join(dir, "b.env")); code != 0 {
		t.Fatal(out)
	}
	if ba, bb := body(t, fa), body(t, fb); ba != bb {
		t.Fatalf("the copy differs:\n--- a\n%s\n--- b\n%s", ba, bb)
	}
	if strings.Count(body(t, fb), "show_url: true") != 1 {
		t.Fatalf("status page groups were not copied:\n%s", body(t, fb))
	}
	envA, _ := os.ReadFile(filepath.Join(dir, "a.env"))
	envB, _ := os.ReadFile(filepath.Join(dir, "b.env"))
	if string(envA) != string(envB) {
		t.Fatalf("secrets differ:\n%s\n%s", envA, envB)
	}
	// A second apply has nothing to do.
	if code, out := kf(t, b, nil, "apply", "-y", fa); code != 0 || !strings.Contains(out, "The instance matches the file.") || strings.Contains(out, "+ ") {
		t.Fatalf("second apply (exit %d):\n%s", code, out)
	}
}

func TestApplyChangesAndPrune(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	dir := t.TempDir()
	file := filepath.Join(dir, "kuma.yaml")
	if code, out := kf(t, s, nil, "export", "-o", file); code != 0 {
		t.Fatal(out)
	}
	data, _ := os.ReadFile(file)
	text := string(data)
	for old, nw := range map[string]string{
		"interval: 120": "interval: 300",
		"    tags:\n      - name: prod\n        value: eu\n": "    tags:\n      - name: prod\n        value: us\n      - name: team\n        value: web\n",
		"  - name: prod\n    color: '#059669'\n":             "  - name: prod\n    color: '#059669'\n  - name: team\n    color: '#2563eb'\n",
		"description: All our services":                      "description: Everything",
		"telegramChatID: \"42\"":                             "telegramChatID: \"43\"",
		"monitors: [Web / Homepage]":                         "monitors: [Web / Homepage, Web / API]",
	} {
		if !strings.Contains(text, old) {
			t.Fatalf("the export has no %q:\n%s", old, text)
		}
		text = strings.Replace(text, old, nw, 1)
	}
	text = strings.Replace(text, "  - name: Router\n", `  - name: API
    group: Web
    type: keyword
    url: https://api.example.org/health
    keyword: ok
    active: false
  - name: Router
`, 1)
	os.WriteFile(file, []byte(text), 0o644)
	code, out := kf(t, s, nil, "apply", "-y", file)
	if code != 0 || !strings.Contains(out, "+ monitor Web / API") || !strings.Contains(out, "~ monitor Web / Homepage") {
		t.Fatalf("apply (exit %d):\n%s\n%s", code, out, s.Logs())
	}
	if code, out := kf(t, s, nil, "diff", file); code != 0 {
		t.Fatalf("after apply (exit %d):\n%s", code, out)
	}

	// Removing things from the file keeps them, unless pruned.
	start := strings.Index(text, "  - name: Router\n")
	end := start + 1 + strings.Index(text[start+1:], "  - name: ")
	text = text[:start] + text[end:]
	if strings.Contains(text, "Router") {
		t.Fatalf("could not remove Router:\n%s", text)
	}
	os.WriteFile(file, []byte(text), 0o644)
	if code, out := kf(t, s, nil, "apply", "-y", file); code != 0 || !strings.Contains(out, "kept (--prune deletes them): monitor Router") {
		t.Fatalf("apply without prune (exit %d):\n%s", code, out)
	}
	if code, out := kf(t, s, nil, "apply", "-y", "--prune", file); code != 0 || !strings.Contains(out, "- monitor Router") {
		t.Fatalf("apply with prune (exit %d):\n%s", code, out)
	}
	if code, out := kf(t, s, nil, "diff", "--prune", file); code != 0 {
		t.Fatalf("after prune (exit %d):\n%s", code, out)
	}
}

// TestEveryMonitorType creates a monitor of each common type from a file
// and checks the instance then matches it.
func TestEveryMonitorType(t *testing.T) {
	s := testkit.Start(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "types.yaml")
	text := `version: 1
docker_hosts:
  - {name: local, dockerType: socket, dockerDaemon: /var/run/docker.sock}
monitors:
  - {name: Group, type: group}
  - {name: HTTP, group: Group, type: http, url: "https://example.org", method: POST, body: '{"a":1}', headers: '{"X-A":"1"}'}
  - {name: Keyword, type: keyword, url: "https://example.org", keyword: Example, invertKeyword: true}
  - {name: JSON, type: json-query, url: "https://example.org/x.json", jsonPath: "$.status", expectedValue: ok}
  - {name: Port, type: port, hostname: example.org, port: 443}
  - {name: Ping, type: ping, hostname: 127.0.0.1}
  - {name: DNS, type: dns, hostname: example.org, dns_resolve_type: AAAA}
  - {name: Docker, type: docker, docker_container: web, docker_host: local}
  - {name: Push, type: push, pushToken: "${PUSH_TOKEN}"}
  - {name: Game, type: gamedig, game: minecraft, hostname: mc.example.org, port: 25565}
  - {name: MQTT, type: mqtt, hostname: mqtt.example.org, port: 1883, mqttTopic: a/b, mqttUsername: u, mqttPassword: "${MQTT_PASS}"}
  - {name: Postgres, type: postgres, databaseConnectionString: "${PG_URL}"}
  - {name: Redis, type: redis, databaseConnectionString: "redis://localhost:6379"}
  - {name: Radius, type: radius, hostname: radius.lan, radiusUsername: u, radiusPassword: p, radiusSecret: s}
  - {name: Websocket, type: websocket-upgrade, url: "wss://example.org/ws"}
  - {name: gRPC, type: grpc-keyword, grpcUrl: "grpc.example.org:443", keyword: SERVING, grpcServiceName: health}
`
	os.WriteFile(file, []byte(text), 0o644)
	vars := map[string]string{"PUSH_TOKEN": "abcdefghijklmnopqrstuvwxyz012345", "MQTT_PASS": "m", "PG_URL": "postgres://u:p@db:5432/x"}
	code, out := kf(t, s, vars, "apply", "-y", file)
	if code != 0 {
		t.Fatalf("apply (exit %d):\n%s\n%s", code, out, s.Logs())
	}
	if code, out := kf(t, s, vars, "apply", "-y", file); code != 0 || strings.Contains(out, "~ ") {
		t.Fatalf("second apply (exit %d):\n%s", code, out)
	}
	exported := filepath.Join(dir, "out.yaml")
	if code, out := kf(t, s, nil, "export", "-o", exported); code != 0 {
		t.Fatal(out)
	}
	ex := body(t, exported)
	for _, want := range []string{"pushToken: ${MONITOR_PUSH_PUSHTOKEN}", "databaseConnectionString: ${MONITOR_POSTGRES_DATABASECONNECTIONSTRING}",
		"mqttPassword: ${MONITOR_MQTT_MQTTPASSWORD}", "docker_host: local", "group: Group", "dns_resolve_type: AAAA"} {
		if !strings.Contains(ex, want) {
			t.Errorf("export lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(ex)
	}
}

func TestTwoFactorLogin(t *testing.T) {
	s := testkit.Start(t)
	c := s.Client()
	r := s.Must(c, "prepare2FA", testkit.AdminPassword)
	u, err := url.Parse(r["uri"].(string))
	if err != nil {
		t.Fatal(err)
	}
	secret := u.Query().Get("secret")
	s.Must(c, "save2FA", testkit.AdminPassword)
	dir := t.TempDir()
	if code, out := kf(t, s, nil, "export", "-o", filepath.Join(dir, "x.yaml")); code != 1 || !strings.Contains(out, "two-factor") {
		t.Fatalf("without a code (exit %d): %s", code, out)
	}
	file := filepath.Join(dir, "k.yaml")
	os.WriteFile(file, []byte("tags:\n  - {name: a, color: '#000000'}\n"), 0o644)
	// apply logs in twice (apply, then check): the one-time code works only
	// once, so the second login uses the token from the first.
	if code, out := kf(t, s, map[string]string{"KUMAFILE_TOTP_SECRET": secret}, "apply", "-y", file); code != 0 || !strings.Contains(out, "Done") {
		t.Fatalf("with the TOTP secret (exit %d): %s", code, out)
	}
}
