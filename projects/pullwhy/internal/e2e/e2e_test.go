// Package e2e runs pullwhy side by side with real docker pulls, each in a
// scratch daemon set up to fail (or not) in one particular way. It needs
// PULLWHY_INTEGRATION=1 and Docker that can run privileged containers.
package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ganeshphutane-g/pullwhy/internal/testkit"
)

type world struct {
	gw         string
	plain, tls *testkit.Registry
	auth       *testkit.Registry
	mirror     *testkit.Registry // holds library/alpine:3, single-platform
	authMirror *testkit.Registry // the same, with a password
}

func setup(t *testing.T) *world {
	testkit.Integration(t)
	testkit.Binary(t)
	if out, err := testkit.Docker("image", "inspect", "alpine:3"); err != nil {
		if out, err = testkit.Docker("pull", "-q", "alpine:3"); err != nil {
			t.Fatalf("alpine:3 is needed: %s", out)
		}
	}
	w := &world{gw: testkit.Gateway(t)}
	w.plain = testkit.StartRegistry(t, w.gw, testkit.RegistryOptions{})
	w.plain.Push(t, "alpine:3", "team/app:1")
	w.plain.PutIndex(t, "team/app", "1", "amd64-only", "linux/amd64")
	w.tls = testkit.StartRegistry(t, w.gw, testkit.RegistryOptions{TLS: true})
	w.tls.Push(t, "alpine:3", "team/app:1")
	// The auth registry serves the plain one's storage, with a password.
	w.auth = testkit.StartRegistry(t, w.gw, testkit.RegistryOptions{User: "bob", Password: "s3cret", Data: w.plain.Data()})
	w.mirror = testkit.StartRegistry(t, w.gw, testkit.RegistryOptions{})
	w.mirror.Push(t, "alpine:3", "library/alpine:3")
	w.authMirror = testkit.StartRegistry(t, w.gw, testkit.RegistryOptions{User: "bob", Password: "s3cret", Data: w.mirror.Data()})
	return w
}

func insecure(regs ...string) string {
	return `{"insecure-registries": ["` + strings.Join(regs, `","`) + `"]}`
}

func dockerConfig(registry, user, pass string) []byte {
	auth := basicB64(user + ":" + pass)
	return []byte(fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, registry, auth))
}

// expect checks the real pull and pullwhy agree.
func expect(t *testing.T, p testkit.Pull, r *testkit.Report, pullOK bool, code int, kind string) {
	t.Helper()
	if (p.Code == 0) != pullOK {
		t.Errorf("docker pull: exit %d (want ok=%v)\n%s", p.Code, pullOK, p.Output)
	}
	if r.Code != code {
		t.Errorf("pullwhy: exit %d, want %d\n%s", r.Code, code, r.Raw)
	}
	if kind != "" && (len(r.Findings) == 0 || r.Findings[0].Kind != kind) {
		t.Errorf("pullwhy: findings %q, want %s first\n%s", r.Kinds(), kind, r.Raw)
	}
}

