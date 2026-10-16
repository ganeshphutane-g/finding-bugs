package cli

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ganeshphutane-g/kumafile/internal/testkit"
)

// ui is a monitor as the web interface sends it, with changes.
func ui(changes map[string]any) map[string]any {
	m := map[string]any{"type": "http", "parent": nil, "url": "https://", "method": "GET", "location": "world",
		"interval": 60, "retryInterval": 60, "resendInterval": 0, "maxretries": 0, "notificationIDList": map[string]any{},
		"expiryNotification": false, "domainExpiryNotification": true, "maxredirects": 10, "accepted_statuscodes": []string{"200-299"},
		"saveErrorResponse": true, "responseMaxLength": 1024, "dns_resolve_type": "A", "dns_resolve_server": "",
		"mqttCheckType": "keyword", "oauth_auth_method": "client_secret_basic", "httpBodyEncoding": "json",
		"kafkaProducerBrokers": []any{}, "kafkaProducerSaslOptions": map[string]any{"mechanism": "None"},
		"gamedigGivenPortOnly": true, "rabbitmqNodes": []any{}, "conditions": []any{}, "sshAuthMethod": "password",
		"ntpStratumThreshold": 5, "ntpTimeOffsetThreshold": 1000, "ntpRootDispersionThreshold": 500, "timeout": 48,
		"snmpVersion": "2c", "jsonPath": "$", "jsonPathOperator": "=="}
	for k, v := range changes {
		m[k] = v
	}
	return m
}

// onePixelPNG is a valid 1x1 PNG.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

