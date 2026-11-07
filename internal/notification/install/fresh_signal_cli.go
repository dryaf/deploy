package install

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runFreshSignalCLI(args []string, out io.Writer) error {
	if len(args) == 1 && args[0] == "__fresh-signal-restart-probe" {
		return json.NewEncoder(out).Encode(managedCapabilities{Version: managedVersion, Schema: worker.StorageSchemaVersion(), FreshSignalRestart: 1})
	}
	if len(args) == 1 && args[0] == "__fresh-signal-probe" {
		return json.NewEncoder(out).Encode(managedCapabilities{Version: managedVersion, Schema: worker.StorageSchemaVersion(), FreshSignal: 1})
	}
	if len(args) == 0 || args[0] != "__fresh-signal-serve" {
		return fmt.Errorf("invalid fresh Signal internal command")
	}
	f := flag.NewFlagSet("notification-worker __fresh-signal-serve", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	local := f.Bool("local", false, "Local managed runtime")
	root := f.String("root", "", "Pinned service root")
	operation := f.String("operation", "", "Pinned operation")
	if err := f.Parse(args[1:]); err != nil {
		return fmt.Errorf("invalid fresh Signal runtime options")
	}
	if !*local || !managedPath(*root) || !safefs.ValidID(*operation, "n") || f.NArg() != 0 {
		return fmt.Errorf("fresh Signal runtime requires exact local root and operation")
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] Fresh Signal runtime arguments validated; no service started")
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := serveFreshSignal(ctx, *root, *operation)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
