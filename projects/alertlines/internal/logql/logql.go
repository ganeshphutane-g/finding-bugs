// Package logql finds the log query inside a Loki metric expression: the
// part that selects the lines an alert counted.
//
// It is a small scanner, not a LogQL parser: it knows strings (so a "[" or
// "(" inside quotes is text), parentheses, and the names of the functions
// that take a log range. That is enough to cut
//
//	sum by (app) (count_over_time({app="shop"} |= "ERROR" [5m])) > 0
//
// into the log query {app="shop"} |= "ERROR", the range 5m and the
// grouping labels [app].
package logql

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Query is what an expression counts.
type Query struct {
	// Log is the log query: a stream selector and its pipeline.
	Log string
	// Range is how far back the expression looks.
	Range time.Duration
	// Offset moves the range back in time (count_over_time(...[5m] offset 1h)).
	Offset time.Duration
	// By are the labels the expression groups by (from every "by (...)"),
	// less those label_replace makes.
	By []string
	// Without are the labels "without (...)" drops.
	Without []string
	// Grouped is set when the expression aggregates at all (sum, count, ...),
	// so the alert's labels other than By are not stream labels.
	Grouped bool
}

// rangeFuncs take a log range ({...} | ... [5m]).
var rangeFuncs = map[string]bool{
	"count_over_time": true, "rate": true, "bytes_over_time": true, "bytes_rate": true, "absent_over_time": true,
	"sum_over_time": true, "avg_over_time": true, "max_over_time": true, "min_over_time": true,
	"first_over_time": true, "last_over_time": true, "stdvar_over_time": true, "stddev_over_time": true,
	"quantile_over_time": true, "rate_counter": true,
}

// aggregations group series.
var aggregations = map[string]bool{
	"sum": true, "avg": true, "min": true, "max": true, "count": true, "stddev": true, "stdvar": true,
	"topk": true, "bottomk": true, "sort": true, "sort_desc": true, "approx_topk": true,
}

// ErrNoLogQuery means the expression has no log range in it.
var ErrNoLogQuery = errors.New("the expression has no log range (such as count_over_time({...}[5m]))")

type token struct {
	kind string // "ident", "str", "(", ")", "[", "]", "{", "}", ",", "other"
	text string
	pos  int // byte offset in the expression
	end  int
}

// tokens splits an expression, keeping strings whole.
func tokens(s string) ([]token, error) {
	var out []token
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '"':
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' {
					j++
				}
			}
			if j >= len(s) {
				return nil, fmt.Errorf("a string starting at %d is not closed", i)
			}
			out = append(out, token{"str", s[i : j+1], i, j + 1})
			i = j + 1
		case c == '`':
			j := strings.IndexByte(s[i+1:], '`')
			if j < 0 {
				return nil, fmt.Errorf("a string starting at %d is not closed", i)
			}
			out = append(out, token{"str", s[i : i+j+2], i, i + j + 2})
			i += j + 2
		case strings.IndexByte("()[]{},", c) >= 0:
			out = append(out, token{string(c), string(c), i, i + 1})
			i++
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || s[j] >= '0' && s[j] <= '9') {
				j++
			}
			out = append(out, token{"ident", s[i:j], i, j})
			i = j
		default:
			out = append(out, token{"other", string(c), i, i + 1})
			i++
		}
	}
	return out, nil
}

