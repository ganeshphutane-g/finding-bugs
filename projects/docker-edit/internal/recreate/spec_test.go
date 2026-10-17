package recreate

import (
	"encoding/json"
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
