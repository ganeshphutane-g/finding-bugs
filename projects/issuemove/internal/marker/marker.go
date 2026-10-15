// Package marker reads and writes the hidden notes issuemove leaves in the
// text it posts, so a later run knows what was already done.
//
// A marker is only as trustworthy as whoever posted the text it is in:
// callers check the poster, and text copied from people has any marker-like
// note defused first, so only issuemove's own markers are ever found.
package marker

import "strings"

const prefix = "<!-- issuemove:"

// Make is the marker of a kind ("issue", "comment", "moved-to") and value.
func Make(kind, value string) string {
	return prefix + kind + "=" + value + " -->"
}

// Last is the value of the last marker of a kind in text.
func Last(text, kind string) (string, bool) {
	start := prefix + kind + "="
	i := strings.LastIndex(text, start)
	if i < 0 {
		return "", false
	}
	rest := text[i+len(start):]
	end := strings.Index(rest, " -->")
	if end < 0 || strings.ContainsAny(rest[:end], "\n<>") {
		return "", false
	}
	return rest[:end], true
}

// Defuse changes marker-like notes in someone's text so they are never
// read as markers (the text still shows the same).
func Defuse(text string) string {
	return strings.ReplaceAll(text, prefix, "<!-- issuemove (quoted):")
}
