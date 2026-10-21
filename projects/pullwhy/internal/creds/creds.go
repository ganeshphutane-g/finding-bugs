// Package creds finds registry credentials the way the docker CLI does:
// credHelpers for the registry, else credsStore, else the auths entry of
// config.json. Secrets are kept here and never printed.
package creds

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// HubKey is how config.json and helpers name Docker Hub.
const HubKey = "https://index.docker.io/v1/"

// Auth is what was found for a registry.
type Auth struct {
	Username string
	Password string
	// IdentityToken is a refresh token (from docker login with a token
	// flow); it is traded for an access token.
	IdentityToken string
	// Source says where it came from, for the report: "config.json auths",
	// "credential helper desktop" and so on. Never the secret.
	Source string
}

// Anonymous reports whether there is nothing to log in with.
func (a *Auth) Anonymous() bool { return a == nil || a.Username == "" && a.IdentityToken == "" }

// Runner runs a credential helper's "get" with server on stdin.
type Runner func(ctx context.Context, helper, server string) ([]byte, error)

// RunHelper runs docker-credential-<helper> get.
func RunHelper(ctx context.Context, helper, server string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker-credential-"+helper, "get")
	cmd.Stdin = strings.NewReader(server)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(out.String() + " " + errb.String())
		return nil, fmt.Errorf("docker-credential-%s: %v %s", helper, err, msg)
	}
	return out.Bytes(), nil
}

// ErrNotFound means the helper or store has nothing for the registry.
var ErrNotFound = errors.New("no credentials stored for this registry")

type config struct {
	Auths map[string]struct {
		Auth          string `json:"auth"`
		Username      string `json:"username"`
		Password      string `json:"password"`
		IdentityToken string `json:"identitytoken"`
	} `json:"auths"`
	CredsStore  string            `json:"credsStore"`
	CredHelpers map[string]string `json:"credHelpers"`
}

// key is how registry is looked up: Hub's URL, or host[:port].
func key(registry string) string {
	if registry == "docker.io" || registry == "index.docker.io" || registry == "registry-1.docker.io" {
		return HubKey
	}
	return registry
}

// host strips a scheme and path from a config key.
func host(k string) string {
	k = strings.TrimPrefix(strings.TrimPrefix(k, "https://"), "http://")
	if i := strings.Index(k, "/"); i >= 0 {
		k = k[:i]
	}
	return k
}

// Lookup finds credentials for registry (docker.io, ghcr.io, host:port) in
// configDir's config.json. No config, or nothing for the registry, is
// anonymous (nil, nil).
func Lookup(ctx context.Context, configDir, registry string, run Runner) (*Auth, error) {
	b, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %v", filepath.Join(configDir, "config.json"), err)
	}
	k := key(registry)
	if run == nil {
		run = RunHelper
	}
	// credHelpers names a helper per registry.
	for name, helper := range c.CredHelpers {
		if name == k || host(name) == host(k) && k != HubKey {
			return fromHelper(ctx, run, helper, k)
		}
	}
	if c.CredsStore != "" {
		a, err := fromHelper(ctx, run, c.CredsStore, k)
		if a != nil || err != nil && !errors.Is(err, ErrNotFound) {
			return a, err
		}
		// Nothing in the store: the CLI pulls anonymously.
		return nil, nil
	}
	for name, e := range c.Auths {
		if name != k && (k == HubKey || host(name) != host(k)) {
			continue
		}
		a := &Auth{Username: e.Username, Password: e.Password, IdentityToken: e.IdentityToken, Source: "config.json auths"}
		if e.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(e.Auth)
			if err != nil {
				return nil, fmt.Errorf("config.json auths %q: the auth value is not base64", name)
			}
			user, pass, ok := strings.Cut(string(raw), ":")
			if !ok {
				return nil, fmt.Errorf("config.json auths %q: the auth value is not user:password", name)
			}
			a.Username, a.Password = user, pass
		}
		if a.Anonymous() {
			return nil, nil
		}
		return a, nil
	}
	return nil, nil
}

func fromHelper(ctx context.Context, run Runner, helper, server string) (*Auth, error) {
	out, err := run(ctx, helper, server)
	if err != nil {
		if strings.Contains(err.Error(), "credentials not found") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var r struct {
		Username string `json:"Username"`
		Secret   string `json:"Secret"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return nil, fmt.Errorf("docker-credential-%s: the answer is not JSON", helper)
	}
	a := &Auth{Source: "credential helper " + helper}
	// "<token>" as the user name marks an identity token.
	if r.Username == "<token>" {
		a.IdentityToken = r.Secret
	} else {
		a.Username, a.Password = r.Username, r.Secret
	}
	if a.Anonymous() {
		return nil, nil
	}
	return a, nil
}

// Header is the X-Registry-Auth value the docker CLI sends the daemon for a
// pull: base64url of a JSON auth config ("" when anonymous).
func (a *Auth) Header(serverAddress string) string {
	if a.Anonymous() {
		return ""
	}
	b, _ := json.Marshal(map[string]string{"username": a.Username, "password": a.Password,
		"identitytoken": a.IdentityToken, "serveraddress": serverAddress})
	return base64.URLEncoding.EncodeToString(b)
}

// ServerAddress is what the daemon should be told the credentials are for.
func ServerAddress(registry string) string { return key(registry) }
