// Package redact hides secrets in log lines before they go into alerts,
// which are sent on to chat, mail and pagers.
package redact

import (
	"fmt"
	"regexp"
)

// Mask replaces what was hidden.
const Mask = "[redacted]"

// names are keys whose values are secrets. A name matches whole (db_password,
// x.api_key, refresh_token), not inside another word (tokens_used, author).
const names = `(?:[a-z0-9]+[_.-])*(?:password|passwd|pwd|passphrase|secret|token|apikey|api[_-]key|access[_-]key|secret[_-]key|` +
	`private[_-]key|client[_-]secret|credentials?|session[_-]?id|sid|sig|signature)|key`

// rule hides part of a match: the groups listed in hide.
type rule struct {
	re   *regexp.Regexp
	hide []int // groups to replace with the mask; none: the whole match
}

var builtin = []rule{
	// A private key, whole (log lines are joined into one).
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`), nil},
	// Header values that are credentials, whatever the scheme: everything up
	// to the end of the value.
	{regexp.MustCompile(`(?i)\b(authorization|proxy-authorization|cookie|set-cookie|x-api-key|api-key|x-auth-token)(\\?["']?\s*[:=]\s*\\?["']?)([^"'\\\n]+)`), []int{3}},
	// Escaped JSON inside a string: \"password\":\"x\".
	{regexp.MustCompile(`(?i)\b(` + names + `)(\\"\s*:\s*\\")(.*?)(\\")`), []int{3}},
	// name="value with spaces".
	{regexp.MustCompile(`(?i)\b(` + names + `)(["']?\s*[:=]\s*)(["'])((?:\\.|[^"'\\])*)(["'])`), []int{4}},
	// name=value (logfmt, query strings).
	{regexp.MustCompile(`(?i)\b(` + names + `)(\\?["']?\s*=\s*)([^\s"'\\&,;})\]]+)`), []int{3}},
	// "name": 12345 (JSON numbers and bare values).
	{regexp.MustCompile(`(?i)"(` + names + `)"(\s*:\s*)([^\s"',}\]]+)`), []int{3}},
	// name: value, when the value looks like a secret (has a digit, or is
	// long), so "token: expired" stays readable.
	{regexp.MustCompile(`(?i)\b(` + names + `)(\s*:\s+)([^\s"',;]*[0-9][^\s"',;]*|[^\s"',;]{16,})`), []int{3}},
	// Bearer, Basic and Token credentials anywhere.
	{regexp.MustCompile(`(?i)\b(bearer|basic|token)(\s+)([A-Za-z0-9._~+/=-]{16,})`), []int{3}},
	// Passwords in URLs (scheme://user:password@host), even with an @ in them.
	{regexp.MustCompile(`(://[^/\s:@]*:)([^/\s]+)(@)`), []int{2}},
	// Go and MySQL DSNs: user:password@tcp(host).
	{regexp.MustCompile(`\b([A-Za-z0-9_.-]+:)([^@\s/]+)(@(?:tcp|udp|unix)\()`), []int{2}},
	// Well-known token formats.
	{regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[abprs]-[A-Za-z0-9-]{10,}|` +
		`[sr]k_(?:live|test)_[A-Za-z0-9]{10,}|(?:AKIA|ASIA)[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,}|glpat-[A-Za-z0-9_-]{20,}|` +
		`npm_[A-Za-z0-9]{30,}|sk-(?:proj-)?[A-Za-z0-9_-]{20,}|eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*)`), nil},
	// Slack webhook URLs.
	{regexp.MustCompile(`hooks\.slack\.com/services/[A-Za-z0-9/]+`), nil},
}

// Redactor hides secrets.
type Redactor struct {
	rules []rule
}

// New makes a Redactor from the built-in rules (unless off) and extra
// patterns. In an extra pattern, the whole match is hidden, or only its
// first group if it has one.
func New(useBuiltin bool, extra []string) (*Redactor, error) {
	r := &Redactor{}
	if useBuiltin {
		r.rules = append(r.rules, builtin...)
	}
	for _, p := range extra {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("redact pattern %q: %v", p, err)
		}
		ru := rule{re: re}
		if re.NumSubexp() > 0 {
			ru.hide = []int{1}
		}
		r.rules = append(r.rules, ru)
	}
	return r, nil
}

// Line hides the secrets in one line.
func (r *Redactor) Line(s string) string {
	for _, ru := range r.rules {
		s = ru.apply(s)
	}
	return s
}

func (ru rule) apply(s string) string {
	matches := ru.re.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	var out []byte
	last := 0
	for _, m := range matches {
		if len(ru.hide) == 0 {
			out = append(out, s[last:m[0]]...)
			out = append(out, Mask...)
			last = m[1]
			continue
		}
		for _, g := range ru.hide {
			start, end := m[2*g], m[2*g+1]
			if start < 0 || start < last {
				continue
			}
			out = append(out, s[last:start]...)
			if s[start:end] != Mask {
				out = append(out, Mask...)
			} else {
				out = append(out, s[start:end]...)
			}
			last = end
		}
	}
	out = append(out, s[last:]...)
	return string(out)
}
