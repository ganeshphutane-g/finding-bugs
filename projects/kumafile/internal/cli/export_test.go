package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ganeshphutane-g/kumafile/internal/config"
	"github.com/ganeshphutane-g/kumafile/internal/testkit"
)

func testEnv(t *testing.T, s *testkit.Server, extra map[string]string) (Env, *bytes.Buffer, *bytes.Buffer) {
	vars := map[string]string{"KUMAFILE_URL": s.URL, "KUMAFILE_USERNAME": testkit.AdminUser, "KUMAFILE_PASSWORD": testkit.AdminPassword}
	for k, v := range extra {
		vars[k] = v
	}
	var out, errb bytes.Buffer
	e := DefaultEnv()
	e.Stdin, e.Stdout, e.Stderr = strings.NewReader(""), &out, &errb
	e.Getenv = func(k string) string { return vars[k] }
	return e, &out, &errb
}

// seed gives an instance one of most things: a group with an HTTP monitor
// (basic auth, a tag with a value, a notification), a ping monitor, a
// proxy, a Docker host, a status page and a maintenance window.
func seed(t *testing.T, s *testkit.Server) {
	c := s.Client()
	n := s.Must(c, "addNotification", map[string]any{"name": "Ops Telegram", "type": "telegram",
		"telegramBotToken": "123:SECRET", "telegramChatID": "42", "isDefault": false, "applyExisting": false}, nil)
	tag := s.Must(c, "addTag", map[string]any{"name": "prod", "color": "#059669"})
	tagID := tag["tag"].(map[string]any)["id"]
	s.Must(c, "addProxy", map[string]any{"protocol": "http", "host": "proxy.lan", "port": 3128, "auth": true,
		"username": "u", "password": "proxy-pass", "default": false, "applyExisting": false}, nil)
	s.Must(c, "addDockerHost", map[string]any{"name": "local", "dockerType": "socket", "dockerDaemon": "/var/run/docker.sock"}, nil)
	g := s.Must(c, "add", map[string]any{"type": "group", "name": "Web", "accepted_statuscodes": []string{"200-299"},
		"notificationIDList": map[string]any{}, "conditions": []any{}, "interval": 60})
	m := s.Must(c, "add", map[string]any{"type": "http", "name": "Homepage", "url": "https://example.org", "parent": g["monitorID"],
		"interval": 120, "retryInterval": 60, "maxretries": 2, "accepted_statuscodes": []string{"200-299"}, "method": "GET",
		"notificationIDList": map[string]any{itoa(n["id"]): true}, "conditions": []any{}, "basic_auth_user": "bob", "basic_auth_pass": "http-pass"})
	s.Must(c, "addMonitorTag", tagID, m["monitorID"], "eu")
	s.Must(c, "add", map[string]any{"type": "ping", "name": "Router", "hostname": "192.168.1.1", "interval": 60,
		"accepted_statuscodes": []string{"200-299"}, "notificationIDList": map[string]any{}, "conditions": []any{}})
	s.Must(c, "addStatusPage", "Public status", "public")
	sp := s.Must(c, "getStatusPage", "public")["config"].(map[string]any)
	sp["description"] = "All our services"
	s.Must(c, "saveStatusPage", "public", sp, sp["icon"], []any{map[string]any{"name": "Website",
		"monitorList": []any{map[string]any{"id": m["monitorID"], "sendUrl": 1}}}})
	mt := s.Must(c, "addMaintenance", map[string]any{"title": "Patch night", "description": "", "strategy": "manual", "active": true,
		"intervalDay": 1, "dateRange": []any{}, "timeRange": []any{}, "weekdays": []any{}, "daysOfMonth": []any{},
		"cron": "", "durationMinutes": 60, "timezoneOption": "SAME_AS_SERVER"})
	s.Must(c, "addMonitorMaintenance", mt["maintenanceID"], []any{map[string]any{"id": m["monitorID"]}})
}

func itoa(v any) string {
	if n, ok := v.(float64); ok {
		return strconv.FormatInt(int64(n), 10)
	}
	return ""
}

