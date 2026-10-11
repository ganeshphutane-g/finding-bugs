package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/ganeshphutane-g/docker-mop/internal/volume"
)

const volumeUsage = `Usage: docker mop volume COMMAND

Commands:
  copy SOURCE TARGET     Copy a volume into a new volume and verify every file
  rename SOURCE TARGET   Copy, verify, then remove the old volume

Options:
      --compose-project NAME   Label the new volume as part of Docker Compose
                               project NAME (TARGET must be NAME_<volume>)
  -q, --quiet                  Only print errors

The new volume gets the same driver and labels as the old one. Only local
volumes without driver options can be copied. Copy refuses while a running
container uses the volume; rename refuses while any container uses it.
`

func runVolume(ctx context.Context, env Env, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(env.Stdout, volumeUsage)
		return nil
	}
	sub, rest := args[0], args[1:]
	if sub != "copy" && sub != "cp" && sub != "rename" && sub != "mv" {
		return usagef("unknown volume command %q (use copy or rename)", sub)
	}

	fs := flag.NewFlagSet("volume "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts volume.Options
	var quiet, help bool
	fs.StringVar(&opts.ComposeProject, "compose-project", "", "")
	fs.BoolVar(&quiet, "q", false, "")
	fs.BoolVar(&quiet, "quiet", false, "")
	fs.BoolVar(&help, "h", false, "")
	fs.BoolVar(&help, "help", false, "")

	// Allow flags before, between and after the two names.
	var names []string
	for {
		if err := fs.Parse(rest); err != nil {
			return usagef("volume %s: %v", sub, err)
		}
		if fs.NArg() == 0 {
			break
		}
		names = append(names, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	if help {
		fmt.Fprint(env.Stdout, volumeUsage)
		return nil
	}
	if len(names) != 2 {
		return usagef("volume %s needs SOURCE and TARGET", sub)
	}
	src, dst := names[0], names[1]
	if !quiet {
		opts.Progress = func(s string) { fmt.Fprintln(env.Stdout, s) }
	}

	client, err := connect(env)
	if err != nil {
		return err
	}
	start := time.Now()
	var rep volume.Report
	if sub == "rename" || sub == "mv" {
		rep, err = volume.Rename(ctx, client, src, dst, opts)
	} else {
		rep, err = volume.Copy(ctx, client, src, dst, opts)
	}
	if err != nil {
		return err
	}
	if !quiet {
		verb := "Copied"
		if sub == "rename" || sub == "mv" {
			verb = "Renamed"
		}
		fmt.Fprintf(env.Stdout, "%s %s to %s: %d %s, %s, verified in %s.\n",
			verb, src, dst, rep.Files, plural(rep.Files, "file"), humanSize(rep.Bytes), time.Since(start).Round(100*time.Millisecond))
	}
	return nil
}
