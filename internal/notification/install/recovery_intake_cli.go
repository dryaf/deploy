package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runRecoveryIntakeCLI(args []string, out io.Writer) error {
	return runRecoveryIntakeCLIWith(args, out, nil)
}

func runRecoveryIntakeCLIWith(args []string, out io.Writer, transport http.RoundTripper) error {
	if len(args) == 0 {
		return fmt.Errorf("recovery intake operation required")
	}
	operation := args[0]
	allowed := map[string]map[string]bool{
		"recovery-telegram-history": {"from": true, "snapshot-id": true, "database-hash": true},
		"recovery-telegram-drain":   {"receivers-stopped": true},
		"recovery-requests-seal":    {"reconciliation-complete": true},
		"recovery-telegram-cursor":  {},
	}
	flags, ok := allowed[operation]
	if !ok {
		return fmt.Errorf("unknown recovery intake operation")
	}
	f := flag.NewFlagSet("notification-worker "+operation, flag.ContinueOnError)
	local := f.Bool("local", false, "Use the stopped, quarantined local gateway")
	config := f.String("config", "", "Private runtime configuration")
	state := f.String("state-dir", "", "Quarantined gateway directory")
	id := f.String("operation", "", "Exact recovery operation from recovery-prepare")
	from := f.String("from", "", "Separately retained gateway history snapshot")
	snapshotID := f.String("snapshot-id", "", "Exact history snapshot ID")
	hash := f.String("database-hash", "", "Exact history snapshot database digest")
	stopped := f.Bool("receivers-stopped", false, "Acknowledge all other receivers for this Telegram account are stopped")
	complete := f.Bool("reconciliation-complete", false, "Acknowledge available provider and specialist command mappings have been imported")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !safefs.ValidID(*id, "n") {
		return fmt.Errorf("recovery intake requires --local, exact --operation, and no positional arguments")
	}
	invalid := false
	f.Visit(func(v *flag.Flag) {
		if v.Name != "local" && v.Name != "config" && v.Name != "state-dir" && v.Name != "operation" && !flags[v.Name] {
			invalid = true
		}
	})
	if invalid {
		return fmt.Errorf("option not valid for selected recovery intake operation")
	}
	for _, p := range []string{*config, *state} {
		if !managedPath(p) || strings.ContainsAny(p, "\x00\r\n") {
			return fmt.Errorf("recovery intake requires clean absolute configuration/state paths")
		}
	}
	if operation == "recovery-telegram-history" {
		if !managedPath(*from) || !safefs.ValidID(*snapshotID, "n") || !safefs.DigestPattern.MatchString(*hash) || managedWithin(*from, *state) || managedWithin(*state, *from) {
			return fmt.Errorf("history import requires separate snapshot path and exact snapshot/database pins")
		}
	}
	if operation == "recovery-telegram-drain" && !*stopped {
		return fmt.Errorf("Telegram drain requires --receivers-stopped; polling may acknowledge provider updates")
	}
	if operation == "recovery-requests-seal" && !*complete {
		return fmt.Errorf("request sealing requires --reconciliation-complete after available provider and command imports")
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if c.Worker.Interaction == nil || operation != "recovery-requests-seal" && c.Telegram == nil {
		return fmt.Errorf("recovery intake is not configured for this operation")
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] recovery intake arguments/configuration validated; no state opened, history changed or provider contacted")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var result any
	switch operation {
	case "recovery-telegram-history":
		result, err = worker.ReconcilePinnedTelegramHistory(ctx, *state, *from, c.Telegram.Account, c.Worker, *id, *snapshotID, *hash)
	case "recovery-telegram-drain":
		result, err = worker.DrainPinnedTelegramRecovery(ctx, *state, c.Worker, *id, provider.TelegramPoller{Profile: *c.Telegram, Transport: transport})
	case "recovery-requests-seal":
		result, err = worker.SealPinnedRecoveryRequests(ctx, *state, c.Worker, *id)
	case "recovery-telegram-cursor":
		result, err = worker.CommitPinnedTelegramRecoveryCursor(ctx, *state, c.Worker, *id, c.Telegram.Account)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
