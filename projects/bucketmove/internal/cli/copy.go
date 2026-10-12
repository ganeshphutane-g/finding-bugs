package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ganeshphutane-g/bucketmove/internal/copier"
	"github.com/ganeshphutane-g/bucketmove/internal/inventory"
	"github.com/ganeshphutane-g/bucketmove/internal/probe"
	"github.com/ganeshphutane-g/bucketmove/internal/verify"
)

const copyUsage = `Usage: bucketmove copy --from SOURCE --to TARGET [OPTIONS]

Copy buckets with every object version, oldest first, plus delete markers,
headers, metadata, tags, retention, legal holds and the bucket settings the
target supports. Runs 'check' first and refuses if anything would be lost,
unless --accept-loss is given. Safe to run again: a journal in the state
directory records what is done, and the next run continues from there.

Options:
      --from ENDPOINT    The server to copy from
      --to ENDPOINT      The server to copy to
      --bucket NAME      Only these buckets (repeatable, or comma-separated)
      --accept-loss      Copy even though check found something the target cannot keep
      --into-existing    Allow copying into target buckets that already exist
      --workers N        Keys copied at the same time (default 4)
      --state-dir DIR    Where journals are kept (default ./.bucketmove/<source>-<target>)
      --verify           Run 'verify' on each bucket after copying it
`

func runCopy(ctx context.Context, env Env, args []string) error {
	fs := flagSet("copy")
	from := fs.String("from", "", "")
	to := fs.String("to", "", "")
	acceptLoss := fs.Bool("accept-loss", false, "")
	intoExisting := fs.Bool("into-existing", false, "")
	workers := fs.Int("workers", 4, "")
	stateDir := fs.String("state-dir", "", "")
	doVerify := fs.Bool("verify", false, "")
	var buckets stringList
	fs.Var(&buckets, "bucket", "")
	if help, err := parseFlags(fs, args, copyUsage, env.Stdout); help || err != nil {
		return err
	}
	if *workers < 1 || *workers > 64 {
		return usagef("copy: --workers must be between 1 and 64")
	}
	srcEP, src, err := connect(env, *from, "from")
	if err != nil {
		return err
	}
	dstEP, dst, err := connect(env, *to, "to")
	if err != nil {
		return err
	}
	if sameServer(srcEP.URL, dstEP.URL) {
		return usagef("copy: source and target are the same server (%s and %s); bucketmove copies buckets under the same names", srcEP.URL.Host, dstEP.URL.Host)
	}

	report, err := buildReport(ctx, env, srcEP, src, dstEP, dst, buckets, true)
	if err != nil {
		return err
	}
	if report.HasLoss() {
		writeReport(env.Stdout, dstEP.Name, report)
		if !*acceptLoss {
			return fmt.Errorf("stopped: the target cannot keep everything (see above); pass --accept-loss to copy anyway")
		}
		fmt.Fprintln(env.Stdout, "\nContinuing because of --accept-loss.")
	}

	dir := *stateDir
	if dir == "" {
		dir = filepath.Join(".bucketmove", stateName(srcEP.URL.Host, dstEP.URL.Host))
	}
	inv, err := inventory.Take(ctx, src, inventory.Options{Buckets: buckets})
	if err != nil {
		return fmt.Errorf("%s: %w", srcEP.Name, err)
	}

	var mu sync.Mutex
	last := time.Time{}
	cp := copier.Copier{Src: src, Dst: dst, Caps: report.Target, Opts: copier.Options{
		StateDir:     dir,
		Workers:      *workers,
		IntoExisting: *intoExisting,
		Warn:         func(s string) { fmt.Fprintln(env.Stderr, "Warning: "+s) },
		Progress: func(bucket string, done, bytes int64) {
			if !isTerminal(env.Stderr) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if time.Since(last) > 500*time.Millisecond {
				last = time.Now()
				fmt.Fprintf(env.Stderr, "\r%s: %s versions, %s copied...\033[K", bucket, count(done), humanSize(bytes))
			}
		},
	}}

	fmt.Fprintf(env.Stdout, "Copying %d %s from %s to %s (journal: %s)\n", len(inv), plural(len(inv), "bucket"), srcEP.Name, dstEP.Name, dir)
	failed := 0
	for _, b := range inv {
		start := time.Now()
		st, err := cp.Bucket(ctx, b)
		clearLine(env)
		if err != nil {
			failed++
			fmt.Fprintf(env.Stdout, "%s: FAILED after %s versions: %v\n", b.Name, count(st.Versions+st.Markers), err)
			if ctx.Err() != nil {
				return fmt.Errorf("interrupted; run the same command again to continue")
			}
			continue
		}
		line := fmt.Sprintf("%s: %s %s and %s delete %s copied (%s), %s already done",
			b.Name, count(st.Versions), plural(int(st.Versions), "version"), count(st.Markers), plural(int(st.Markers), "marker"),
			humanSize(st.Bytes), count(st.Skipped))
		if len(st.Settings) > 0 {
			line += "; settings: " + strings.Join(st.Settings, ", ")
		}
		fmt.Fprintf(env.Stdout, "%s, in %s.\n", line, time.Since(start).Round(100*time.Millisecond))
		if *doVerify {
			res, err := verify.Bucket(ctx, src, dst, b.Name, verifyOptionsFor(report.Target, b))
			switch {
			case err != nil:
				failed++
				fmt.Fprintf(env.Stdout, "  verify FAILED: %v\n", err)
			case !res.OK():
				failed++
				writeDiffs(env, res)
			default:
				fmt.Fprintf(env.Stdout, "  verified: %s keys, %s versions identical.\n", count(res.Keys), count(res.Versions))
			}
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d %s did not finish; fix the problem and run the same command again to continue", failed, len(inv), plural(len(inv), "bucket"))
	}
	return nil
}

// sameServer reports whether two endpoints reach the same host and port,
// looking through names such as localhost and 127.0.0.1.
func sameServer(a, b *url.URL) bool {
	if strings.EqualFold(a.Host, b.Host) {
		return true
	}
	if port(a) != port(b) {
		return false
	}
	addrs := func(host string) map[string]bool {
		out := map[string]bool{}
		ips, err := net.LookupHost(host)
		if err != nil {
			out[host] = true
			return out
		}
		for _, ip := range ips {
			if parsed := net.ParseIP(ip); parsed != nil && parsed.IsLoopback() {
				ip = "loopback"
			}
			out[ip] = true
		}
		return out
	}
	x, y := addrs(a.Hostname()), addrs(b.Hostname())
	for ip := range x {
		if y[ip] {
			return true
		}
	}
	return false
}

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// stateName is a readable, file-safe name for a source/target pair.
func stateName(src, dst string) string {
	clean := func(s string) string {
		return strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
				return r
			}
			return '_'
		}, s)
	}
	sum := sha256.Sum256([]byte(src + "\x00" + dst))
	return clean(src) + "-to-" + clean(dst) + "-" + hex.EncodeToString(sum[:4])
}

// verifyOptionsFor compares what the target can keep for bucket b.
func verifyOptionsFor(caps probe.Capabilities, b inventory.Bucket) verify.Options {
	return verify.Options{
		LatestOnly:   b.Versioning == "" || !caps.Supports(probe.Versioning) || !caps.Supports(probe.DeleteMarker),
		SkipTags:     !caps.Supports(probe.ObjectTags),
		GuessedType:  !caps.Supports(probe.ExactType),
		Locks:        b.ObjectLock && caps.Supports(probe.ObjectLock) && caps.Supports(probe.Retention) && caps.Supports(probe.LegalHold),
		StorageClass: caps.Supports(probe.StorageClass),
	}
}
