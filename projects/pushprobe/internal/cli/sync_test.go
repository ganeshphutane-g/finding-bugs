package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
	"github.com/ganeshphutane-g/pushprobe/internal/kuma"
	"github.com/ganeshphutane-g/pushprobe/internal/runner"
	"github.com/ganeshphutane-g/pushprobe/internal/testkit"
)

func TestSyncWithARealServer(t *testing.T) {
	s := testkit.Start(t)
	c := s.Client()
	// A notification on a monitor that sync will change must stay.
	n := s.Must(c, "addNotification", map[string]any{"name": "ops", "type": "webhook", "webhookURL": "https://example.com/hook",
		"webhookContentType": "json", "isDefault": false, "applyExisting": false}, nil)
	noteID := kuma.Int(n["id"])
	g := s.Must(c, "add", map[string]any{"type": "group", "name": "Office", "accepted_statuscodes": []string{"200-299"},
		"notificationIDList": map[string]any{}, "conditions": []any{}, "interval": 60, "retryInterval": 60, "active": true})
	s.Must(c, "add", map[string]any{"type": "push", "name": "printer", "pushToken": "printerTokenFromKuma123", "parent": g["monitorID"],
		"interval": 60, "retryInterval": 60, "maxretries": 0, "accepted_statuscodes": []string{"200-299"},
		"notificationIDList": map[string]any{kumaID(noteID): true}, "conditions": []any{}, "active": true})

	dir := t.TempDir()
	file := filepath.Join(dir, "pushprobe.yaml")
	os.WriteFile(file, []byte(`
defaults: {interval: 30s, timeout: 5s, retries: 0}
targets: {kuma: {url: "`+s.URL+`", group: Office}}
checks:
  - {name: nas, tcp: {address: "127.0.0.1:1"}, kuma: {push_token: "${KUMA_NAS}"}}
  - {name: printer, tcp: {address: "127.0.0.1:1"}, kuma: {push_token: "${KUMA_PRINTER}"}}
`), 0o600)
	vars := map[string]string{"PUSHPROBE_KUMA_USERNAME": testkit.AdminUser, "PUSHPROBE_KUMA_PASSWORD": testkit.AdminPassword}
	sync := func(args ...string) (int, string) {
		var out syncBuffer
		env := Env{Stdout: &out, Stderr: &out, ReadFile: os.ReadFile, LookupEnv: func(k string) (string, bool) { v, ok := vars[k]; return v, ok }}
		code := Run(context.Background(), env, append([]string{"sync", "-c", file}, args...))
		return code, out.String()
	}
	code, out := sync("--dry-run")
	if code != 0 || !strings.Contains(out, `+ push monitor "nas" (heartbeat every 45s, a new token`) || !strings.Contains(out, `~ push monitor "printer": heartbeat interval 60s -> 45s`) ||
		!strings.Contains(out, "Nothing was changed") {
		t.Fatalf("dry run: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "pushprobe.env")); err == nil {
		t.Fatal("the dry run wrote the env file")
	}
	code, out = sync()
	if code != 0 || !strings.Contains(out, "Done: 2 monitors changed; 2 tokens saved in") {
		t.Fatalf("sync: exit %d\n%s", code, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "pushprobe.env"))
	saved, _ := config.ParseEnv(data)
	if saved["KUMA_PRINTER"] != "printerTokenFromKuma123" || len(saved["KUMA_NAS"]) != 26 || strings.Contains(out, saved["KUMA_NAS"]) {
		t.Fatalf("env file %q, output:\n%s", data, out)
	}
	// Again: nothing to do.
	if code, out := sync(); code != 0 || !strings.Contains(out, `= push monitor "nas"`) || !strings.Contains(out, "Done: 0 monitors changed.") {
		t.Fatalf("second sync: exit %d\n%s", code, out)
	}
	// The printer kept its notification, and got the new interval.
	c2 := s.Client()
	ids := map[string]int64{}
	st, _ := c2.Read(context.Background())
	for _, o := range st.Monitors {
		ids[o.Str("name")] = o.ID()
	}
	m, err := c2.Do(context.Background(), "getMonitor", ids["printer"])
	if err != nil {
		t.Fatal(err)
	}
	mon := kuma.Object(m["monitor"].(map[string]any))
	if mon.Str("name") != "printer" || kuma.Int(mon["interval"]) != 45 || !strings.Contains(jsonOf(mon["notificationIDList"]), kumaID(noteID)) {
		t.Fatalf("printer after sync: %v", mon)
	}

	// The saved tokens work: run pushes to both monitors.
	runner.MaxStartDelay = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var stderr syncBuffer
	env := Env{Stdout: io.Discard, Stderr: &stderr, ReadFile: os.ReadFile, Reload: make(chan struct{}),
		LookupEnv: func(string) (string, bool) { return "", false }}
	done := make(chan int)
	go func() { done <- Run(ctx, env, []string{"run", "-c", file}) }()
	for _, name := range []string{"nas", "printer"} {
		eventually(t, name+" in Kuma", time.Minute, func() bool {
			bs := kumaBeats(t, s, c2, ids[name])
			return len(bs) > 0 && bs[len(bs)-1].Status == 0 && bs[len(bs)-1].Msg == "connection refused"
		})
	}
	cancel()
	<-done
	if strings.Contains(stderr.String(), "cannot deliver") {
		t.Errorf("deliveries failed:\n%s", stderr.String())
	}
}
