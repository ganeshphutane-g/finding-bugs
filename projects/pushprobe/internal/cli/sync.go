package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/ganeshphutane-g/pushprobe/internal/config"
	"github.com/ganeshphutane-g/pushprobe/internal/deliver"
	"github.com/ganeshphutane-g/pushprobe/internal/kuma"
	"github.com/ganeshphutane-g/pushprobe/internal/kumasync"
	"github.com/ganeshphutane-g/pushprobe/internal/runner"
)

// syncKuma is `pushprobe sync`.
func syncKuma(ctx context.Context, env Env, file, envFile string, dryRun bool) error {
	data, err := env.ReadFile(file)
	if err != nil {
		return err
	}
	c, err := config.Load(data, env.LookupEnv, config.ForSync)
	if err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	get := func(k string) string { v, _ := env.LookupEnv(k); return v }
	cr := kuma.Credentials{Username: get("PUSHPROBE_KUMA_USERNAME"), Password: get("PUSHPROBE_KUMA_PASSWORD"),
		TOTP: get("PUSHPROBE_KUMA_TOTP"), TOTPSecret: get("PUSHPROBE_KUMA_TOTP_SECRET"), Token: get("PUSHPROBE_KUMA_TOKEN")}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client, err := kuma.ConnectWith(cctx, c.Targets.Kuma.URL, deliver.Client(c.Targets.Kuma.Insecure))
	if err != nil {
		return err
	}
	defer client.Close()
	if _, err := client.Login(cctx, cr); err != nil {
		return err
	}
	st, err := client.Read(cctx)
	if err != nil {
		return err
	}
	plan, err := kumasync.MakePlan(c, st.Monitors, runner.RetryPause, kumasync.NewToken)
	if err != nil {
		return err
	}
	changes := 0
	for _, a := range plan.Actions {
		if a.Op != "ok" {
			changes++
		}
	}
	// Tokens to save: new ones, and ones read from the server for a ${VAR}
	// that has no value yet.
	save := map[string]string{}
	for _, a := range plan.Actions {
		if _, set := env.LookupEnv(a.Var); a.Var != "" && !set && (a.NewToken || a.Op != "create") {
			save[a.Var] = a.Token
		}
	}
	fmt.Fprintf(env.Stdout, "%s (Uptime Kuma %s), group %q:\n", c.Targets.Kuma.URL, st.Info.Version, plan.Group)
	for _, l := range kumasync.Describe(plan, func(a kumasync.Action) bool { _, ok := save[a.Var]; return ok }) {
		fmt.Fprintf(env.Stdout, "  %s\n", l)
	}
	if dryRun {
		fmt.Fprintf(env.Stdout, "%s to change", plural(changes, "monitor"))
		if len(save) > 0 {
			fmt.Fprintf(env.Stdout, " and %s to save in %s", plural(len(save), "token"), envFile)
		}
		fmt.Fprintln(env.Stdout, ". Nothing was changed (--dry-run).")
		return nil
	}
	// Tokens taken from monitors that are already there can be saved now.
	early := map[string]string{}
	for _, a := range plan.Actions {
		if v, ok := save[a.Var]; ok && !a.NewToken {
			early[a.Var] = v
		}
	}
	if len(early) > 0 {
		if err := config.UpdateEnv(envFile, early); err != nil {
			return err
		}
	}
	saved := map[string]string{}
	err = kumasync.Apply(cctx, client, plan, func(a kumasync.Action) {
		if v, ok := save[a.Var]; ok && a.NewToken {
			saved[a.Var] = v
			// Saved at once, so a later failure does not lose it.
			if uerr := config.UpdateEnv(envFile, map[string]string{a.Var: v}); uerr != nil {
				fmt.Fprintf(env.Stderr, "pushprobe: saving %s in %s: %v\n", a.Var, envFile, uerr)
			}
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "Done: %s changed", plural(changes, "monitor"))
	if n := len(saved) + len(early); n > 0 {
		fmt.Fprintf(env.Stdout, "; %s saved in %s (mode 0600)", plural(n, "token"), envFile)
	}
	fmt.Fprintln(env.Stdout, ".")
	return nil
}
