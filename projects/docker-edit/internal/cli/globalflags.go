package cli

import (
	"strconv"
	"strings"
)

// globalFlags applies the docker CLI's global flags that come before the
// plugin name (docker --context prod edit web ...), by overriding the matching
// environment variables, and returns the arguments from the plugin name on.
func globalFlags(args []string, env Env) ([]string, Env, error) {
	overrides := map[string]string{}
	for len(args) > 0 && args[0] != pluginName {
		name, value, hasValue := strings.Cut(args[0], "=")
		takeValue := func() (string, error) {
			if hasValue {
				args = args[1:]
				return value, nil
			}
			if len(args) < 2 {
				return "", usagef("docker flag %s needs a value", name)
			}
			v := args[1]
			args = args[2:]
			return v, nil
		}
		var err error
		switch name {
		case "-c", "--context":
			overrides["DOCKER_CONTEXT"], err = takeValue()
		case "-H", "--host":
			overrides["DOCKER_HOST"], err = takeValue()
		case "--config":
			overrides["DOCKER_CONFIG"], err = takeValue()
		case "-l", "--log-level":
			_, err = takeValue()
		case "--tls", "--tlsverify":
			on := true
			if hasValue {
				if on, err = strconv.ParseBool(value); err != nil {
					return nil, env, usagef("docker flag %s=%s: expected true or false", name, value)
				}
			}
			key := map[string]string{"--tls": "DOCKER_TLS", "--tlsverify": "DOCKER_TLS_VERIFY"}[name]
			overrides[key] = ""
			if on {
				overrides[key] = "1"
			}
			args = args[1:]
		case "-D", "--debug":
			args = args[1:]
		default:
			return nil, env, usagef("docker flag %s is not supported by docker edit; set the matching DOCKER_* environment variable instead", name)
		}
		if err != nil {
			return nil, env, err
		}
	}
	if len(overrides) > 0 {
		base := env.Getenv
		env.Getenv = func(k string) string {
			if v, ok := overrides[k]; ok {
				return v
			}
			return base(k)
		}
		// An explicit host or context wins over the other source.
		// (The default context is DOCKER_HOST, if set.)
		if ctx, ok := overrides["DOCKER_CONTEXT"]; ok && ctx != "default" {
			if _, hostSet := overrides["DOCKER_HOST"]; !hostSet {
				overrides["DOCKER_HOST"] = ""
			}
		}
	}
	return args, env, nil
}
