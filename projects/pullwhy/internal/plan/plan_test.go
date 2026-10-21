package plan

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/ganeshphutane-g/pullwhy/internal/engine"
	"github.com/ganeshphutane-g/pullwhy/internal/ref"
)

func info() *engine.Info {
	i := &engine.Info{HTTPSProxy: "http://proxy:3128", HTTPProxy: "http://proxy:3128", NoProxy: "registry.internal,10.0.0.0/8"}
	i.DriverStatus = [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}}
	i.RegistryConfig.InsecureRegistryCIDRs = []string{"127.0.0.0/8", "::1/128"}
	i.RegistryConfig.IndexConfigs = map[string]engine.IndexConfig{
		"docker.io":      {Name: "docker.io", Secure: true, Official: true, Mirrors: []string{"http://10.0.0.9:5000/", "https://mirror.gcr.io/"}},
		"old.local:5000": {Name: "old.local:5000", Secure: false},
	}
	return i
}

func resolver(m map[string]string) Resolver {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		if a, ok := m[host]; ok {
			return []netip.Addr{netip.MustParseAddr(a)}, nil
		}
		return nil, nil
	}
}

func TestHub(t *testing.T) {
	r, _ := ref.Parse("nginx:alpine")
	p, err := Make(context.Background(), r, info(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Containerd || len(p.Endpoints) != 3 {
		t.Fatalf("%+v", p)
	}
	m1, m2, hub := p.Endpoints[0], p.Endpoints[1], p.Endpoints[2]
	if !m1.Mirror || m1.Base != "http://10.0.0.9:5000" || m1.Proxy != nil { // 10.0.0.0/8 is in NoProxy
		t.Fatalf("%+v", m1)
	}
	if !m2.Mirror || m2.Proxy == nil || m2.Proxy.Host != "proxy:3128" {
		t.Fatalf("%+v", m2)
	}
	if hub.Mirror || hub.Base != "https://registry-1.docker.io" || hub.Insecure {
		t.Fatalf("%+v", hub)
	}
}

func TestOtherRegistries(t *testing.T) {
	ctx := context.Background()
	look := resolver(map[string]string{"reg.lan": "127.0.0.5"})
	for name, want := range map[string]struct {
		n        int
		insecure bool
		proxied  bool
	}{
		"ghcr.io/o/r":           {1, false, true},
		"registry.internal/a/b": {1, false, false}, // NoProxy
		"old.local:5000/x":      {2, true, true},   // insecure-registries
		"reg.lan:5000/x":        {2, true, true},   // resolves into 127.0.0.0/8
		"127.0.0.1:5001/x":      {2, true, false},  // loopback: never proxied
		"[::1]:5000/x":          {2, true, false},
	} {
		r, err := ref.Parse(name)
		if err != nil {
			t.Fatal(name, err)
		}
		p, err := Make(ctx, r, info(), look, false)
		if err != nil {
			t.Fatal(name, err)
		}
		e := p.Endpoints[0]
		if len(p.Endpoints) != want.n || e.Insecure != want.insecure || (e.Proxy != nil) != want.proxied || e.Mirror {
			t.Errorf("%s: %+v", name, p.Endpoints)
		}
		if want.n == 2 && p.Endpoints[1].Base != "http://"+r.Registry {
			t.Errorf("%s: %+v", name, p.Endpoints[1])
		}
	}
}

func TestCertsDir(t *testing.T) {
	old := CertsRoot
	CertsRoot = t.TempDir()
	defer func() { CertsRoot = old }()
	os.MkdirAll(filepath.Join(CertsRoot, "reg.example:5443"), 0o755)
	os.WriteFile(filepath.Join(CertsRoot, "reg.example:5443", "ca.crt"), []byte("x"), 0o644)
	r, _ := ref.Parse("reg.example:5443/x")
	p, _ := Make(context.Background(), r, info(), nil, true)
	if e := p.Endpoints[0]; e.CertsDir == "" || len(e.CertFiles) != 1 || e.CertFiles[0] != "ca.crt" {
		t.Fatalf("%+v", e)
	}
	// Not looked at when the daemon is elsewhere.
	p, _ = Make(context.Background(), r, info(), nil, false)
	if p.Endpoints[0].CertsDir != "" {
		t.Fatal("certs.d read for a remote daemon")
	}
	i := info()
	i.RegistryConfig.Mirrors = []string{"ftp://x"}
	i.RegistryConfig.IndexConfigs = nil
	r, _ = ref.Parse("alpine")
	if _, err := Make(context.Background(), r, i, nil, false); err == nil {
		t.Fatal("an ftp mirror was accepted")
	}
}