// TestReviewedCases covers what the reviews found, on an instance copied to
// a fresh one: every case must copy exactly and then stay in sync.
func TestReviewedCases(t *testing.T) {
	a := testkit.Start(t)
	b := testkit.Start(t)
	c := a.Client()
	prod := a.Must(c, "addTag", map[string]any{"name": "prod", "color": "#059669"})["tag"].(map[string]any)["id"]
	env := a.Must(c, "addTag", map[string]any{"name": "env", "color": "#2563eb"})["tag"].(map[string]any)["id"]
	// Kafka with SASL (a structured secret).
	a.Must(c, "add", ui(map[string]any{"type": "kafka-producer", "name": "Kafka", "kafkaProducerBrokers": []any{"k:9092"},
		"kafkaProducerTopic": "t", "kafkaProducerMessage": "m", "timeout": 1,
		"kafkaProducerSaslOptions": map[string]any{"mechanism": "plain", "username": "u", "password": "kafka-pass"}}))
	// A paused group with a child that is itself enabled.
	g := a.Must(c, "add", ui(map[string]any{"type": "group", "name": "Paused group"}))
	a.Must(c, "add", ui(map[string]any{"name": "Kid", "url": "https://kid.example", "parent": g["monitorID"]}))
	a.Must(c, "pauseMonitor", g["monitorID"])
	// Accepting any status code (an empty list), and tags in an order,
	// one with a null value (as API clients add them).
	m := a.Must(c, "add", ui(map[string]any{"name": "Any code", "url": "https://any.example", "accepted_statuscodes": []string{}}))
	a.Must(c, "addMonitorTag", env, m["monitorID"], "b")
	a.Must(c, "addMonitorTag", prod, m["monitorID"], nil)
	a.Must(c, "addMonitorTag", env, m["monitorID"], "a")
	// A status page with a custom link and an uploaded logo.
	a.Must(c, "addStatusPage", "Public", "public")
	sp := a.Must(c, "getStatusPage", "public")["config"].(map[string]any)
	a.Must(c, "saveStatusPage", "public", sp, "data:image/png;base64,"+onePixelPNG, []any{map[string]any{"name": "Web",
		"monitorList": []any{map[string]any{"id": m["monitorID"], "sendUrl": 1, "url": "https://custom-link.example"}}}})
	// A weekly maintenance window, whose cron the server works out.
	a.Must(c, "addMaintenance", map[string]any{"title": "Weekly", "description": "", "strategy": "recurring-weekday", "active": true,
		"intervalDay": 1, "dateRange": []any{}, "timeRange": []any{map[string]any{"hours": 2, "minutes": 0}, map[string]any{"hours": 3, "minutes": 30}},
		"weekdays": []any{1, 3}, "daysOfMonth": []any{}, "cron": "", "durationMinutes": 60, "timezoneOption": "Europe/Berlin"})

	dir := t.TempDir()
	fa, fb := filepath.Join(dir, "a.yaml"), filepath.Join(dir, "b.yaml")
	if code, out := kf(t, a, nil, "export", "-o", fa); code != 0 {
		t.Fatal(out)
	}
	text := body(t, fa)
	for _, want := range []string{"kafkaProducerSaslOptions: ${KUMA_MONITOR_KAFKA_KAFKAPRODUCERSASLOPTIONS}",
		"url: https://custom-link.example", "logo: data:image/png;base64,", "accepted_statuscodes: []"} {
		if !strings.Contains(text, want) {
			t.Errorf("the export lacks %q", want)
		}
	}
	if strings.Contains(text, "kafka-pass") || strings.Contains(text, "cron:") {
		t.Errorf("a secret or a derived cron is in the file")
	}
	if code, out := kf(t, a, nil, "diff", fa); code != 0 {
		t.Fatalf("the instance differs from its own export (exit %d):\n%s", code, out)
	}
	if code, out := kf(t, b, nil, "apply", "-y", fa); code != 0 {
		t.Fatalf("copy (exit %d):\n%s\n%s", code, out, b.Logs())
	}
	if code, out := kf(t, b, nil, "export", "-o", fb, "--secrets", filepath.Join(dir, "b.env")); code != 0 {
		t.Fatal(out)
	}
	if ta, tb := body(t, fa), body(t, fb); ta != tb {
		t.Fatalf("the copy differs:\n--- a\n%s\n--- b\n%s", ta, tb)
	}
	logo, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	data, _, err := b.Client().Fetch(t.Context(), "/upload/logo1.png", 1<<20)
	if err != nil || string(data) != string(logo) {
		t.Fatalf("the logo was not copied: %v", err)
	}
	// An edit of the weekly window converges.
	edited := strings.Replace(string(mustRead(t, fa)), "hours: 2", "hours: 4", 1)
	os.WriteFile(fa, []byte(edited), 0o644)
	if code, out := kf(t, b, nil, "apply", "-y", fa); code != 0 || !strings.Contains(out, "~ maintenance window Weekly") {
		t.Fatalf("edit (exit %d):\n%s", code, out)
	}
	// Removing the null-valued tag works.
	edited = strings.Replace(edited, "      - prod\n", "", 1)
	os.WriteFile(fa, []byte(edited), 0o644)
	if code, out := kf(t, b, nil, "apply", "-y", fa); code != 0 {
		t.Fatalf("removing a tag (exit %d):\n%s\n%s", code, out, edited)
	}

	// A probe left behind by an interrupted apply is removed, and never
	// exported.
	cb := b.Client()
	b.Must(cb, "add", ui(map[string]any{"name": "kumafile probe (temporary)", "url": "https://example.com", "active": false}))
	if code, out := kf(t, b, nil, "export", "-o", fb, "--secrets", filepath.Join(dir, "b.env")); code != 0 || strings.Contains(body(t, fb), "probe") {
		t.Fatalf("export with a leftover probe (exit %d): %s", code, out)
	}
	os.WriteFile(fa, []byte(strings.Replace(edited, "title: Weekly", "title: Weekly\n    description: changed", 1)), 0o644)
	if code, out := kf(t, b, nil, "apply", "-y", fa); code != 0 {
		t.Fatalf("apply with a leftover probe (exit %d): %s", code, out)
	}
	st, err := b.Client().Read(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range st.Monitors {
		if m.Str("name") == "kumafile probe (temporary)" {
			t.Fatal("the leftover probe was not removed")
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
