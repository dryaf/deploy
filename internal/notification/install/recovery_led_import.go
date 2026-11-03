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

func runRecoveryLEDImportCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-led-import-cleanup", flag.ContinueOnError)
	local := f.Bool("local", false, "Use an existing quarantined local worker")
	config := f.String("config", "", "Private runtime JSON")
	state := f.String("state-dir", "", "Quarantined worker state directory")
	epoch := f.String("epoch-hash", "", "Exact committed LED epoch handoff digest")
	result := f.String("result-hash", "", "Exact completed domain-authorized cleanup result digest")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*config) || !managedPath(*state) || !safefs.DigestPattern.MatchString(*epoch) || !safefs.DigestPattern.MatchString(*result) {
		return fmt.Errorf("LED cleanup import requires --local, absolute paths, --epoch-hash and --result-hash")
	}
	runtime, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if !managedPath(runtime.LEDSocket) {
		return fmt.Errorf("LED cleanup import requires a pinned LED socket")
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] LED cleanup import arguments/configuration validated; no state opened, receipt imported or daemon contacted")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	v, err := worker.ImportRecoveryLEDCleanup(ctx, *state, runtime.Worker, runtime.LEDSocket, *epoch, *result)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(v)
}
