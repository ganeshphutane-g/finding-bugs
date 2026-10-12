// Package endpoint turns the ways people name an S3 server into a connected
// client: URLs with credentials, environment variables, and MinIO client
// (mc) aliases.
package endpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Endpoint is one S3 server and the credentials to use with it.
type Endpoint struct {
	// Name is how the user referred to it (an alias, or the host).
	Name      string
	URL       *url.URL // scheme and host only
	AccessKey string
	SecretKey string
	Region    string
	// PathStyle forces path-style requests (http://host/bucket/key). Most
	// self-hosted servers need it; it is the default.
	PathStyle bool
}

// String shows the endpoint without its secret.
func (e Endpoint) String() string {
	return fmt.Sprintf("%s (%s, key %s)", e.Name, e.URL, e.AccessKey)
}

// Env is what Resolve needs from the outside world.
type Env struct {
	Getenv func(string) string
	// HomeDir is used to find ~/.mc/config.json.
	HomeDir string
}

var aliasRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Resolve understands, in order:
//   - a URL with credentials: https://ACCESS:SECRET@host:port[?region=r&path-style=false]
//   - a URL without credentials plus BUCKETMOVE_<ROLE>_ACCESS_KEY / _SECRET_KEY
//   - MC_HOST_<alias>, as the MinIO client uses it
//   - an alias from the MinIO client config (~/.mc/config.json, or MC_CONFIG_DIR)
//
// role is "from" or "to", used for the environment variable names.
func Resolve(ref, role string, env Env) (Endpoint, error) {
	if ref == "" {
		ref = env.Getenv("BUCKETMOVE_" + strings.ToUpper(role))
	}
	if ref == "" {
		return Endpoint{}, fmt.Errorf("no --%s endpoint given (a URL like https://KEY:SECRET@host:9000, or an mc alias)", role)
	}

	if strings.Contains(ref, "://") {
		ep, err := parseURL(ref, role, env)
		if err != nil {
			return Endpoint{}, err
		}
		return ep, nil
	}

	if !aliasRe.MatchString(ref) {
		return Endpoint{}, fmt.Errorf("%q is neither a URL nor an mc alias name", ref)
	}
	if v := env.Getenv("MC_HOST_" + ref); v != "" {
		ep, err := parseURL(v, role, env)
		if err != nil {
			return Endpoint{}, fmt.Errorf("MC_HOST_%s: %w", ref, err)
		}
		ep.Name = ref
		return ep, nil
	}
	return fromMCConfig(ref, env)
}

func parseURL(raw, role string, env Env) (Endpoint, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("invalid endpoint URL: %w", redact(err, raw))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Endpoint{}, fmt.Errorf("endpoint URL must start with http:// or https://, got %q", u.Scheme+"://")
	}
	if u.Host == "" {
		return Endpoint{}, errors.New("endpoint URL has no host")
	}
	if u.Path != "" && u.Path != "/" {
		return Endpoint{}, fmt.Errorf("endpoint URL must not have a path (%q); give buckets with --bucket", u.Path)
	}

	ep := Endpoint{
		Name:      u.Host,
		URL:       &url.URL{Scheme: u.Scheme, Host: u.Host},
		Region:    u.Query().Get("region"),
		PathStyle: u.Query().Get("path-style") != "false",
	}
	if u.User != nil {
		ep.AccessKey = u.User.Username()
		ep.SecretKey, _ = u.User.Password()
	}
	prefix := "BUCKETMOVE_" + strings.ToUpper(role) + "_"
	if ep.AccessKey == "" {
		ep.AccessKey = env.Getenv(prefix + "ACCESS_KEY")
	}
	if ep.SecretKey == "" {
		ep.SecretKey = env.Getenv(prefix + "SECRET_KEY")
	}
	if ep.AccessKey == "" || ep.SecretKey == "" {
		return Endpoint{}, fmt.Errorf("no credentials for %s: put them in the URL (https://KEY:SECRET@host) or set %sACCESS_KEY and %sSECRET_KEY", ep.Name, prefix, prefix)
	}
	return ep, nil
}

// redact keeps secrets out of URL parse errors.
func redact(err error, raw string) error {
	msg := err.Error()
	if u, perr := url.Parse(raw); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "REDACTED")
		}
	}
	return errors.New(msg)
}

type mcConfig struct {
	Aliases map[string]struct {
		URL       string `json:"url"`
		AccessKey string `json:"accessKey"`
		SecretKey string `json:"secretKey"`
		Path      string `json:"path"`
	} `json:"aliases"`
}

func mcConfigPath(env Env) string {
	if dir := env.Getenv("MC_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	return filepath.Join(env.HomeDir, ".mc", "config.json")
}

func fromMCConfig(alias string, env Env) (Endpoint, error) {
	path := mcConfigPath(env)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Endpoint{}, fmt.Errorf("unknown endpoint %q: not a URL, no MC_HOST_%s variable, and no mc config at %s", alias, alias, path)
	}
	if err != nil {
		return Endpoint{}, err
	}
	var cfg mcConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Endpoint{}, fmt.Errorf("reading %s: %w", path, err)
	}
	a, ok := cfg.Aliases[alias]
	if !ok {
		return Endpoint{}, fmt.Errorf("alias %q is not in %s", alias, path)
	}
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Endpoint{}, fmt.Errorf("alias %q in %s has an invalid url %q", alias, path, a.URL)
	}
	if a.AccessKey == "" || a.SecretKey == "" {
		return Endpoint{}, fmt.Errorf("alias %q in %s has no credentials", alias, path)
	}
	return Endpoint{
		Name:      alias,
		URL:       &url.URL{Scheme: u.Scheme, Host: u.Host},
		AccessKey: a.AccessKey,
		SecretKey: a.SecretKey,
		// mc's "path" is "auto", "on" or "off"; only "off" means virtual-host style.
		PathStyle: a.Path != "off",
	}, nil
}

// Client builds an S3 client for the endpoint.
func (e Endpoint) Client(transport http.RoundTripper) (*minio.Client, error) {
	lookup := minio.BucketLookupPath
	if !e.PathStyle {
		lookup = minio.BucketLookupDNS
	}
	return minio.New(e.URL.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(e.AccessKey, e.SecretKey, ""),
		Secure:       e.URL.Scheme == "https",
		Region:       e.Region,
		BucketLookup: lookup,
		Transport:    transport,
	})
}
