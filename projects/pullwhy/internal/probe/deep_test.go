package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func deepRegistry(t *testing.T) (*httptest.Server, *httptest.Server, *[]string) {
	var cdnAuth []string
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnAuth = append(cdnAuth, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(make([]byte, 300<<10))
	}))
	t.Cleanup(cdn.Close)
	idx := `{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
 {"digest":"sha256:aaaa","platform":{"os":"linux","architecture":"amd64"}},
 {"digest":"sha256:bbbb","platform":{"os":"linux","architecture":"arm","variant":"v7"}},
 {"digest":"sha256:att","platform":{"os":"unknown","architecture":"unknown"}}]}`
	man := `{"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[
 {"digest":"sha256:small","size":100},{"digest":"sha256:big","size":3000000}]}`
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/manifests/1"):
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			fmt.Fprint(w, idx)
		case strings.HasSuffix(r.URL.Path, "/manifests/sha256:aaaa"), strings.HasSuffix(r.URL.Path, "/manifests/single"):
			fmt.Fprint(w, man)
		case strings.HasSuffix(r.URL.Path, "/blobs/sha256:big"):
			if r.Header.Get("Range") == "" {
				w.WriteHeader(400)
				return
			}
			http.Redirect(w, r, cdn.URL+"/blob/big", http.StatusTemporaryRedirect)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(reg.Close)
	return reg, cdn, &cdnAuth
}

func TestDeep(t *testing.T) {
	reg, cdn, cdnAuth := deepRegistry(t)
	res := &Result{Endpoint: endpoint(reg.URL), Token: "tok"}
	d := DeepCheck(context.Background(), res, mustRef("example.com/x:1"), Options{}, Platform{OS: "linux", Arch: "amd64"}, 256<<10, 2*time.Second)
	if d.Kind != "" || d.Missing || d.Layer != "sha256:big" || d.LayerSize != 3000000 {
		t.Fatalf("%+v", d)
	}
	if strings.Join(d.Platforms, " ") != "linux/amd64 linux/arm/v7" {
		t.Fatal(d.Platforms)
	}
	if len(d.Hops) != 2 || d.Hops[0].Status != 307 || d.Hops[1].Host != strings.TrimPrefix(cdn.URL, "http://") || d.Read != 256<<10 || d.Rate() <= 0 {
		t.Fatalf("%+v", d.Hops)
	}
	// The CDN got no registry token.
	if (*cdnAuth)[0] != "" {
		t.Fatal("the token went to the CDN")
	}
	// A platform the index lacks.
	d = DeepCheck(context.Background(), res, mustRef("example.com/x:1"), Options{}, Platform{OS: "linux", Arch: "s390x"}, 1<<10, time.Second)
	if !d.Missing || d.Layer != "" {
		t.Fatalf("%+v", d)
	}
	// A single-platform manifest needs no index.
	d = DeepCheck(context.Background(), res, mustRef("example.com/x:single"), Options{}, Platform{OS: "linux", Arch: "arm64"}, 1<<10, time.Second)
	if d.Missing || d.Layer != "sha256:big" {
		t.Fatalf("%+v", d)
	}
	d = DeepCheck(context.Background(), res, mustRef("example.com/x:gone"), Options{}, Platform{OS: "linux", Arch: "amd64"}, 1<<10, time.Second)
	if d.Kind != KindNotFound {
		t.Fatalf("%+v", d)
	}
}

func TestPlatforms(t *testing.T) {
	for in, want := range map[[2]string]string{{"linux", "x86_64"}: "linux/amd64", {"linux", "aarch64"}: "linux/arm64", {"linux", "armv7l"}: "linux/arm/v7", {"", "riscv64"}: "linux/riscv64"} {
		if got := DaemonPlatform(in[0], in[1]).String(); got != want {
			t.Errorf("%v: %s", in, got)
		}
	}
	if p, err := ParsePlatform("linux/arm/v7"); err != nil || p.Variant != "v7" {
		t.Fatal(p, err)
	}
	for _, bad := range []string{"linux", "linux/", "a/b/c/d"} {
		if _, err := ParsePlatform(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}