func TestExport(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	dir := t.TempDir()
	out := filepath.Join(dir, "kuma.yaml")
	e, _, errb := testEnv(t, s, nil)
	if code := Run(context.Background(), e, []string{"export", "-o", out}); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errb, s.Logs())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"name: Homepage\n    group: Web\n    type: http\n    url: https://example.org",
		"interval: 120", "maxretries: 2",
		"basic_auth_user: bob", "basic_auth_pass: ${MONITOR_HOMEPAGE_BASIC_AUTH_PASS}",
		"notifications: [Ops Telegram]",
		"- name: prod\n        value: eu",
		"telegramBotToken: ${NOTIFICATION_OPS_TELEGRAM_TELEGRAMBOTTOKEN}",
		"password: ${PROXY_PROXY_LAN_PASSWORD}",
		"dockerDaemon: /var/run/docker.sock",
		"monitors: [Web / Homepage]",
		"description: All our services",
		"- name: Website\n        monitors:\n          - monitor: Web / Homepage\n            show_url: true",
		"name: Router\n    type: ping\n    hostname: 192.168.1.1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	for _, secret := range []string{"http-pass", "123:SECRET", "proxy-pass", testkit.AdminPassword} {
		if strings.Contains(text, secret) {
			t.Errorf("secret %q is in the file", secret)
		}
	}
	if t.Failed() {
		t.Log(text)
	}
	fi, err := os.Stat(filepath.Join(dir, "kuma.env"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file: %v %v", fi, err)
	}
	f, _ := os.Open(filepath.Join(dir, "kuma.env"))
	defer f.Close()
	env, err := config.ReadEnv(f)
	if err != nil || env["MONITOR_HOMEPAGE_BASIC_AUTH_PASS"] != "http-pass" || env["NOTIFICATION_OPS_TELEGRAM_TELEGRAMBOTTOKEN"] != "123:SECRET" {
		t.Fatalf("secrets %v %v", env, err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("the file is not valid YAML: %v", err)
	}

	// To standard output, with the secrets in it.
	e, stdout, errb := testEnv(t, s, nil)
	if code := Run(context.Background(), e, []string{"export", "-o", "-", "--with-secrets"}); code != 0 || !strings.Contains(stdout.String(), "http-pass") {
		t.Fatalf("exit %d: %s", code, errb)
	}
	// Refused: standard output without saying where secrets go.
	e, _, _ = testEnv(t, s, nil)
	if code := Run(context.Background(), e, []string{"export", "-o", "-"}); code != 2 {
		t.Fatalf("exit %d", code)
	}
	// A wrong password.
	e, _, errb = testEnv(t, s, map[string]string{"KUMAFILE_PASSWORD": "wrong"})
	if code := Run(context.Background(), e, []string{"export", "-o", filepath.Join(dir, "x.yaml")}); code != 1 || !strings.Contains(errb.String(), "incorrect username or password") {
		t.Fatalf("exit %d: %s", code, errb)
	}
}

func TestDiff(t *testing.T) {
	s := testkit.Start(t)
	seed(t, s)
	dir := t.TempDir()
	file := filepath.Join(dir, "kuma.yaml")
	e, _, errb := testEnv(t, s, nil)
	if code := Run(context.Background(), e, []string{"export", "-o", file}); code != 0 {
		t.Fatalf("export: %s", errb)
	}
	e, out, errb := testEnv(t, s, nil)
	if code := Run(context.Background(), e, []string{"diff", file}); code != 0 || !strings.Contains(out.String(), "The instance matches the file.") {
		t.Fatalf("a fresh export differs (exit %d):\n%s%s", code, out, errb)
	}
	data, _ := os.ReadFile(file)
	changed := strings.Replace(string(data), "interval: 120", "interval: 300", 1)
	changed = strings.Replace(changed, "description: All our services", "description: Everything", 1)
	os.WriteFile(file, []byte(changed), 0o644)
	e, out, _ = testEnv(t, s, map[string]string{"MONITOR_HOMEPAGE_BASIC_AUTH_PASS": "rotated"})
	code := Run(context.Background(), e, []string{"diff", file})
	for _, want := range []string{"interval: 120 -> 300", "basic_auth_pass: (secret, changed)", "description: All our services -> Everything", "0 to create, 2 to change, 0 to delete."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q", want)
		}
	}
	if code != 3 || strings.Contains(out.String(), "rotated") || strings.Contains(out.String(), "http-pass") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	// Without the env file, the secrets are missing.
	os.Remove(filepath.Join(dir, "kuma.env"))
	e, _, errb = testEnv(t, s, nil)
	if code := Run(context.Background(), e, []string{"diff", file}); code != 1 || !strings.Contains(errb.String(), "no value for") {
		t.Fatalf("exit %d: %s", code, errb)
	}
}
