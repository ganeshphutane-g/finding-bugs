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
func ParseFilters(raw []string) (Filters, error) {
	f := Filters{}
	for _, r := range raw {
		key, value, ok := strings.Cut(r, "=")
		key = strings.TrimSpace(key)
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
// when none is set.
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

// parseTimestamp accepts the same forms as Docker's "until" filter: a Go
// duration relative to now ("24h", "90m"), an RFC 3339 date or date-time
// (local time when no zone is given), or Unix seconds with optional fraction.
func parseTimestamp(value string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(value); err == nil {
		return now.Add(-d), nil
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.999999999",
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02T15",
		"2006-01-02",
	}
	for _, layout := range layouts {
		var t time.Time
		var err error
		if strings.Contains(layout, "Z07") {
			t, err = time.Parse(layout, value)
		} else {
			t, err = time.ParseInLocation(layout, value, time.Local)
		}
		if err == nil {
			return t, nil
		}
	}

	secs, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(secs) || math.IsInf(secs, 0) || strings.ContainsAny(value, "eE") {
		return time.Time{}, fmt.Errorf("invalid until value %q: use a duration (24h), a date (2006-01-02), a date-time (2006-01-02T15:04:05) or Unix seconds", value)
	}
	whole, frac := math.Modf(secs)
	return time.Unix(int64(whole), int64(frac*1e9)), nil
}
