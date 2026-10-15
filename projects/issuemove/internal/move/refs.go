package move

import (
	"regexp"
	"strconv"
	"strings"
)

// shortRef is "#12" not preceded by a word character, a slash or another
// "#" (so owner/repo#12, a#1 and ##1 stay as they are).
var shortRef = regexp.MustCompile(`(^|[^\w/#&])#(\d+)\b`)

// mention is "@name" as Gitea reads it: after a space, a bracket, an
// emphasis mark or the start, not inside an email address.
var mention = regexp.MustCompile(`(^|[\s(\[>"'*_~])@([0-9A-Za-z_-][0-9A-Za-z_.-]*)`)

// outsideCode applies f to the parts of text that are not code: fenced
// blocks and inline spans are left alone. (Indented code blocks are not
// skipped: in issues, four-space indentation is far more often a nested
// list item.)
func outsideCode(text string, f func(string) string) string {
	var out strings.Builder
	inFence := ""
	for _, line := range strings.SplitAfter(text, "\n") {
		trim := strings.TrimLeft(line, " ")
		if fence := fenceOf(trim); fence != "" {
			switch {
			case inFence == "":
				inFence = fence
			case strings.HasPrefix(trim, inFence):
				inFence = ""
			}
			out.WriteString(line)
			continue
		}
		if inFence != "" {
			out.WriteString(line)
			continue
		}
		out.WriteString(outsideInline(line, f))
	}
	return out.String()
}

func fenceOf(trim string) string {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(trim, f) {
			n := len(trim) - len(strings.TrimLeft(trim, f[:1]))
			return strings.Repeat(f[:1], n)
		}
	}
	return ""
}

// outsideInline applies f to a line, skipping `inline code` spans.
func outsideInline(line string, f func(string) string) string {
	var out strings.Builder
	for len(line) > 0 {
		i := strings.IndexByte(line, '`')
		if i < 0 {
			out.WriteString(f(line))
			break
		}
		out.WriteString(f(line[:i]))
		// Find the matching run of backticks.
		n := len(line[i:]) - len(strings.TrimLeft(line[i:], "`"))
		ticks := line[i : i+n]
		rest := line[i+n:]
		j := strings.Index(rest, ticks)
		if j < 0 {
			out.WriteString(line[i:])
			break
		}
		out.WriteString(line[i : i+n+j+n])
		line = rest[j+n:]
	}
	return out.String()
}

// rewriteRefs points same-repository references (#12) at the right place:
// the new number when that issue moved too, otherwise the source
// repository. Links to a heading on the page, [text](#12), are kept.
func rewriteRefs(s string, moved map[int64]int64, sourceRef func(n int64) string) string {
	var out strings.Builder
	last := 0
	for _, m := range shortRef.FindAllStringSubmatchIndex(s, -1) {
		pre, num := s[m[2]:m[3]], s[m[4]:m[5]]
		if pre == "(" && m[2] > 0 && s[m[2]-1] == ']' {
			continue
		}
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			continue
		}
		out.WriteString(s[last:m[0]])
		out.WriteString(pre)
		if to, ok := moved[n]; ok {
			out.WriteString("#" + strconv.FormatInt(to, 10))
		} else {
			out.WriteString(sourceRef(n))
		}
		last = m[1]
	}
	out.WriteString(s[last:])
	return out.String()
}

// quietMentions keeps @name from notifying anyone, for names keep says no
// to: on another instance the same name can be someone else. A zero-width
// space after the @ keeps the text looking the same.
func quietMentions(s string, keep func(login string) bool) string {
	return mention.ReplaceAllStringFunc(s, func(m string) string {
		sub := mention.FindStringSubmatch(m)
		if keep(sub[2]) {
			return m
		}
		return sub[1] + "@​" + sub[2]
	})
}

// replaceUUIDs points attachment links at the new copies.
func replaceUUIDs(text string, uuids map[string]string) string {
	for old, nw := range uuids {
		text = strings.ReplaceAll(text, old, nw)
	}
	return text
}
