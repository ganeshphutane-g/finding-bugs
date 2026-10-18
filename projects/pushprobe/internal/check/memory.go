package check

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

type memoryCheck struct{ c config.Memory }

// meminfo is read from here (a variable for tests).
var meminfo = "/proc/meminfo"

func (m memoryCheck) Run(ctx context.Context) Result {
	start := time.Now()
	if runtime.GOOS != "linux" {
		return fail(start, "memory checks are not supported on %s yet", runtime.GOOS)
	}
	f, err := os.Open(meminfo)
	if err != nil {
		return fail(start, "%v", err)
	}
	defer f.Close()
	values := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		fields := strings.Fields(v)
		if len(fields) == 0 {
			continue
		}
		if n, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			values[k] = n * 1024
		}
	}
	total, avail := values["MemTotal"], values["MemAvailable"]
	if total == 0 {
		return fail(start, "%s has no MemTotal", meminfo)
	}
	if _, has := values["MemAvailable"]; !has {
		return fail(start, "%s has no MemAvailable (Linux 3.14 or newer needed)", meminfo)
	}
	percent := float64(total-avail) * 100 / float64(total)
	return usage(start, "memory", percent, m.c.MaxUsedPercent, avail, total)
}

// usage is the result for a share of something used.
func usage(start time.Time, what string, percent, max float64, free, size uint64) Result {
	desc := fmt.Sprintf("%s %.1f%% used (%s free of %s)", what, percent, humanSize(free), humanSize(size))
	if percent > max {
		return fail(start, "%s, more than %g%%", desc, max)
	}
	return ok(start, "%s", desc)
}

func humanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
