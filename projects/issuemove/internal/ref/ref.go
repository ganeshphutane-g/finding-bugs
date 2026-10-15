// Package ref parses issue references such as owner/repo#12.
package ref

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Source is a repository and, optionally, chosen issue numbers.
type Source struct {
	Owner, Repo string
	Numbers     []int64 // empty: chosen by filters
}

// Repo is owner/repo.
type Repo struct{ Owner, Name string }

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// ParseRepo parses owner/repo.
func ParseRepo(s string) (Repo, error) {
	owner, name, ok := strings.Cut(s, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(name, "/#") || strings.ContainsAny(owner, "#") {
		return Repo{}, fmt.Errorf("%q is not owner/repo", s)
	}
	return Repo{owner, name}, nil
}

// ParseSource parses owner/repo, owner/repo#12, owner/repo#12,15 or
// owner/repo#12-20 (and mixes such as #3,7-9).
func ParseSource(s string) (Source, error) {
	repo, nums, hasNums := strings.Cut(s, "#")
	r, err := ParseRepo(repo)
	if err != nil {
		return Source{}, err
	}
	src := Source{Owner: r.Owner, Repo: r.Name}
	if !hasNums {
		return src, nil
	}
	seen := map[int64]bool{}
	for _, part := range strings.Split(nums, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		a, err := strconv.ParseInt(lo, 10, 64)
		if err != nil || a < 1 {
			return Source{}, fmt.Errorf("%q: %q is not an issue number", s, part)
		}
		b := a
		if isRange {
			if b, err = strconv.ParseInt(hi, 10, 64); err != nil || b < a {
				return Source{}, fmt.Errorf("%q: %q is not a range", s, part)
			}
			if b-a > 10000 {
				return Source{}, fmt.Errorf("%q: range too large", s)
			}
		}
		for n := a; n <= b; n++ {
			if !seen[n] {
				seen[n] = true
				src.Numbers = append(src.Numbers, n)
			}
		}
	}
	sort.Slice(src.Numbers, func(i, j int) bool { return src.Numbers[i] < src.Numbers[j] })
	return src, nil
}
