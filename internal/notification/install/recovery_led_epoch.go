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

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runRecoveryLEDEpochCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-led-handoff", flag.ContinueOnError)
	local := f.Bool("local", false, "Use an existing quarantined local worker")
	config := f.String("config", "", "Private runtime JSON")
	state := f.String("state-dir", "", "Quarantined worker state directory")
	adoption := f.String("adoption-hash", "", "Exact committed LED adoption digest")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*config) || !managedPath(*state) || !safefs.DigestPattern.MatchString(*adoption) {
		return fmt.Errorf("LED history handoff requires --local, absolute paths and --adoption-hash")
	}
	runtime, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if !managedPath(runtime.LEDSocket) {
		return fmt.Errorf("LED history handoff requires a pinned LED socket")
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] LED history handoff arguments/configuration validated; no state opened, history changed or daemon contacted")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	v, err := worker.HandoffRecoveryLED(ctx, *state, runtime.Worker, runtime.LEDSocket, *adoption)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(v)
}
