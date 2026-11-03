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

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runRecoverySignalInstallCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-signal-install", flag.ContinueOnError)
	local := f.Bool("local", false, "Install offline local Signal recovery state")
	config := f.String("config", "", "Private gateway runtime JSON")
	state := f.String("state-dir", "", "Published gateway directory")
	commit := f.String("commit-hash", "", "Exact completed gateway publication commit")
	source := f.String("relay-source", "", "Existing offline relay inbox with completed cursor")
	destination := f.String("relay-destination", "", "Private empty destination or exact interrupted installation")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !safefs.DigestPattern.MatchString(*commit) {
		return fmt.Errorf("Signal installation requires --local and exact publication commit")
	}
	for _, p := range []string{*config, *state, *source, *destination} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" {
			return fmt.Errorf("Signal installation paths must be clean absolute paths")
		}
	}
	for _, pair := range [][2]string{{*state, *source}, {*state, *destination}, {*source, *destination}} {
		if managedWithin(pair[0], pair[1]) || managedWithin(pair[1], pair[0]) {
			return fmt.Errorf("gateway, source and destination must not overlap")
		}
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if c.Signal == nil {
		return fmt.Errorf("gateway has no interactive Signal account")
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] Signal recovery runtime installation inputs validated; no state opened, service started or provider contacted")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var result relay.SignalRuntimeInstallation
	err = worker.WithPublishedSignalCursor(ctx, *state, c.Worker, *commit, c.Signal.Account, func(p interaction.SignalRecoveryCursor) error {
		if p.Handoff.Before.Identity != c.Signal.InboxIdentity {
			return fmt.Errorf("published Signal source differs from configured inbox identity")
		}
		var e error
		result, e = relay.InstallSignalRuntime(ctx, *source, *destination, p)
		return e
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
