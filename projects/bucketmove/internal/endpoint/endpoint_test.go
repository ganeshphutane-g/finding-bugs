package endpoint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(vars map[string]string, home string) Env {
	return Env{Getenv: func(k string) string { return vars[k] }, HomeDir: home}
}

func TestResolveURL(t *testing.T) {
	ep, err := Resolve("https://AK:S%2Fsecret@s3.example.com:9000?region=garage", "from", envOf(nil, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if ep.URL.String() != "https://s3.example.com:9000" || ep.AccessKey != "AK" || ep.SecretKey != "S/secret" || ep.Region != "garage" || !ep.PathStyle {
		t.Fatalf("got %+v", ep)
	}
	if strings.Contains(ep.String(), "secret") {
		t.Fatalf("String() leaks the secret: %s", ep)
	}
}

func TestResolveURLWithEnvCredentials(t *testing.T) {
	ep, err := Resolve("http://localhost:9000", "to", envOf(map[string]string{
		"BUCKETMOVE_TO_ACCESS_KEY": "k", "BUCKETMOVE_TO_SECRET_KEY": "s",
	}, t.TempDir()))
	if err != nil || ep.AccessKey != "k" || ep.SecretKey != "s" {
		t.Fatalf("got %+v %v", ep, err)
	}
	if _, err := Resolve("http://localhost:9000", "to", envOf(nil, t.TempDir())); err == nil || !strings.Contains(err.Error(), "BUCKETMOVE_TO_ACCESS_KEY") {
		t.Fatalf("expected a hint about the env vars, got %v", err)
	}
}

func TestResolveFromEnvRef(t *testing.T) {
	ep, err := Resolve("", "from", envOf(map[string]string{"BUCKETMOVE_FROM": "http://a:b@h:1"}, t.TempDir()))
	if err != nil || ep.URL.Host != "h:1" {
		t.Fatalf("got %+v %v", ep, err)
	}
	if _, err := Resolve("", "from", envOf(nil, t.TempDir())); err == nil {
		t.Fatal("expected an error with no endpoint")
	}
}

func TestResolveMCHost(t *testing.T) {
	ep, err := Resolve("old", "from", envOf(map[string]string{"MC_HOST_old": "https://k:s@minio.lan"}, t.TempDir()))
	if err != nil || ep.Name != "old" || ep.URL.Host != "minio.lan" {
		t.Fatalf("got %+v %v", ep, err)
	}
}

func TestResolveMCConfigAlias(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".mc"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{"version":"10","aliases":{
		"myminio":{"url":"http://192.168.1.5:9000","accessKey":"ak","secretKey":"sk","api":"s3v4","path":"auto"},
		"cloud":{"url":"https://s3.amazonaws.com","accessKey":"a","secretKey":"b","api":"s3v4","path":"off"},
		"broken":{"url":"ftp://x","accessKey":"a","secretKey":"b"}}}`
	if err := os.WriteFile(filepath.Join(home, ".mc", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	env := envOf(nil, home)

	ep, err := Resolve("myminio", "from", env)
	if err != nil || ep.URL.String() != "http://192.168.1.5:9000" || ep.AccessKey != "ak" || !ep.PathStyle {
		t.Fatalf("got %+v %v", ep, err)
	}
	ep, err = Resolve("cloud", "to", env)
	if err != nil || ep.PathStyle {
		t.Fatalf("path off should mean virtual-host style: %+v %v", ep, err)
	}
	for _, bad := range []string{"missing", "broken", "not an alias!"} {
		if _, err := Resolve(bad, "from", env); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestResolveRejectsBadURLs(t *testing.T) {
	for _, bad := range []string{"ftp://a:b@h", "http://a:b@h/bucket", "http://"} {
		if _, err := Resolve(bad, "from", envOf(nil, t.TempDir())); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestParseErrorsNeverShowSecrets(t *testing.T) {
	for _, raw := range []string{
		"http://AKIA:TOPSECRET@127.0.0.1:19x00",
		"http://AKIA:abc/TOPSECRET@host",
		"http://AKIA:TOP%zzSECRET@host",
		"http://AKIA:TOPSECRET@host\n",
	} {
		_, err := Resolve(raw, "from", envOf(nil, t.TempDir()))
		if err != nil && strings.Contains(err.Error(), "TOPSECRET") {
			t.Errorf("%q: secret in error: %v", raw, err)
		}
		_, err = Resolve("x", "from", envOf(map[string]string{"MC_HOST_x": raw}, t.TempDir()))
		if err != nil && strings.Contains(err.Error(), "TOPSECRET") {
			t.Errorf("MC_HOST %q: secret in error: %v", raw, err)
		}
	}
}

func TestMCHostSessionToken(t *testing.T) {
	ep, err := Resolve("tmp", "from", envOf(map[string]string{"MC_HOST_tmp": "https://AK:SK:TOKEN@s3.lan"}, t.TempDir()))
	if err != nil || ep.SecretKey != "SK" || ep.SessionToken != "TOKEN" {
		t.Fatalf("got %+v %v", ep, err)
	}
}

func TestMCPathStyle(t *testing.T) {
	cases := []struct {
		setting, host string
		want          bool
	}{
		{"auto", "minio.lan", true}, {"", "minio.lan", true}, {"auto", "s3.eu-west-1.amazonaws.com", false},
		{"Off", "minio.lan", false}, {"on", "s3.amazonaws.com", true},
	}
	for _, c := range cases {
		if got := mcPathStyle(c.setting, c.host); got != c.want {
			t.Errorf("%q %q: got %v", c.setting, c.host, got)
		}
	}
}
