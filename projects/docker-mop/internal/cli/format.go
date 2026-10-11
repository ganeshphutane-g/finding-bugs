package cli

import (
	"fmt"
	"math"
	"time"
)

// humanSize formats bytes the way the docker CLI does: decimal units with four
// significant digits, e.g. "209.5kB".
func humanSize(n int64) string {
	if n < 0 {
		return "unknown"
	}
	units := []string{"B", "kB", "MB", "GB", "TB", "PB"}
	size := float64(n)
	i := 0
	for size >= 1000 && i < len(units)-1 {
		size /= 1000
		i++
	}
	return fmt.Sprintf("%.4g%s", size, units[i])
}

// humanAge formats how long ago t was, e.g. "3 days ago".
func humanAge(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "less than a minute ago"
	case d < time.Hour:
		return countAgo(int(d.Minutes()), "minute")
	case d < 48*time.Hour:
		return countAgo(int(d.Hours()), "hour")
	case d < 14*24*time.Hour:
		return countAgo(int(d.Hours()/24), "day")
	case d < 60*24*time.Hour:
		return countAgo(int(d.Hours()/24/7), "week")
	case d < 2*365*24*time.Hour:
		return countAgo(int(math.Round(d.Hours()/24/30)), "month")
	default:
		return countAgo(int(d.Hours()/24/365), "year")
	}
}

func countAgo(n int, unit string) string {
	if n == 1 {
		return "1 " + unit + " ago"
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

// shortID trims "sha256:" and keeps 12 characters, like `docker ps`.
func shortID(id string) string {
	if len(id) > 7 && id[:7] == "sha256:" {
		id = id[7:]
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