// Extract finds the first log range in a metric expression.
func Extract(expr string) (*Query, error) {
	toks, err := tokens(expr)
	if err != nil {
		return nil, err
	}
	q := &Query{}
	by, without, made := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i, t := range toks {
		if t.kind != "ident" {
			continue
		}
		if aggregations[t.text] {
			q.Grouped = true
		}
		if (t.text == "by" || t.text == "without") && i+1 < len(toks) && toks[i+1].kind == "(" {
			set := by
			if t.text == "without" {
				set = without
			}
			for j := i + 2; j < len(toks) && toks[j].kind != ")"; j++ {
				if toks[j].kind == "ident" {
					set[toks[j].text] = true
				}
			}
		}
		// label_replace(v, "dst", ...) makes dst, which no log line has.
		if t.text == "label_replace" && i+1 < len(toks) && toks[i+1].kind == "(" {
			depth := 0
			for j := i + 1; j < len(toks); j++ {
				switch toks[j].kind {
				case "(":
					depth++
				case ")":
					depth--
				case ",":
					if depth == 1 && j+1 < len(toks) && toks[j+1].kind == "str" {
						made[strings.Trim(toks[j+1].text, "\"`")] = true
						j = len(toks)
					}
				}
				if depth == 0 {
					break
				}
			}
		}
	}
	for k := range by {
		if !made[k] {
			q.By = append(q.By, k)
		}
	}
	for k := range without {
		q.Without = append(q.Without, k)
	}
	sort.Strings(q.By)
	sort.Strings(q.Without)

	for i, t := range toks {
		if t.kind != "ident" || !rangeFuncs[t.text] || i+1 >= len(toks) || toks[i+1].kind != "(" {
			continue
		}
		// The log query runs from the first "{" inside the call to the "["
		// of its range, at the call's own depth.
		depth, start := 0, -1
		for j := i + 1; j < len(toks); j++ {
			switch toks[j].kind {
			case "(":
				if start < 0 && depth == 1 {
					start = toks[j].pos // a log query in parentheses
				}
				depth++
			case ")":
				depth--
				if depth == 0 {
					j = len(toks) // the call ended without a range
				}
			case "{":
				if start < 0 && depth == 1 {
					start = toks[j].pos
				}
			case "[":
				if depth != 1 || start < 0 || j+2 >= len(toks) {
					continue
				}
				closeAt := -1
				for k := j + 1; k < len(toks); k++ {
					if toks[k].kind == "]" {
						closeAt = k
						break
					}
				}
				if closeAt < 0 {
					return nil, errors.New("a range is not closed with ]")
				}
				rangeText := strings.TrimSpace(expr[toks[j].end:toks[closeAt].pos])
				d, err := parseDuration(rangeText)
				if err != nil {
					return nil, fmt.Errorf("range [%s]: %v", rangeText, err)
				}
				q.Log = stripUnwrap(unwrapParens(strings.TrimSpace(expr[start:toks[j].pos])))
				if !strings.HasPrefix(q.Log, "{") {
					return nil, ErrNoLogQuery
				}
				q.Range = d
				// [5m] offset 1h (Loki writes 1h0m0s).
				if closeAt+1 < len(toks) && toks[closeAt+1].kind == "ident" && toks[closeAt+1].text == "offset" {
					rest := strings.TrimSpace(expr[toks[closeAt+1].end:])
					end := 0
					for end < len(rest) && (rest[end] >= '0' && rest[end] <= '9' || rest[end] >= 'a' && rest[end] <= 'z') {
						end++
					}
					if o, err := parseDuration(rest[:end]); err == nil {
						q.Offset = o
					}
				}
				return q, nil
			}
		}
	}
	return nil, ErrNoLogQuery
}

// unwrapParens removes parentheses around the whole of s.
func unwrapParens(s string) string {
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") {
		toks, err := tokens(s)
		if err != nil {
			return s
		}
		depth := 0
		for i, t := range toks {
			switch t.kind {
			case "(":
				depth++
			case ")":
				depth--
				if depth == 0 && i != len(toks)-1 {
					return s // the first ( closes before the end
				}
			}
		}
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// stripUnwrap removes "| unwrap ..." (and what follows it up to the next
// stage), which only metric queries allow.
func stripUnwrap(s string) string {
	toks, err := tokens(s)
	if err != nil {
		return s
	}
	for i, t := range toks {
		if t.kind == "ident" && t.text == "unwrap" && i > 0 && toks[i-1].text == "|" {
			cut := toks[i-1].pos
			// Keep any stages after the unwrapped label (| __error__="").
			rest := ""
			for j := i + 1; j < len(toks); j++ {
				if toks[j].text == "|" {
					rest = " " + s[toks[j].pos:]
					break
				}
			}
			return strings.TrimSpace(s[:cut]) + rest
		}
	}
	return s
}

// parseDuration reads Prometheus durations: 5m, 1h30m, 2d, 1w.
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	var total time.Duration
	units := map[string]time.Duration{"ms": time.Millisecond, "s": time.Second, "m": time.Minute, "h": time.Hour,
		"d": 24 * time.Hour, "w": 7 * 24 * time.Hour, "y": 365 * 24 * time.Hour}
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("%q is not a duration", s)
		}
		n := 0
		fmt.Sscan(s[:i], &n)
		s = s[i:]
		j := 0
		for j < len(s) && s[j] >= 'a' && s[j] <= 'z' {
			j++
		}
		u, ok := units[s[:j]]
		if !ok {
			return 0, fmt.Errorf("unknown unit %q", s[:j])
		}
		total += time.Duration(n) * u
		s = s[j:]
	}
	return total, nil
}

// WithLabels adds label filters to a log query: | name="value" for each.
// Names that are not plain label names are skipped.
func WithLabels(log string, labels map[string]string) string {
	var names []string
	for k := range labels {
		if validName(k) {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(log)
	for _, k := range names {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(labels[k])
		fmt.Fprintf(&b, ` | %s="%s"`, k, v)
	}
	return b.String()
}

// keywords cannot be label names in a label filter (Loki rejects them).
var keywords = map[string]bool{"on": true, "by": true, "or": true, "and": true, "unless": true, "bool": true, "offset": true,
	"without": true, "ignoring": true, "group_left": true, "group_right": true, "unwrap": true, "json": true, "logfmt": true,
	"pattern": true, "regexp": true, "unpack": true, "keep": true, "drop": true, "label_format": true, "line_format": true,
	"decolorize": true}

func validName(s string) bool {
	if s == "" || strings.HasPrefix(s, "__") || keywords[s] {
		return false
	}
	for i, c := range s {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
