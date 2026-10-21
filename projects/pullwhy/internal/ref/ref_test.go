package ref

import "testing"

func TestParse(t *testing.T) {
	d := "sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
	for in, want := range map[string]string{
		"nginx":                          "docker.io/library/nginx:latest",
		"nginx:alpine":                   "docker.io/library/nginx:alpine",
		"bitnami/redis:7":                "docker.io/bitnami/redis:7",
		"docker.io/nginx":                "docker.io/library/nginx:latest",
		"index.docker.io/library/nginx":  "docker.io/library/nginx:latest",
		"ghcr.io/o/r/sub:v1.2":           "ghcr.io/o/r/sub:v1.2",
		"localhost/x":                    "localhost/x:latest",
		"localhost:5000/x:1":             "localhost:5000/x:1",
		"10.0.0.5:5000/team/app":         "10.0.0.5:5000/team/app:latest",
		"[::1]:5000/app":                 "[::1]:5000/app:latest",
		"alpine@" + d:                    "docker.io/library/alpine@" + d,
		"alpine:3@" + d:                  "docker.io/library/alpine:3@" + d,
		"registry.example.com:443/a/b:c": "registry.example.com:443/a/b:c",
	} {
		r, err := Parse(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if r.String() != want {
			t.Errorf("%s: %s, want %s", in, r, want)
		}
	}
	r, _ := Parse("alpine@" + d)
	if r.Reference() != d || r.Tag != "" {
		t.Fatal(r)
	}
	if n, ref := r.FromImage(); n != "docker.io/library/alpine" || ref != d {
		t.Fatal(n, ref)
	}
}

func TestParseBad(t *testing.T) {
	for _, in := range []string{"", "Nginx", "nginx:", "nginx:-x", "a//b", "nginx@sha256:short", "bad_host:5000:x/y", "x/Y:1", "nginx@"} {
		if r, err := Parse(in); err == nil {
			t.Errorf("%q: parsed as %s", in, r)
		}
	}
}
