package config

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// WriteEnv writes secrets as an environment file that both a POSIX shell
// (set -a; . FILE) and docker compose (env_file) read the same way.
func WriteEnv(w io.Writer, secrets []Secret) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, "# Secrets for the kumafile configuration. Keep this file private.")
	for _, s := range secrets {
		fmt.Fprintf(bw, "%s=%s\n", s.Name, quote(s.Value))
	}
	return bw.Flush()
}

func quote(v string) string {
	if !strings.Contains(v, "'") {
		return "'" + v + "'"
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	return `"` + r.Replace(v) + `"`
}

// ReadEnv reads a file written by WriteEnv (or a simple KEY=value file).
func ReadEnv(r io.Reader) (map[string]string, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	s := string(data)
	line := 1
	for len(s) > 0 {
		// Skip blank lines and comments.
		if s[0] == '\n' {
			s, line = s[1:], line+1
			continue
		}
		if t := strings.TrimLeft(s, " \t\r"); t != s {
			s = t
			continue
		}
		if s[0] == '#' {
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s, line = s[i+1:], line+1
			} else {
				s = ""
			}
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		eq := strings.IndexByte(s, '=')
		nl := strings.IndexByte(s, '\n')
		if eq < 0 || (nl >= 0 && nl < eq) {
			return nil, fmt.Errorf("line %d: expected NAME=value", line)
		}
		name := strings.TrimSpace(s[:eq])
		s = strings.TrimLeft(s[eq+1:], " \t")
		var val string
		switch {
		case strings.HasPrefix(s, "'"):
			end := strings.IndexByte(s[1:], '\'')
			if end < 0 {
				return nil, fmt.Errorf("line %d: %s: unterminated quote", line, name)
			}
			val = s[1 : end+1]
			s = s[end+2:]
		case strings.HasPrefix(s, `"`):
			var b strings.Builder
			i := 1
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
					switch s[i] {
					case 'n':
						b.WriteByte('\n')
					case '\\', '"', '$', '`':
						b.WriteByte(s[i])
					default:
						b.WriteByte('\\')
						b.WriteByte(s[i])
					}
					continue
				}
				b.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, fmt.Errorf("line %d: %s: unterminated quote", line, name)
			}
			val = b.String()
			s = s[i+1:]
		default:
			end := strings.IndexByte(s, '\n')
			if end < 0 {
				end = len(s)
			}
			val = s[:end]
			if i := strings.Index(val, " #"); i >= 0 {
				val = val[:i] // a comment after an unquoted value
			}
			val = strings.TrimSpace(val)
			s = s[end:]
		}
		line += strings.Count(val, "\n")
		out[name] = val
		// The rest of the line must be empty or a comment.
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			if rest := strings.TrimSpace(s[:i]); rest != "" && !strings.HasPrefix(rest, "#") {
				return nil, fmt.Errorf("line %d: unexpected %q after the value of %s", line, rest, name)
			}
			s = s[i:]
		} else if rest := strings.TrimSpace(s); rest != "" && !strings.HasPrefix(rest, "#") {
			return nil, fmt.Errorf("line %d: unexpected %q after the value of %s", line, rest, name)
		} else {
			s = ""
		}
	}
	return out, nil
}
