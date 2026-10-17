package edit

import (
	"strings"
	"testing"

	"github.com/ganeshphutane-g/docker-edit/internal/recreate"
)

func spec() *recreate.Spec {
	return &recreate.Spec{
		Name: "web",
		Config: Doc{
			"Image":  "nginx:1",
			"Env":    []any{"A=1", "API_TOKEN=s3cret"},
			"Labels": Doc{"keep": "1"},
		},
		Host: Doc{
			"PortBindings":  Doc{"80/tcp": []any{Doc{"HostIp": "", "HostPort": "8080"}}},
			"Binds":         []any{"/srv:/srv:ro"},
			"RestartPolicy": Doc{"Name": "no", "MaximumRetryCount": 0},
			"NetworkMode":   "app",
		},
		Networks:    []recreate.Endpoint{{Network: "app", Config: Doc{}}},
		NetworkMode: "app",
	}
}

func TestParsePublish(t *testing.T) {
	for in, want := range map[string]string{
		"80":                 "80/tcp  ",
		"8080:80":            "80/tcp  8080",
		"53:53/udp":          "53/udp  53",
		"127.0.0.1:8080:80":  "80/tcp 127.0.0.1 8080",
		"[::1]:8080:80/sctp": "80/sctp ::1 8080",
		"8000-8001:80":       "80/tcp  8000-8001",
	} {
		key, b, err := parsePublish(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := key + " " + str(b["HostIp"]) + " " + str(b["HostPort"]); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "http", "a:80", "1:2:3:4", "[::1:80", "[::1]:1:2:80", "80/icmp"} {
		if _, _, err := parsePublish(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestParseMount(t *testing.T) {
	m, err := parseMount("type=bind,src=/a,dst=/b,readonly,bind-propagation=rshared")
	if err != nil {
		t.Fatal(err)
	}
	if m["Type"] != "bind" || m["Source"] != "/a" || m["Target"] != "/b" || m["ReadOnly"] != true || asDoc(m["BindOptions"])["Propagation"] != "rshared" {
		t.Fatalf("%v", m)
	}
	if m, _ := parseMount("target=/c"); m["Type"] != "volume" {
		t.Fatalf("default type: %v", m)
	}
	for _, bad := range []string{"type=bind,src=/a", "target=rel", "target=/x,colour=red", "type=tmpfs,target=/t,tmpfs-size=1m"} {
		if _, err := parseMount(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestParseRestart(t *testing.T) {
	for in, want := range map[string]string{"no": "no 0", "always": "always 0", "unless-stopped": "unless-stopped 0", "on-failure": "on-failure 0", "on-failure:5": "on-failure 5"} {
		p, err := parseRestart(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if got := str(p["Name"]) + " " + strings.TrimSpace(jsonText(p["MaximumRetryCount"])); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
	for _, bad := range []string{"sometimes", "always:3", "on-failure:x", "on-failure:-1"} {
		if _, err := parseRestart(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestApplyAndDiff(t *testing.T) {
	before := spec()
	after := spec()
	err := Apply(after, Changes{
		Publish:   []string{"127.0.0.1:8443:443"},
		Unpublish: []string{"80"},
		Env:       []string{"A=2", "B=new", "API_TOKEN=other"},
		Volumes:   []string{"data:/data"},
		Unmount:   []string{"/srv"},
		Restart:   "unless-stopped",
		Labels:    []string{"new=yes"},
		Unlabel:   []string{"keep"},
		Image:     "nginx:2",
		Networks:  []string{"back:db,cache"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range Diff(before, after) {
		got = append(got, strings.TrimSpace(l.String()))
	}
	text := strings.Join(got, "\n")
	for _, want := range []string{
		"~ env A: 1 -> 2",
		"+ env B: new",
		"~ env API_TOKEN: (hidden) -> (hidden)",
		"~ image: nginx:1 -> nginx:2",
		"+ label new: yes",
		"- label keep (was 1)",
		"+ mount /data: data",
		"- mount /srv (was /srv (ro))",
		"+ network back: aliases db, cache",
		"- port 80/tcp (was 8080)",
		"+ port 443/tcp: 127.0.0.1:8443",
		"~ restart: no -> unless-stopped",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plan lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "ExposedPorts") {
		t.Errorf("published ports show as exposed too:\n%s", text)
	}
	if strings.Contains(text, "s3cret") || strings.Contains(text, "other") {
		t.Errorf("a secret value is shown:\n%s", text)
	}
	if d := Diff(before, spec()); len(d) != 0 {
		t.Errorf("no change shows %v", d)
	}
}

func TestApplyErrors(t *testing.T) {
	for _, c := range []struct {
		changes Changes
		want    string
	}{
		{Changes{Unpublish: []string{"81"}}, "does not publish 81/tcp"},
		{Changes{UnsetEnv: []string{"NOPE"}}, "no variable NOPE"},
		{Changes{Env: []string{"=x"}}, "expected NAME=value"},
		{Changes{Unmount: []string{"/nowhere"}}, "nothing is mounted"},
		{Changes{Volumes: []string{"data"}}, "expected SOURCE:/path"},
		{Changes{Unlabel: []string{"nope"}}, "no label nope"},
		{Changes{Disconnect: []string{"other"}}, "not on that network"},
		{Changes{Restart: "maybe"}, "expected no, always"},
	} {
		err := Apply(spec(), c.changes)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.changes, err)
		}
	}
	rm := spec()
	rm.Host["AutoRemove"] = true
	if err := Apply(rm, Changes{Restart: "always"}); err == nil || !strings.Contains(err.Error(), "--rm") {
		t.Errorf("--restart with --rm: %v", err)
	}
	host := spec()
	host.NetworkMode, host.Networks = "host", nil
	if err := Apply(host, Changes{Networks: []string{"app"}}); err != nil || host.NetworkMode != "app" {
		t.Errorf("leaving the host network: %v %s", err, host.NetworkMode)
	}
	shared := spec()
	shared.NetworkMode, shared.Networks = "container:abc", nil
	if err := Apply(shared, Changes{Networks: []string{"app"}}); err == nil {
		t.Error("joining a network while sharing another container's")
	}
}

func TestDisconnectTheMainNetwork(t *testing.T) {
	s := spec()
	s.Networks = append(s.Networks, recreate.Endpoint{Network: "back", Config: Doc{}})
	if err := Apply(s, Changes{Disconnect: []string{"app"}}); err != nil {
		t.Fatal(err)
	}
	if s.NetworkMode != "back" || s.Host["NetworkMode"] != "back" || len(s.Networks) != 1 {
		t.Fatalf("%s %v", s.NetworkMode, s.Networks)
	}
	if err := Apply(s, Changes{Disconnect: []string{"back"}}); err != nil || s.NetworkMode != "none" {
		t.Fatalf("last network: %v %s", err, s.NetworkMode)
	}
}

func TestOnlyRestart(t *testing.T) {
	if !(Changes{Restart: "always"}).OnlyRestart() || (Changes{Restart: "always", Env: []string{"A=1"}}).OnlyRestart() || (Changes{}).OnlyRestart() {
		t.Fatal("OnlyRestart")
	}
}

func TestEffectiveDoesNotChangeTheSpec(t *testing.T) {
	s := spec()
	img := Doc{"Config": Doc{"Env": []any{"PATH=/bin", "A=image"}, "Labels": Doc{"maintainer": "x"}, "Cmd": []any{"nginx"}}}
	e := Effective(s, img)
	e.Config["Image"] = "changed"
	if s.Config["Image"] != "nginx:1" {
		t.Fatal("Effective shares the spec's config")
	}
	env := jsonText(e.Config["Env"])
	if !strings.Contains(env, "PATH=/bin") || strings.Contains(env, "A=image") || !strings.Contains(env, "A=1") {
		t.Fatalf("env %s", env)
	}
	if e := Effective(s, Doc{}); e == s {
		t.Fatal("Effective without an image config returns the spec itself")
	}
}
