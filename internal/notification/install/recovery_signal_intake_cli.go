package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runRecoverySignalIntakeCLI(args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "recovery-signal-history" && args[0] != "recovery-signal-cursor") {
		return fmt.Errorf("Signal recovery intake operation required")
	}
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Reconcile local offline Signal history")
	config := f.String("config", "", "Private gateway runtime configuration")
	state := f.String("state-dir", "", "Quarantined gateway state")
	op := f.String("operation", "", "Exact gateway recovery operation")
	path := f.String("relay-state", "", "Stopped retained relay inbox")
	installed := f.String("installation-hash", "", "Exact installed inbox hash for a managed recovered relay")
	restored := f.Bool("restored", false, "Read a separately prepared quarantined inbox restore")
	stopped := f.Bool("receivers-stopped", false, "Confirm this account's receivers are stopped")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || !*stopped || f.NArg() != 0 || !safefs.ValidID(*op, "n") || (*restored && *installed != "") || (*installed != "" && !safefs.DigestPattern.MatchString(*installed)) {
		return fmt.Errorf("Signal recovery requires --local, --receivers-stopped, exact --operation and one source type")
	}
	for _, p := range []string{*config, *state, *path} {
		if !managedPath(p) {
			return fmt.Errorf("Signal reconciliation requires absolute paths")
		}
	}
	if managedWithin(*path, *state) || managedWithin(*state, *path) {
		return fmt.Errorf("Signal inbox and gateway must be separate")
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if c.Signal == nil || c.Worker.Interaction == nil {
		return fmt.Errorf("Signal interactive account required")
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] Signal reconciliation inputs validated; no account, inbox or gateway state opened")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// Pin the gateway before opening a source: a wrong operation cannot alter
	// even the relay's offline export tables.
	var session interaction.SignalRecoverySession
	if args[0] == "recovery-signal-cursor" {
		session, err = worker.OpenPinnedSignalCursorSession(ctx, *state, c.Worker, *op)
	} else {
		session, err = worker.OpenPinnedSignalRecoverySession(ctx, *state, c.Worker, *op)
	}
	if err != nil {
		return err
	}
	defer session.Close()
	var source interaction.SignalCursorSource
	if *installed != "" {
		source, err = relay.OpenInstalledRecoveryInbox(ctx, *path, *installed)
	} else if *restored {
		source, err = relay.OpenRestoredRecoveryInbox(ctx, *path, c.Signal.Account)
	} else {
		source, err = relay.OpenRecoveryInbox(*path, c.Signal.Account)
	}
	if err != nil {
		return err
	}
	defer source.Close()
	if source.Cursor().Account != c.Signal.Account || source.Cursor().Identity != c.Signal.InboxIdentity {
		return fmt.Errorf("Signal source differs from gateway configuration")
	}
	var result any
	if args[0] == "recovery-signal-history" {
		result, err = interaction.ReconcileSignal(ctx, source, session)
	} else {
		cursor := session.(interaction.SignalCursorSession)
		if _, err = interaction.PrepareSignalRecoveryHandoff(ctx, source, cursor); err == nil {
			result, err = interaction.InstallSignalRecoveryCursor(ctx, source, cursor)
		}
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
