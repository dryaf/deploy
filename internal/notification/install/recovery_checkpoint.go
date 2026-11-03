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

func runRecoveryCheckpointCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-checkpoint", flag.ContinueOnError)
	local := f.Bool("local", false, "Capture the exact reviewed offline recovery projection")
	config := f.String("config", "", "Private runtime JSON path")
	state := f.String("state-dir", "", "Existing offline worker state directory")
	operation := f.String("operation", "", "Exact operation from recovery-prepare")
	review := f.String("review-hash", "", "Exact review_hash from recovery-status")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !safefs.ValidID(*operation, "n") || !safefs.DigestPattern.MatchString(*review) {
		return fmt.Errorf("recovery checkpoint requires --local, exact --operation/--review-hash and no positional arguments")
	}
	for _, p := range []string{*config, *state} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\x00\r\n") {
			return fmt.Errorf("recovery checkpoint requires clean absolute configuration/state paths")
		}
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] recovery checkpoint arguments/configuration validated; no state opened or checkpoint written")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := worker.CheckpointRecovery(ctx, *state, c.Worker, *operation, *review)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
