package tailscale

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// From tailscale 1.104.2 in the lab (trimmed).
const status = `{
  "BackendState": "Running",
  "ExitNodeStatus": {"ID": "1", "Online": true, "TailscaleIPs": ["100.64.0.1/32"]},
  "Peer": {
    "nodekey:aa": {"ID": "1", "HostName": "sr", "DNSName": "sr.lab.example.", "TailscaleIPs": ["100.64.0.1", "fd7a:115c:a1e0::1"], "ExitNode": true, "ExitNodeOption": true, "Online": true},
    "nodekey:bb": {"ID": "7", "HostName": "nas", "DNSName": "nas-1.lab.example.", "TailscaleIPs": ["100.64.0.7"], "Online": false}
  }
}`

func TestParseAndFind(t *testing.T) {
	s, err := parseStatus([]byte(status))
	if err != nil || !s.Running() || s.ExitNodeID != "1" || len(s.Peers) != 2 {
		t.Fatalf("%+v %v", s, err)
	}
	for node, id := range map[string]string{"100.64.0.1": "1", "fd7a:115c:a1e0::1": "1", "sr": "1", "SR": "1", "sr.lab.example": "1", "sr.lab.example.": "1", "nas-1": "7", "nas": "", "100.64.0.9": "", "s": "", "lab": ""} {
		p, ok := s.Find(node)
		if ok != (id != "") || p.ID != id {
			t.Errorf("%s: %+v %v, want %q", node, p, ok, id)
		}
	}
	// Two peers with one name: ambiguous, as tailscale says too.
	two, _ := parseStatus([]byte(`{"Peer": {"a": {"ID": "1", "DNSName": "exit.a.ts.net."}, "b": {"ID": "2", "DNSName": "exit.b.ts.net."}}}`))
	if p, ok := two.Find("exit"); ok {
		t.Errorf("an ambiguous name found %+v", p)
	}
	if p, ok := two.Find("exit.b.ts.net"); !ok || p.ID != "2" {
		t.Errorf("the full name: %+v %v", p, ok)
	}
	if s, _ := parseStatus([]byte(`{"BackendState":"Stopped"}`)); s.Running() || s.ExitNodeID != "" {
		t.Errorf("%+v", s)
	}
	if _, err := parseStatus([]byte(`not json`)); err == nil {
		t.Error("no error")
	}
}

// fake writes a tailscale stand-in that records its arguments.
func fake(t *testing.T, script string) (*CLI, string) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "tailscale")
	os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"+script), 0o755)
	return &CLI{Bin: bin, Socket: "/run/ts.sock"}, log
}

func TestSetExitNode(t *testing.T) {
	c, log := fake(t, "")
	ctx := context.Background()
	if err := c.SetExitNode(ctx, "sr", false); err != nil {
		t.Fatal(err)
	}
	if err := c.SetExitNode(ctx, "", false); err != nil {
		t.Fatal(err)
	}
	if err := c.SetExitNode(ctx, "--operator=me", false); err == nil {
		t.Error("a flag was accepted as a node")
	}
	b, _ := os.ReadFile(log)
	want := "--socket=/run/ts.sock set --exit-node=sr --exit-node-allow-lan-access=false\n--socket=/run/ts.sock set --exit-node=\n"
	if string(b) != want {
		t.Errorf("calls:\n%s\nwant:\n%s", b, want)
	}
}

func TestErrorsCarryTheMessage(t *testing.T) {
	c, _ := fake(t, `echo 'invalid value "nosuch" for --exit-node; must be IP or peer hostname' >&2; exit 1`)
	err := c.SetExitNode(context.Background(), "nosuch", false)
	if err == nil || !strings.Contains(err.Error(), "tailscale set: exit status 1: invalid value") {
		t.Errorf("%v", err)
	}
	c, _ = fake(t, `echo '{"BackendState":"Running"}'`)
	if s, err := c.Status(context.Background()); err != nil || !s.Running() {
		t.Errorf("%+v %v", s, err)
	}
}
