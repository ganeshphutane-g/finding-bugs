package config

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/ganeshphutane-g/kumafile/internal/kuma"
)

func render(t *testing.T, ex *Export) string {
	t.Helper()
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(ex.Doc); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func state() *kuma.State {
	return &kuma.State{
		Info: kuma.Info{Version: "2.5.6"},
		Tags: []kuma.Object{{"id": num(1), "name": "prod", "color": "#059669"}},
		Notifications: []kuma.Object{{"id": num(1), "name": "Chat", "isDefault": true, "active": true,
			"config": map[string]any{"type": "discord", "discordWebhookUrl": "https://discord/hook/abc", "discordUsername": "kuma", "isDefault": true}}},
		Monitors: []kuma.Object{
			{"id": num(2), "name": "API", "parent": num(1), "type": "http", "url": "https://user:pw@api.example.org", "active": false,
				"notificationIDList": map[string]any{"1": true}, "keyword": "cost ${PRICE}",
				"tags": []any{map[string]any{"name": "prod", "value": ""}}, "headers": `{"Authorization":"Bearer x"}`,
				"basic_auth_pass": "pw2"},
			{"id": num(1), "name": "Web", "type": "group", "parent": nil},
		},
		Maintenance: []kuma.Maintenance{{Object: kuma.Object{"id": num(1), "title": "Night", "strategy": "manual", "dateRange": []any{nil}}, Monitors: []int64{2}}},
	}
}

func TestExportSecretsAndReferences(t *testing.T) {
	ex, err := Make(state(), ExportOptions{Source: "https://kuma.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	out := render(t, ex)
	for _, want := range []string{
		"name: API\n    group: Web\n    type: http\n    active: false",
		"url: ${KUMA_MONITOR_API_URL}",
		"keyword: cost $${PRICE}",
		"headers: ${KUMA_MONITOR_API_HEADERS}",
		"basic_auth_pass: ${KUMA_MONITOR_API_BASIC_AUTH_PASS}",
		"notifications: [Chat]",
		"tags: [prod]",
		"discordWebhookUrl: ${KUMA_NOTIFICATION_CHAT_DISCORDWEBHOOKURL}",
		"discordUsername: kuma",
		"default: true",
		"monitors: [Web / API]",
		"Uptime Kuma 2.5.6",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "pw2") || strings.Contains(out, "abc") || strings.Contains(out, "dateRange") {
		t.Errorf("a secret or an empty date range is in the file:\n%s", out)
	}
	if strings.Index(out, "name: Web") > strings.Index(out, "name: API") {
		t.Error("the group should come before what it contains")
	}
	names := map[string]string{}
	for _, s := range ex.Secrets {
		names[s.Name] = s.Value
	}
	if names["KUMA_MONITOR_API_URL"] != "https://user:pw@api.example.org" || len(ex.Secrets) != 4 {
		t.Fatalf("secrets %+v", ex.Secrets)
	}
	inline, _ := Make(state(), ExportOptions{InlineSecrets: true})
	if out := render(t, inline); !strings.Contains(out, "pw2") || len(inline.Secrets) != 0 {
		t.Fatalf("inline secrets:\n%s", out)
	}
}

func TestExportRefusesAmbiguousNames(t *testing.T) {
	st := state()
	st.Monitors = append(st.Monitors, kuma.Object{"id": num(3), "name": "api", "parent": num(1), "type": "ping"})
	if _, err := Make(st, ExportOptions{}); err == nil || !strings.Contains(err.Error(), "are both") {
		t.Fatalf("duplicate monitors: %v", err)
	}
	st = state()
	st.Monitors[1]["name"] = "A / B"
	if _, err := Make(st, ExportOptions{}); err == nil || !strings.Contains(err.Error(), "separates groups") {
		t.Fatalf("separator in a name: %v", err)
	}
}

func TestEnvFileRoundTrip(t *testing.T) {
	in := []Secret{{"A", "plain"}, {"B", "it's \"quoted\" $HOME `x` \\"}, {"C", "-----BEGIN KEY-----\nabc\n-----END KEY-----"}, {"D", ""}}
	var buf bytes.Buffer
	if err := WriteEnv(&buf, in); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEnv(&buf)
	if err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	for _, s := range in {
		if got[s.Name] != s.Value {
			t.Errorf("%s = %q, want %q", s.Name, got[s.Name], s.Value)
		}
	}
	if _, err := ReadEnv(strings.NewReader("A='open\n")); err == nil {
		t.Error("an unterminated quote was accepted")
	}
	if m, err := ReadEnv(strings.NewReader("# c\nexport X=1 # note\n\nY = 'two'\n")); err != nil || m["X"] != "1" || m["Y"] != "two" {
		t.Errorf("simple file: %v %v", m, err)
	}
}

func TestAwkwardTextReadsBack(t *testing.T) {
	texts := []string{"\t.header{}\n.footer{}", "\nleading newline", "trailing newline\n", "  indented\nlines",
		"tab\tinside", "cr\r\nlf", "multi\nline\n\n", "${not a ref}", "x: y # z", "- dash", "'quoted\"", "ünï code", "\x00nul"}
	st := &kuma.State{}
	for i, s := range texts {
		st.Tags = append(st.Tags, kuma.Object{"id": num(int64(i + 1)), "name": "t" + string(rune('a'+i)), "color": s})
	}
	ex, err := Make(st, ExportOptions{InlineSecrets: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := Load([]byte(render(t, ex)), func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("%v\n%s", err, render(t, ex))
	}
	for i, s := range texts {
		if got := d.Items["tags"][i].Fields["color"]; got != s {
			t.Errorf("%q came back as %q", s, got)
		}
	}
}

func TestSecretsAreFoundByNameAndShape(t *testing.T) {
	for _, c := range []struct {
		key, value string
		want       bool
	}{
		{"pagerdutyIntegrationKey", "PDKEY", true}, {"pushoveruserkey", "u", true}, {"bearsmsHashKey", "h", true},
		{"appriseURL", "discord://id/TOKEN", true}, {"callMeBotEndpoint", "https://api.callmebot.com/x.php?apikey=1", true},
		{"GrafanaOncallURL", "https://oncall/integrations/v1/abc/", true}, {"twilioAccountSID", "AC1", true},
		{"webhookContentType", "json", false}, {"telegramChatID", "42", false}, {"ntfyserverurl", "https://ntfy.sh", false},
		{"telegramUseTemplate", "true", false}, {"smtpPort", "587", false},
	} {
		if got := secretSetting(c.key, c.value); got != c.want {
			t.Errorf("secretSetting(%s, %s) = %v", c.key, c.value, got)
		}
	}
	for _, c := range []struct {
		field, value string
		want         bool
	}{
		{"url", "https://x/health?api_key=1", true}, {"url", "https://x/health?q=1", false}, {"url", "https://u:p@x", true},
		{"body", `{"password":"x"}`, true}, {"body", `{"a":1}`, false}, {"grpcMetadata", `{"authorization":"Bearer x"}`, true},
		{"headers", `{"X-Auth-Key":"k"}`, true}, {"hostname", "example.org", false},
	} {
		if got := secretField("monitors", c.field, c.value); got != c.want {
			t.Errorf("secretField(%s, %s) = %v", c.field, c.value, got)
		}
	}
}

func TestDiffHidesSecretsFromBothSides(t *testing.T) {
	have := load(t, "notifications:\n  - {name: P, type: PagerDuty, settings: {pagerdutyIntegrationKey: OLD-KEY}}\nmonitors:\n  - {name: M, type: http, url: 'https://x?api_key=OLD', body: '{\"password\":\"OLD\"}'}", nil)
	want := load(t, "notifications:\n  - {name: P, type: PagerDuty, settings: {pagerdutyIntegrationKey: NEW-KEY}}\nmonitors:\n  - {name: M, type: http, url: 'https://x?api_key=NEW', body: '{\"password\":\"NEW\"}'}", nil)
	var b strings.Builder
	Diff(want, have, false).Render(&b)
	if strings.Contains(b.String(), "OLD") || strings.Contains(b.String(), "NEW") || strings.Count(b.String(), "(secret, changed)") != 3 {
		t.Fatalf("diff shows secrets:\n%s", b.String())
	}
}

func TestUnknownTypes(t *testing.T) {
	have := load(t, "version: 1", nil)
	have.MonitorTypes = map[string]bool{"dns": true, "group": true}
	p := Diff(load(t, "monitors:\n  - {name: A, type: ntp, hostname: x}\n  - {name: B, type: http, url: 'https://x'}", nil), have, false)
	if len(p.Problems) != 1 || !strings.Contains(p.Problems[0], `no monitor type "ntp"`) {
		t.Fatalf("problems %q", p.Problems)
	}
}
