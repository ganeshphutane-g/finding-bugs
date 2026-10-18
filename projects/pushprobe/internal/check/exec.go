package check

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
)

type execCheck struct{ c config.Exec }

// execEnv is what the command gets from pushprobe's environment: nothing
// that could hold a token.
var execEnv = []string{"PATH", "HOME", "LANG", "TZ"}

func (e execCheck) Run(ctx context.Context) Result {
	start := time.Now()
	cmd := exec.CommandContext(ctx, e.c.Command[0], e.c.Command[1:]...)
	cmd.Env = []string{}
	for _, k := range execEnv {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out.String()), "\n", 2)[0])
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return fail(start, "timed out")
	case errors.As(err, &exit):
		if first == "" {
			return fail(start, "exit code %d", exit.ExitCode())
		}
		return fail(start, "exit code %d: %s", exit.ExitCode(), first)
	case err != nil:
		return fail(start, "%v", err)
	}
	if first == "" {
		first = "exit code 0"
	}
	return ok(start, "%s", first)
}
