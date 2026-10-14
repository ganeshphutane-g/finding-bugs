package images

import "testing"

func TestTarget(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"postgres:14", "postgres:18"},
		{"postgres:14.5", "postgres:18"},
		{"postgres:14-alpine", "postgres:18-alpine"},
		{"postgres:14.5-alpine3.20", "postgres:18-alpine"},
		{"postgres:14-bookworm", "postgres:18"},
		{"postgres:alpine", "postgres:18-alpine"},
		{"docker.io/library/postgres:13", "docker.io/library/postgres:18"},
		{"pgvector/pgvector:pg16", "pgvector/pgvector:pg18"},
		{"pgvector/pgvector:0.8.0-pg16", "pgvector/pgvector:pg18"},
		{"timescale/timescaledb:2.14.2-pg16", "timescale/timescaledb:latest-pg18"},
		{"timescale/timescaledb:latest-pg15-oss", "timescale/timescaledb:latest-pg18-oss"},
	} {
		got, err := Target(c.in, 18)
		if err != nil || got != c.want {
			t.Errorf("Target(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"postgis/postgis:16-3.4", "ghcr.io/immich-app/postgres:14-vectorchord0.4.3", "postgres@sha256:abc", "registry:5000/pg:14"} {
		if got, err := Target(bad, 18); err == nil {
			t.Errorf("Target(%q) = %q; want an error asking for --image", bad, got)
		}
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]Ref{
		"postgres":                 {Repo: "postgres"},
		"postgres:14":              {Repo: "postgres", Tag: "14"},
		"registry:5000/team/pg:14": {Repo: "registry:5000/team/pg", Tag: "14"},
		"registry:5000/team/pg":    {Repo: "registry:5000/team/pg"},
		"postgres:14@sha256:abc":   {Repo: "postgres", Tag: "14", Digest: "sha256:abc"},
	} {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %+v, want %+v", in, got, want)
		}
		if got := Parse(in).String(); got != in {
			t.Errorf("String() = %q, want %q", got, in)
		}
	}
}
