package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// write makes a self-signed certificate for name and writes it and its key.
func write(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	cf, kf := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(cf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(kf, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return cf, kf
}

func name(t *testing.T, r *Reloader) string {
	t.Helper()
	c, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	x, _ := x509.ParseCertificate(c.Certificate[0])
	return x.Subject.CommonName
}

func TestReload(t *testing.T) {
	dir := t.TempDir()
	cf, kf := write(t, dir, "one.test")
	r, err := New(cf, kf)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	r.now = func() time.Time { return now }
	var events []error
	r.OnReload = func(err error) { events = append(events, err) }
	if name(t, r) != "one.test" {
		t.Fatal("first")
	}
	// A renewal: seen at the next check, not before.
	write(t, dir, "two.test")
	os.Chtimes(cf, now.Add(time.Minute), now.Add(time.Minute))
	if name(t, r) != "one.test" {
		t.Fatal("reloaded before Every")
	}
	now = now.Add(r.Every)
	if name(t, r) != "two.test" || len(events) != 1 || events[0] != nil {
		t.Fatal("not reloaded", events)
	}
	// A half-written file: the old certificate stays, one error is reported.
	os.WriteFile(kf, []byte("-----BEGIN"), 0o600)
	now = now.Add(r.Every)
	if name(t, r) != "two.test" || len(events) != 2 || events[1] == nil {
		t.Fatal("a broken file replaced the certificate", events)
	}
	now = now.Add(r.Every)
	if name(t, r) != "two.test" || len(events) != 2 {
		t.Fatal("the same broken file was reported again", events)
	}
	// Fixed: picked up.
	write(t, dir, "three.test")
	os.Chtimes(cf, now.Add(2*time.Minute), now.Add(2*time.Minute))
	os.Chtimes(kf, now.Add(2*time.Minute), now.Add(2*time.Minute))
	now = now.Add(r.Every)
	if name(t, r) != "three.test" {
		t.Fatal("not picked up after the fix")
	}
}

func TestNewErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(filepath.Join(dir, "no.pem"), filepath.Join(dir, "no.key")); err == nil {
		t.Fatal("missing files")
	}
	cf, _ := write(t, dir, "x.test")
	if _, err := New(cf, cf); err == nil {
		t.Fatal("a certificate as its own key")
	}
}
