package plan

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Filters holds --filter values keyed by filter name, mirroring Docker's
// filters.Args. Values for one key are a set.
type Filters map[string]map[string]bool

// ParseFilters turns "key=value" strings into Filters, splitting at the first
// "=", the way the docker CLI does ("label!=a=b" is key "label!", value "a=b").
// Like the CLI it lowercases the key and trims spaces around both parts.
func ParseFilters(raw []string) (Filters, error) {
	f := Filters{}
	for _, r := range raw {
		key, value, ok := strings.Cut(r, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if !ok || key == "" {
			return nil, fmt.Errorf("bad format of filter (expected name=value): %q", r)
		}
		f.Add(key, value)
	}
	return f, nil
}

// Add adds one value for key.
func (f Filters) Add(key, value string) {
	if f[key] == nil {
		f[key] = map[string]bool{}
	}
	f[key][value] = true
}

// Contains reports whether any value is set for key.
func (f Filters) Contains(key string) bool {
	return len(f[key]) > 0
}

// Get returns the values for key, sorted.
func (f Filters) Get(key string) []string {
	out := make([]string, 0, len(f[key]))
	for v := range f[key] {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Validate rejects keys that are not in accepted, with Docker's wording.
func (f Filters) Validate(accepted ...string) error {
	ok := map[string]bool{}
	for _, a := range accepted {
		ok[a] = true
	}
	for _, k := range sortedKeys(f) {
		if !ok[k] {
			return fmt.Errorf("invalid filter '%s'", k)
		}
	}
	return nil
}

func sortedKeys(f Filters) []string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// matchKVList is Docker's filters.Args.MatchKVList: true when every value for
// key ("k" or "k=v") is present in labels. No values means true; values with
// no labels means false.
func (f Filters) matchKVList(key string, labels map[string]string) bool {
	values := f[key]
	if len(values) == 0 {
		return true
	}
	if len(labels) == 0 {
		return false
	}
	for value := range values {
		k, v, hasValue := strings.Cut(value, "=")
		got, ok := labels[k]
		if !ok {
			return false
		}
		if hasValue && got != v {
			return false
		}
	}
	return true
}

// MatchLabels is the label check Docker's prune code uses: all "label"
// filters must match, and the object is excluded only when all "label!"
// filters match together.
func (f Filters) MatchLabels(labels map[string]string) bool {
	if !f.matchKVList("label", labels) {
		return false
	}
	if f.Contains("label!") && f.matchKVList("label!", labels) {
		return false
	}
	return true
}

// Until returns the cut-off time from the "until" filter, or the zero time
// when none is set. now should be the daemon's clock, in the daemon's time
// zone, because Docker evaluates the filter there.
func (f Filters) Until(now time.Time) (time.Time, error) {
	values := f.Get("until")
	switch len(values) {
	case 0:
		return time.Time{}, nil
	case 1:
		return parseTimestamp(values[0], now)
	default:
		return time.Time{}, fmt.Errorf("more than one until filter specified")
	}
}

// parseTimestamp follows Docker's timetypes.GetTimestamp: a Go duration
// relative to now ("24h", "90m"; but "0" means the Unix epoch), an RFC 3339
// date or date-time with or without a zone (no zone means now's time zone),
// or Unix seconds with an optional fraction.
func parseTimestamp(value string, now time.Time) (time.Time, error) {
	if value != "0" {
		if d, err := time.ParseDuration(value); err == nil {
			return now.Add(-d), nil
		}
	}

	bases := []string{
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02T15",
		"2006-01-02",
	}
	for _, base := range bases {
		if t, err := time.Parse(base+"Z07:00", value); err == nil {
			return t, nil
		}
		if t, err := time.ParseInLocation(base, value, now.Location()); err == nil {
			return t, nil
		}
	}

	if !unixSeconds(value) {
		return time.Time{}, fmt.Errorf("invalid until value %q: use a duration (24h), a date (2006-01-02), a date-time (2006-01-02T15:04:05) or Unix seconds", value)
	}
	secs, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsInf(secs, 0) {
		return time.Time{}, fmt.Errorf("invalid until value %q", value)
	}
	whole, frac := math.Modf(secs)
	return time.Unix(int64(whole), int64(math.Round(frac*1e9))), nil
}

// unixSeconds accepts digits with at most one decimal point and an optional
// leading minus, the only number forms Docker takes.
func unixSeconds(s string) bool {
	s = strings.TrimPrefix(s, "-")
	if s == "" || s == "." {
		return false
	}
	dot := false
	for _, r := range s {
		switch {
		case r == '.' && !dot:
			dot = true
		case r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