func TestSideBySide(t *testing.T) {
	w := setup(t)
	for _, store := range []string{"containerd", "classic"} {
		classic := store == "classic"
		t.Run(store, func(t *testing.T) {
			t.Run("working", func(t *testing.T) {
				d := testkit.StartDind(t, insecure(w.plain.Addr), classic)
				img := w.plain.Addr + "/team/app:1"
				expect(t, d.Pull(img), d.Pullwhy(img), true, 0, "")
			})
			t.Run("plain HTTP without insecure-registries", func(t *testing.T) {
				d := testkit.StartDind(t, "", classic)
				img := w.plain.Addr + "/team/app:1"
				expect(t, d.Pull(img), d.Pullwhy(img), false, 3, "http-on-https")
			})
			t.Run("private CA", func(t *testing.T) {
				d := testkit.StartDind(t, "", classic)
				img := w.tls.Addr + "/team/app:1"
				r := d.Pullwhy(img)
				expect(t, d.Pull(img), r, false, 3, "tls-unknown-authority")
				if len(r.Findings) > 0 && !strings.Contains(r.Findings[0].Fix, "/etc/docker/certs.d/"+w.tls.Addr+"/ca.crt") {
					t.Errorf("fix: %s", r.Findings[0].Fix)
				}
				// With the CA in certs.d, both work.
				d.Write("/etc/docker/certs.d/"+w.tls.Addr+"/ca.crt", w.tls.CA)
				expect(t, d.Pull(img), d.Pullwhy(img), true, 0, "")
			})
			t.Run("basic auth", func(t *testing.T) {
				d := testkit.StartDind(t, insecure(w.auth.Addr), classic)
				img := w.auth.Addr + "/team/app:1"
				r := d.Pullwhy(img)
				expect(t, d.Pull(img), r, false, 3, "unauthorized")
				if len(r.Findings) > 0 && !strings.Contains(r.Findings[0].Title, "needs a login") {
					t.Errorf("%s", r.Findings[0].Title)
				}
				d.Write("/root/.docker/config.json", dockerConfig(w.auth.Addr, "bob", "wrong"))
				r = d.Pullwhy(img)
				expect(t, d.Pull(img), r, false, 3, "unauthorized")
				if len(r.Findings) > 0 && !strings.Contains(r.Findings[0].Title, "refused the credentials") {
					t.Errorf("%s", r.Findings[0].Title)
				}
				d.Write("/root/.docker/config.json", dockerConfig(w.auth.Addr, "bob", "s3cret"))
				expect(t, d.Pull(img), d.Pullwhy(img), true, 0, "")
			})
			t.Run("missing tag", func(t *testing.T) {
				d := testkit.StartDind(t, insecure(w.plain.Addr), classic)
				img := w.plain.Addr + "/team/app:nope"
				expect(t, d.Pull(img), d.Pullwhy(img), false, 3, "not-found")
			})
			t.Run("missing platform", func(t *testing.T) {
				d := testkit.StartDind(t, insecure(w.plain.Addr), classic)
				img := w.plain.Addr + "/team/app:amd64-only"
				expect(t, d.Pull("--platform", "linux/s390x", img), d.Pullwhy("--deep", "--platform", "linux/s390x", img), false, 3, "platform")
			})
			t.Run("proxy", func(t *testing.T) {
				px := testkit.StartProxy(t)
				conf := fmt.Sprintf(`{"insecure-registries":[%q],"proxies":{"http-proxy":"http://%s:%d","https-proxy":"http://%s:%d"}}`,
					w.plain.Addr, w.gw, px.Port, w.gw, px.Port)
				d := testkit.StartDind(t, conf, classic)
				img := w.plain.Addr + "/team/app:1"
				r := d.Pullwhy(img)
				expect(t, d.Pull(img), r, true, 0, "")
				if len(r.Plan) == 0 || r.Plan[0].Proxy == "" {
					t.Errorf("the plan does not show the proxy: %s", r.Raw)
				}
				if !strings.Contains(strings.Join(px.Hosts(), " "), w.plain.Addr) {
					t.Errorf("nothing went through the proxy: %v", px.Hosts())
				}
				// A proxy that is not there.
				dead := fmt.Sprintf(`{"insecure-registries":[%q],"proxies":{"http-proxy":"http://%s:%d","https-proxy":"http://%s:%d"}}`,
					w.plain.Addr, w.gw, testkit.FreePort(t), w.gw, testkit.FreePort(t))
				d = testkit.StartDind(t, dead, classic)
				expect(t, d.Pull(img), d.Pullwhy(img), false, 3, "proxy")
			})
			t.Run("mirrors", func(t *testing.T) {
				// A mirror that holds a single-platform copy of alpine:3: the
				// pull uses it, and pullwhy notes it differs from Docker Hub.
				d := testkit.StartDind(t, fmt.Sprintf(`{"registry-mirrors":["http://%s"],"insecure-registries":[%q]}`, w.mirror.Addr, w.mirror.Addr), classic)
				hubOK(t, d)
				r := d.Pullwhy("alpine:3")
				expect(t, d.Pull("alpine:3"), r, true, 0, "")
				if !strings.Contains(r.Kinds(), "stale-mirror") {
					t.Errorf("%s", r.Raw)
				}
				// A mirror that drops packets: the pull is slow, and pullwhy
				// says why.
				d = testkit.StartDind(t, `{"registry-mirrors":["http://10.255.255.1:5000"]}`, classic)
				r = d.Pullwhy("--timeout", "3s", "alpine:3")
				p := d.Pull("alpine:3")
				expect(t, p, r, true, 4, "mirror-timeout")
				if p.Took < 14*time.Second {
					t.Errorf("the pull took %s; the dead mirror should have cost more", p.Took)
				}
				t.Logf("%s store: a pull with a dead mirror took %s", store, p.Took.Round(time.Second))

				// A mirror that asks for a login. Anonymous pulls fall back to
				// Docker Hub on both stores.
				d = testkit.StartDind(t, fmt.Sprintf(`{"registry-mirrors":["http://%s"],"insecure-registries":[%q]}`, w.authMirror.Addr, w.authMirror.Addr), classic)
				expect(t, d.Pull("alpine:3"), d.Pullwhy("alpine:3"), true, 0, "")
				// With Docker Hub credentials, the containerd store sends the
				// mirror none and stops there. (The credentials are not Hub's,
				// so on the classic store the pull fails at Hub instead.)
				d.Write("/root/.docker/config.json", dockerConfig("https://index.docker.io/v1/", "someone", "else"))
				if !classic {
					expect(t, d.Pull("alpine:3"), d.Pullwhy("alpine:3"), false, 3, "mirror-unauthorized")
				}
			})
			t.Run("docker hub", func(t *testing.T) {
				d := testkit.StartDind(t, "", classic)
				hubOK(t, d)
				// A name that is not there: Docker Hub answers as for a
				// private repository.
				r := d.Pullwhy("--pull", "library/no-such-image-pullwhy")
				expect(t, d.Pull("library/no-such-image-pullwhy"), r, false, 3, "not-found")
				if len(r.Findings) > 0 && !strings.Contains(r.Findings[0].Title, "not on Docker Hub, or it is private") {
					t.Errorf("%s", r.Findings[0].Title)
				}
			})
		})
	}
}

// hubOK skips tests that need Docker Hub when the scratch daemon's
// container cannot reach it (as where outgoing TLS is intercepted).
func hubOK(t *testing.T, d *testkit.Dind) {
	t.Helper()
	if out, code := d.Exec("wget", "-q", "-O", "/dev/null", "-T", "10",
		"https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/alpine:pull"); code != 0 {
		t.Skip("Docker Hub is out of reach from the scratch daemon:", strings.TrimSpace(out))
	}
}
