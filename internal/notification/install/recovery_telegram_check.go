package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runRecoveryTelegramCheckCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-telegram-check", flag.ContinueOnError)
	local := f.Bool("local", false, "Inspect local published Telegram handoff")
	config := f.String("config", "", "Private gateway runtime JSON")
	state := f.String("state-dir", "", "Published gateway directory")
	commit := f.String("commit-hash", "", "Exact completed gateway publication commit")
	renewal := f.String("renewal-hash", "", "Exact latest completed Telegram renewal (optional)")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !safefs.DigestPattern.MatchString(*commit) {
		return fmt.Errorf("Telegram handoff check requires --local and exact publication commit")
	}
	if *renewal != "" && !safefs.DigestPattern.MatchString(*renewal) {
		return fmt.Errorf("Telegram renewal hash must be an exact digest")
	}
	for _, p := range []string{*config, *state} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
			return fmt.Errorf("Telegram handoff paths must be clean absolute paths")
		}
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if c.Telegram == nil {
		return fmt.Errorf("gateway has no interactive Telegram account")
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] Telegram handoff check inputs validated; no state opened or provider contacted")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var result worker.PublishedTelegramHandoff
	if *renewal == "" {
		result, err = worker.VerifyPublishedTelegramHandoff(ctx, *state, c.Worker, *commit, c.Telegram.Account)
	} else {
		result, err = worker.VerifyRenewedTelegramHandoff(ctx, *state, c.Worker, *commit, c.Telegram.Account, *renewal)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
