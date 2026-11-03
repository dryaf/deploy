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
	"strings"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runRecoveryStatusCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-status", flag.ContinueOnError)
	local := f.Bool("local", false, "Review offline local recovery evidence")
	config := f.String("config", "", "Private runtime JSON path")
	state := f.String("state-dir", "", "Existing offline worker state directory")
	operation := f.String("operation", "", "Exact operation from recovery-prepare")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !safefs.ValidID(*operation, "n") {
		return fmt.Errorf("recovery status requires --local, exact --operation and no positional arguments")
	}
	for _, p := range []string{*config, *state} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\x00\r\n") {
			return fmt.Errorf("recovery status requires clean absolute configuration/state paths")
		}
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] recovery status arguments/configuration validated; no state opened or services contacted")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := worker.ReviewRecovery(ctx, *state, c.Worker, *operation)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
