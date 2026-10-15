package move

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// shortRef is "#12" not preceded by a word character, a slash or another
// "#" (so owner/repo#12, a#1 and ##1 stay as they are).
var shortRef = regexp.MustCompile(`(^|[^\w/#&])#(\d+)\b`)

// rewriteRefs points same-repository references (#12) of a moved text at the
// right place: the new number when that issue moved too, otherwise the
// source repository. Code (fenced blocks and inline spans) is left alone.
func rewriteRefs(text string, moved map[int64]int64, sourceRef func(n int64) string) string {
	var out strings.Builder
	lines := strings.SplitAfter(text, "\n")
	inFence := ""
	for _, line := range lines {
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
		// (Indented code blocks are not skipped: in issues, four-space
		// indentation is far more often a nested list item.)
		if inFence != "" {
			out.WriteString(line)
			continue
		}
		out.WriteString(rewriteOutsideCode(line, moved, sourceRef))
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

// rewriteOutsideCode rewrites a line, skipping `inline code` spans.
func rewriteOutsideCode(line string, moved map[int64]int64, sourceRef func(n int64) string) string {
	var out strings.Builder
	for len(line) > 0 {
		i := strings.IndexByte(line, '`')
		if i < 0 {
			out.WriteString(rewriteText(line, moved, sourceRef))
			break
		}
		out.WriteString(rewriteText(line[:i], moved, sourceRef))
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

func rewriteText(s string, moved map[int64]int64, sourceRef func(n int64) string) string {
	return shortRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := shortRef.FindStringSubmatch(m)
		n, err := strconv.ParseInt(sub[2], 10, 64)
		if err != nil {
			return m
		}
		if to, ok := moved[n]; ok {
			return sub[1] + "#" + strconv.FormatInt(to, 10)
		}
		return sub[1] + sourceRef(n)
	})
}

// replaceUUIDs points attachment links at the new copies.
func replaceUUIDs(text string, uuids map[string]string) string {
	for old, nw := range uuids {
		text = strings.ReplaceAll(text, old, nw)
	}
	return text
}

// marker is a hidden note in moved text naming its source, so a second run
// finds what was already copied.
func marker(kind, key string) string {
	return fmt.Sprintf("<!-- issuemove:%s=%s -->", kind, key)
}

// findMarker reports whether text carries the marker.
func hasMarker(text, kind, key string) bool {
	return strings.Contains(text, marker(kind, key))
}
