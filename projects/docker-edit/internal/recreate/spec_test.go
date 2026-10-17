package recreate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func doc(t *testing.T, s string) Doc {
	t.Helper()
	var d Doc
	if err := json.Unmarshal([]byte(s), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestOlderAPIsConnectNetworksLater(t *testing.T) {
	ctr := doc(t, `{"Id":"0123456789abcdef","Name":"/web","Config":{"Image":"nginx","Hostname":"0123456789ab"},
		"HostConfig":{"NetworkMode":"front","Links":["/db:/web/database"]},
		"NetworkSettings":{"Networks":{
			"back":{"Aliases":["b","0123456789ab","web"],"IPAMConfig":{"IPv4Address":"10.0.0.5","IPv6Address":""}},
			"front":{"Aliases":["f"],"IPAMConfig":{},"DriverOpts":{}}}}}`)
	s, err := FromInspect(ctr)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Config["Hostname"]; ok {
		t.Error("the automatic hostname was kept")
	}
	if got := s.Host["Links"].([]any)[0]; got != "db:database" {
		t.Errorf("link %v", got)
	}
	old := s.Body("1.43")["NetworkingConfig"].(Doc)["EndpointsConfig"].(Doc)
	if len(old) != 1 || old["front"] == nil {
		t.Fatalf("API 1.43 endpoints at create: %v", old)
	}
	later := s.Later("1.43")
	if len(later) != 1 || later[0].Network != "back" {
		t.Fatalf("later: %+v", later)
	}
	back := later[0].Config
	if a := back["Aliases"].([]any); len(a) != 1 || a[0] != "b" {
		t.Errorf("aliases %v (the ID and name are added by Docker)", a)
	}
	if ip := back["IPAMConfig"].(Doc); len(ip) != 1 || ip["IPv4Address"] != "10.0.0.5" {
		t.Errorf("IPAM %v", ip)
	}
	if n := len(s.Body("1.44")["NetworkingConfig"].(Doc)["EndpointsConfig"].(Doc)); n != 2 || s.Later("1.44") != nil {
		t.Errorf("API 1.44 takes every endpoint at create (%d)", n)
	}
}

func TestDropImageDefaults(t *testing.T) {
	ctr := doc(t, `{"Id":"x","Name":"/a","Config":{"Image":"app:1","Env":["PATH=/bin","APP_VERSION=1","MINE=yes"],
		"Labels":{"maintainer":"them","mine":"yes"},"Cmd":["serve"],"User":"app","WorkingDir":"/srv"},"HostConfig":{}}`)
	img := doc(t, `{"Config":{"Env":["PATH=/bin","APP_VERSION=1"],"Labels":{"maintainer":"them"},"Cmd":["serve"],"WorkingDir":"/srv"}}`)
	s, _ := FromInspect(ctr)
	s.DropImageDefaults(img)
	if env := s.Config["Env"].([]any); len(env) != 1 || env[0] != "MINE=yes" {
		t.Errorf("env %v", env)
	}
	if l := s.Config["Labels"].(Doc); len(l) != 1 || l["mine"] != "yes" {
		t.Errorf("labels %v", l)
	}
	if _, ok := s.Config["Cmd"]; ok {
		t.Error("the image's command was kept")
	}
	if s.Config["User"] != "app" {
		t.Error("a user set on the container was dropped")
	}
}

func TestDropNewImageNames(t *testing.T) {
	s := &Spec{Config: Doc{
		"Env":    []any{"PATH=/bin", "APP_VERSION=1", "MINE=yes"},
		"Labels": Doc{"app.version": "1", "keep": "1"},
		"Cmd":    []any{"sleep", "3600"},
	}}
	newImage := Doc{"Config": Doc{
		"Env":    []any{"PATH=/bin", "APP_VERSION=9"},
		"Labels": Doc{"app.version": "9"},
		"Cmd":    []any{"sleep", "3600"},
	}}
	changed := s.DropNewImageNames(newImage)
	if got := strings.Join(changed, ","); got != "env APP_VERSION,label app.version" {
		t.Fatalf("changed %q", got)
	}
	if env := fmt.Sprint(s.Config["Env"]); env != "[MINE=yes]" {
		t.Fatalf("env %s", env)
	}
	if labels := fmt.Sprint(s.Config["Labels"]); labels != "map[keep:1]" {
		t.Fatalf("labels %s", labels)
	}
	if _, ok := s.Config["Cmd"]; ok {
		t.Fatal("the image's Cmd was kept")
	}
}

func TestMACAddress(t *testing.T) {
	s, err := FromInspect(doc(t, `{"Id": "0123456789abcdef", "Name": "/m", "Config": {"Image": "x", "MacAddress": "02:00:00:00:00:01"},
		"HostConfig": {"NetworkMode": "app"},
		"NetworkSettings": {"Networks": {"app": {"MacAddress": "02:00:00:00:00:01"}, "back": {"MacAddress": "02:00:00:00:00:02"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Config["MacAddress"]; ok || s.Networks[0].Config["MacAddress"] != "02:00:00:00:00:01" || s.Networks[1].Config["MacAddress"] != "02:00:00:00:00:02" {
		t.Fatalf("%v %v", s.Config, s.Networks)
	}
	newer, _ := json.Marshal(s.Body("1.44"))
	older, _ := json.Marshal(s.Body("1.43"))
	if string(newer) != `{"HostConfig":{"NetworkMode":"app"},"Image":"x","NetworkingConfig":{"EndpointsConfig":{"app":{"MacAddress":"02:00:00:00:00:01"},"back":{"MacAddress":"02:00:00:00:00:02"}}}}` ||
		string(older) != `{"HostConfig":{"NetworkMode":"app"},"Image":"x","MacAddress":"02:00:00:00:00:01","NetworkingConfig":{"EndpointsConfig":{"app":{}}}}` {
		t.Fatalf("bodies:\n%s\n%s", newer, older)
	}
	if s.Networks[0].Config["MacAddress"] == nil {
		t.Fatal("Body changed the spec")
	}
}

func TestOwnEntrypointKeepsTheCommand(t *testing.T) {
	s := &Spec{Config: Doc{"Entrypoint": []any{"/usr/bin/env"}, "Cmd": []any{"nginx", "-g", "daemon off;"}}}
	s.DropImageDefaults(Doc{"Config": Doc{"Entrypoint": []any{"/docker-entrypoint.sh"}, "Cmd": []any{"nginx", "-g", "daemon off;"}}})
	if fmt.Sprint(s.Config["Cmd"]) != "[nginx -g daemon off;]" || s.Config["Entrypoint"] == nil {
		t.Fatalf("%v", s.Config)
	}
	s = &Spec{Config: Doc{"Entrypoint": []any{"/docker-entrypoint.sh"}, "Cmd": []any{"nginx"}}}
	s.DropImageDefaults(Doc{"Config": Doc{"Entrypoint": []any{"/docker-entrypoint.sh"}, "Cmd": []any{"nginx"}}})
	if _, ok := s.Config["Cmd"]; ok {
		t.Fatalf("the image's command was kept: %v", s.Config)
	}
}
