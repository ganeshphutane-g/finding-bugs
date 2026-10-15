package move

import (
	"fmt"
	"testing"
)

func TestRewriteRefs(t *testing.T) {
	moved := map[int64]int64{3: 40}
	src := func(n int64) string { return fmt.Sprintf("me/old#%d", n) }
	refs := func(s string) string { return rewriteRefs(s, moved, src) }
	for in, want := range map[string]string{
		"see #3 and #5":                 "see #40 and me/old#5",
		"(#3)":                          "(#40)",
		"other/repo#3 stays":            "other/repo#3 stays",
		"a#3 and ##3 and &#39;":         "a#3 and ##3 and &#39;",
		"#3 at start":                   "#40 at start",
		"`#3` in code, #3 outside":      "`#3` in code, #40 outside",
		"```\n#3 fenced\n```\n#5 after": "```\n#3 fenced\n```\nme/old#5 after",
		"  - item\n    - nested #3":     "  - item\n    - nested #40",
		"~~~go\nx #5\n~~~":              "~~~go\nx #5\n~~~",
		"``a ` #3`` and #3":             "``a ` #3`` and #40",
		"https://host/me/old/issues#3":  "https://host/me/old/issues#3",
		"heading\n## Steps\n#3":         "heading\n## Steps\n#40",
		"[step 3](#3) and (#3)":         "[step 3](#3) and (#40)",
	} {
		if got := outsideCode(in, refs); got != want {
			t.Errorf("rewriteRefs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuietMentions(t *testing.T) {
	keep := func(l string) bool { return l == "alice" }
	for in, want := range map[string]string{
		"thanks @bob and @alice": "thanks @​bob and @alice",
		"mail bob@example.com":   "mail bob@example.com",
		"(@bob) `@bob`":          "(@​bob) `@bob`",
		"@bob: at the start":     "@​bob: at the start",
		"in **@bob** bold":       "in **@\u200bbob** bold",
	} {
		if got := outsideCode(in, func(s string) string { return quietMentions(s, keep) }); got != want {
			t.Errorf("quietMentions(%q) = %q, want %q", in, got, want)
		}
	}
}
