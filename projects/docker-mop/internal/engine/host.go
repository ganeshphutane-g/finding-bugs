package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultHost is the daemon address used when nothing else is configured.
const DefaultHost = "unix:///var/run/docker.sock"

// Endpoint describes where the Docker daemon is and how to reach it.
type Endpoint struct {
	Host string
	// Source says where Host came from, for error messages and `version`.
	Source string
	// TLS settings, only used for tcp:// hosts. TLS turns encryption on;
	// TLSVerify also checks the daemon's certificate.
	TLS       bool
	TLSVerify bool
	CertPath  string
}

// ResolveEndpoint finds the daemon the same way the docker CLI does:
// DOCKER_HOST, then DOCKER_CONTEXT, then the current context in config.json,
// then the default socket.
func ResolveEndpoint(getenv func(string) string) (Endpoint, error) {
	tlsVerify := getenv("DOCKER_TLS_VERIFY") != ""
	if host := getenv("DOCKER_HOST"); host != "" {
		ep := Endpoint{Host: host, Source: "DOCKER_HOST", TLS: tlsVerify || getenv("DOCKER_TLS") != "", TLSVerify: tlsVerify}
		if ep.TLS {
			ep.CertPath = getenv("DOCKER_CERT_PATH")
			if ep.CertPath == "" {
				dir, err := configDir(getenv)
				if err != nil {
					return Endpoint{}, err
				}
				ep.CertPath = dir
			}
		}
		return ep, nil
	}

	dir, err := configDir(getenv)
	if err != nil {
		return Endpoint{}, err
	}

	name := getenv("DOCKER_CONTEXT")
	if name == "" {
		name, err = currentContext(dir)
		if err != nil {
			return Endpoint{}, err
		}
	}
	if name == "" || name == "default" {
		return Endpoint{Host: DefaultHost, Source: "default"}, nil
	}

	return contextEndpoint(dir, name)
}

func configDir(getenv func(string) string) (string, error) {
	if dir := getenv("DOCKER_CONFIG"); dir != "" {
		return dir, nil
	}
	home := getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding the docker config directory: %w", err)
		}
	}
	return filepath.Join(home, ".docker"), nil
}

func currentContext(configDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading docker config: %w", err)
	}
	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parsing %s: %w", filepath.Join(configDir, "config.json"), err)
	}
	return cfg.CurrentContext, nil
}

// contextEndpoint reads a named context from the docker context store.
// The store keys each context by the sha256 of its name.
func contextEndpoint(configDir, name string) (Endpoint, error) {
	sum := sha256.Sum256([]byte(name))
	id := hex.EncodeToString(sum[:])
	metaPath := filepath.Join(configDir, "contexts", "meta", id, "meta.json")

	data, err := os.ReadFile(metaPath)
	if errors.Is(err, os.ErrNotExist) {
		return Endpoint{}, fmt.Errorf("docker context %q not found", name)
	}
	if err != nil {
		return Endpoint{}, fmt.Errorf("reading docker context %q: %w", name, err)
	}

	var meta struct {
		Endpoints map[string]struct {
			Host          string `json:"Host"`
			SkipTLSVerify bool   `json:"SkipTLSVerify"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return Endpoint{}, fmt.Errorf("parsing docker context %q: %w", name, err)
	}
	ep, ok := meta.Endpoints["docker"]
	if !ok || ep.Host == "" {
		return Endpoint{}, fmt.Errorf("docker context %q has no docker endpoint", name)
	}

	out := Endpoint{Host: ep.Host, Source: "context " + name}
	tlsDir := filepath.Join(configDir, "contexts", "tls", id, "docker")
	if _, err := os.Stat(tlsDir); err == nil {
		out.TLS = true
		out.TLSVerify = !ep.SkipTLSVerify
		out.CertPath = tlsDir
	} else if ep.SkipTLSVerify {
		out.TLS = true // TLS without certificate checks, like the docker CLI
	}
	return out, nil
}
