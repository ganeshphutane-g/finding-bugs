package creds

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, conf string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(conf), 0o600)
	return dir
}

func TestAuths(t *testing.T) {
	ctx := context.Background()
	dir := writeConfig(t, `{"auths":{
  "https://index.docker.io/v1/":{"auth":"`+base64.StdEncoding.EncodeToString([]byte("hubuser:hubpass"))+`"},
  "ghcr.io":{"auth":"`+base64.StdEncoding.EncodeToString([]byte("gh:tok:with:colons"))+`"},
  "https://reg.example:5000/v2/":{"identitytoken":"refresh"},
  "empty.example":{}}}`)
	for reg, want := range map[string]string{"docker.io": "hubuser/hubpass", "ghcr.io": "gh/tok:with:colons", "reg.example:5000": "/", "quay.io": "anon", "empty.example": "anon"} {
		a, err := Lookup(ctx, dir, reg, nil)
		if err != nil {
			t.Fatal(reg, err)
		}
		got := "anon"
		if !a.Anonymous() {
			got = a.Username + "/" + a.Password
		}
		if got != want {
			t.Errorf("%s: %s, want %s", reg, got, want)
		}
	}
	a, _ := Lookup(ctx, dir, "reg.example:5000", nil)
	if a.IdentityToken != "refresh" || a.Source != "config.json auths" {
		t.Fatalf("%+v", a)
	}
	// No config at all: anonymous.
	if a, err := Lookup(ctx, t.TempDir(), "docker.io", nil); a != nil || err != nil {
		t.Fatal(a, err)
	}
	if _, err := Lookup(ctx, writeConfig(t, `{"auths":{"ghcr.io":{"auth":"%%%"}}}`), "ghcr.io", nil); err == nil {
		t.Fatal("bad base64 accepted")
	}
}

func TestHelpers(t *testing.T) {
	ctx := context.Background()
	var asked []string
	run := func(_ context.Context, helper, server string) ([]byte, error) {
		asked = append(asked, helper+" "+server)
		switch {
		case helper == "ecr-login" && server == "123.dkr.ecr.eu-west-1.amazonaws.com":
			return []byte(`{"ServerURL":"x","Username":"AWS","Secret":"ecrpass"}`), nil
		case helper == "desktop" && server == HubKey:
			return []byte(`{"ServerURL":"x","Username":"<token>","Secret":"idtok"}`), nil
		case helper == "desktop":
			return nil, errors.New("docker-credential-desktop: exit status 1 credentials not found in native keychain")
		}
		return nil, errors.New("docker-credential-" + helper + ": executable file not found in $PATH")
	}
	dir := writeConfig(t, `{"credsStore":"desktop","credHelpers":{"123.dkr.ecr.eu-west-1.amazonaws.com":"ecr-login","gcr.io":"gcloud"},"auths":{"ghcr.io":{}}}`)
	a, err := Lookup(ctx, dir, "123.dkr.ecr.eu-west-1.amazonaws.com", run)
	if err != nil || a.Username != "AWS" || a.Password != "ecrpass" || a.Source != "credential helper ecr-login" {
		t.Fatalf("%+v %v", a, err)
	}
	a, err = Lookup(ctx, dir, "docker.io", run)
	if err != nil || a.IdentityToken != "idtok" || a.Username != "" {
		t.Fatalf("%+v %v", a, err)
	}
	// The store has nothing for ghcr.io: anonymous, not an error.
	if a, err := Lookup(ctx, dir, "ghcr.io", run); a != nil || err != nil {
		t.Fatal(a, err)
	}
	// A helper that is not installed is an error the report can show.
	if _, err := Lookup(ctx, dir, "gcr.io", run); err == nil || !strings.Contains(err.Error(), "not found in $PATH") {
		t.Fatal(err)
	}
}

func TestHeader(t *testing.T) {
	a := &Auth{Username: "u", Password: "p"}
	raw, err := base64.URLEncoding.DecodeString(a.Header(ServerAddress("docker.io")))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]string
	json.Unmarshal(raw, &m)
	if m["username"] != "u" || m["password"] != "p" || m["serveraddress"] != HubKey {
		t.Fatal(m)
	}
	var none *Auth
	if none.Header("x") != "" || !none.Anonymous() {
		t.Fatal("anonymous header")
	}
}

func TestHubHelperKey(t *testing.T) {
	run := func(_ context.Context, helper, server string) ([]byte, error) {
		return []byte(`{"Username":"u","Secret":"from-` + helper + `"}`), nil
	}
	dir := writeConfig(t, `{"credHelpers":{"https://index.docker.io/v1/":"pass","index.docker.io":"wrong"}}`)
	a, err := Lookup(context.Background(), dir, "docker.io", run)
	if err != nil || a == nil || a.Password != "from-pass" {
		t.Fatalf("%+v %v", a, err)
	}
}
