// Package ref reads image references the way the docker CLI does:
// "nginx" is docker.io/library/nginx:latest, "ghcr.io/o/r:tag" names its
// registry, and a first part with a dot, a colon or "localhost" is a host.
package ref

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// DockerHub is the registry of names without a host.
const DockerHub = "docker.io"

// Ref is a parsed reference.
type Ref struct {
	// Registry is the host (and port): docker.io, ghcr.io, 10.0.0.5:5000.
	Registry string
	// Repo is the repository: library/nginx, o/r.
	Repo string
	// Tag is set unless the reference is by digest only.
	Tag string
	// Digest is sha256:..., when given.
	Digest string
}

var (
	component = regexp.MustCompile(`^[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*$`)
	tagRe     = regexp.MustCompile(`^[\w][\w.-]{0,127}$`)
	digestRe  = regexp.MustCompile(`^[a-z0-9]+(?:[.+_-][a-z0-9]+)*:[a-zA-Z0-9=_-]{32,}$`)
	hostRe    = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?)*|\[[0-9a-fA-F:]+\])(?::[0-9]+)?$`)
)

// Parse reads s.
func Parse(s string) (*Ref, error) {
	if s == "" {
		return nil, errors.New("an image name is needed")
	}
	r := &Ref{}
	name := s
	if i := strings.Index(name, "@"); i >= 0 {
		r.Digest = name[i+1:]
		name = name[:i]
		if !digestRe.MatchString(r.Digest) {
			return nil, fmt.Errorf("%q: %q is not a digest such as sha256:…", s, r.Digest)
		}
	}
	// A tag follows the last colon after the last slash.
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		r.Tag = name[i+1:]
		name = name[:i]
		if !tagRe.MatchString(r.Tag) {
			return nil, fmt.Errorf("%q: %q is not a valid tag", s, r.Tag)
		}
	}
	parts := strings.Split(name, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		r.Registry = parts[0]
		parts = parts[1:]
		if !hostRe.MatchString(r.Registry) {
			return nil, fmt.Errorf("%q: %q is not a registry host", s, r.Registry)
		}
	} else {
		r.Registry = DockerHub
	}
	if r.Registry == "index.docker.io" || r.Registry == "registry-1.docker.io" {
		r.Registry = DockerHub
	}
	if r.Registry == DockerHub && len(parts) == 1 {
		parts = append([]string{"library"}, parts...)
	}
	for _, p := range parts {
		if !component.MatchString(p) {
			return nil, fmt.Errorf("%q: %q is not a valid repository name part (lowercase letters, digits, . _ -)", s, p)
		}
	}
	r.Repo = strings.Join(parts, "/")
	if len(r.Repo) > 255 {
		return nil, fmt.Errorf("%q: the repository name is too long", s)
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = "latest"
	}
	return r, nil
}

// Reference is the version to ask for: the digest if there is one, else the
// tag.
func (r *Ref) Reference() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// String is the full name: docker.io/library/nginx:latest.
func (r *Ref) String() string {
	s := r.Registry + "/" + r.Repo
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// FromImage is how the Engine API's /images/create names it: the name
// without tag, and the tag or digest separately.
func (r *Ref) FromImage() (string, string) {
	return r.Registry + "/" + r.Repo, r.Reference()
}
