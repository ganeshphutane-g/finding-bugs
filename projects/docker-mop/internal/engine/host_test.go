package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeContext(t *testing.T, configDir, name, host string, withTLS bool) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	id := hex.EncodeToString(sum[:])
	dir := filepath.Join(configDir, "contexts", "meta", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"Name":"` + name + `","Endpoints":{"docker":{"Host":"` + host + `","SkipTLSVerify":false}}}`
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if withTLS {
		if err := os.MkdirAll(filepath.Join(configDir, "contexts", "tls", id, "docker"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResolveEndpointDockerHostWins(t *testing.T) {
	dir := t.TempDir()
	writeContext(t, dir, "remote", "tcp://10.0.0.1:2376", false)
	ep, err := ResolveEndpoint(envFrom(map[string]string{
		"DOCKER_HOST":    "tcp://127.0.0.1:2375",
		"DOCKER_CONTEXT": "remote",
		"DOCKER_CONFIG":  dir,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Host != "tcp://127.0.0.1:2375" || ep.Source != "DOCKER_HOST" || ep.TLS {
		t.Fatalf("got %+v", ep)
	}
}

func TestResolveEndpointTLSFromEnv(t *testing.T) {
	ep, err := ResolveEndpoint(envFrom(map[string]string{
		"DOCKER_HOST":       "tcp://example:2376",
		"DOCKER_TLS_VERIFY": "1",
		"DOCKER_CERT_PATH":  "/certs",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !ep.TLS || !ep.TLSVerify || ep.CertPath != "/certs" {
		t.Fatalf("got %+v", ep)
	}
}

func TestResolveEndpointDefault(t *testing.T) {
	ep, err := ResolveEndpoint(envFrom(map[string]string{"DOCKER_CONFIG": t.TempDir()}))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Host != DefaultHost || ep.Source != "default" {
		t.Fatalf("got %+v", ep)
	}
}

func TestResolveEndpointCurrentContext(t *testing.T) {
	dir := t.TempDir()
	writeContext(t, dir, "remote", "tcp://10.0.0.1:2376", true)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"remote"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ep, err := ResolveEndpoint(envFrom(map[string]string{"DOCKER_CONFIG": dir}))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Host != "tcp://10.0.0.1:2376" || ep.Source != "context remote" || !ep.TLS || !ep.TLSVerify {
		t.Fatalf("got %+v", ep)
	}
}

func TestResolveEndpointDockerContextEnv(t *testing.T) {
	dir := t.TempDir()
	writeContext(t, dir, "other", "unix:///tmp/other.sock", false)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"remote"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ep, err := ResolveEndpoint(envFrom(map[string]string{"DOCKER_CONFIG": dir, "DOCKER_CONTEXT": "other"}))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Host != "unix:///tmp/other.sock" || ep.TLS {
		t.Fatalf("got %+v", ep)
	}
}

func TestResolveEndpointDefaultContextName(t *testing.T) {
	ep, err := ResolveEndpoint(envFrom(map[string]string{"DOCKER_CONFIG": t.TempDir(), "DOCKER_CONTEXT": "default"}))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Host != DefaultHost {
		t.Fatalf("got %+v", ep)
	}
}

func TestResolveEndpointMissingContext(t *testing.T) {
	_, err := ResolveEndpoint(envFrom(map[string]string{"DOCKER_CONFIG": t.TempDir(), "DOCKER_CONTEXT": "nope"}))
	if err == nil {
		t.Fatal("expected an error for a missing context")
	}
}

func TestResolveEndpointBadConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveEndpoint(envFrom(map[string]string{"DOCKER_CONFIG": dir})); err == nil {
		t.Fatal("expected an error for a broken config.json")
	}
}
