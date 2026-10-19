// Package redact hides secrets in log lines before they go into alerts,
// which are sent on to chat, mail and pagers.
package redact

import (
	"fmt"
	"regexp"
	"strings"
)

// Mask replaces what was hidden.
const Mask = "[redacted]"

// builtin catch the common shapes of secrets in logs. Each keeps what names
// the secret and hides the value.
var builtin = []*regexp.Regexp{
	// key=value and key: value, for names that hold secrets.
	regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|credential|session[_-]?id|cookie|auth)[a-z0-9_.-]*)(["']?\s*[:=]\s*["']?)([^\s"'&,;})\]]+)`),
	// Authorization: Bearer xyz, Basic xyz.
	regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)([A-Za-z0-9._~+/=-]{8,})`),
	// Passwords in URLs: scheme://user:password@host.
	regexp.MustCompile(`(://[^/\s:@]*:)([^/\s@]+)(@)`),
	// Well-known token formats (GitHub, Slack, Stripe, AWS, JWT).
	regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|[sr]k_(?:live|test)_[A-Za-z0-9]{10,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`),
}

// Redactor hides secrets.
type Redactor struct {
	patterns []*regexp.Regexp
}

// New makes a Redactor from the built-in patterns (unless off) and extra
// ones. In an extra pattern, the whole match is hidden, or only its first
// group if it has one.
func New(useBuiltin bool, extra []string) (*Redactor, error) {
	r := &Redactor{}
	if useBuiltin {
		r.patterns = append(r.patterns, builtin...)
	}
	for _, p := range extra {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redact pattern %q: %v", p, err)
		}
		r.patterns = append(r.patterns, re)
	}
	return r, nil
}

// Line hides the secrets in one line.
func (r *Redactor) Line(s string) string {
	for i, re := range r.patterns {
		if i < len(builtin) && re == builtin[i] {
			s = r.builtinReplace(i, re, s)
			continue
		}
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatchIndex(m)
			if len(sub) >= 4 && sub[2] >= 0 {
				return m[:sub[2]] + Mask + m[sub[3]:]
			}
			return Mask
		})
	}
	return s
}

func (r *Redactor) builtinReplace(i int, re *regexp.Regexp, s string) string {
	switch i {
	case 0:
		// "Authorization: Bearer x" is left to the next pattern, which
		// keeps the scheme.
		return re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatch(m)
			if v := strings.ToLower(sub[3]); v == "bearer" || v == "basic" || sub[3] == Mask {
				return m
			}
			return sub[1] + sub[2] + Mask
		})
	case 1:
		return re.ReplaceAllString(s, "${1}${2}"+Mask)
	case 2:
		return re.ReplaceAllString(s, "${1}"+Mask+"${3}")
	}
	return re.ReplaceAllString(s, Mask)
}
