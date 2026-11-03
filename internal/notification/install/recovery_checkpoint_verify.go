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

func runRecoveryCheckpointVerifyCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-checkpoint-verify", flag.ContinueOnError)
	local := f.Bool("local", false, "Verify a retained historical recovery checkpoint")
	config := f.String("config", "", "Private runtime JSON path")
	state := f.String("state-dir", "", "Existing offline worker state directory")
	operation := f.String("operation", "", "Exact operation from recovery-prepare")
	review := f.String("review-hash", "", "Exact review_hash from recovery-status")
	checkpoint := f.String("checkpoint-hash", "", "Exact checkpoint_hash from recovery-checkpoint")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !safefs.ValidID(*operation, "n") || !safefs.DigestPattern.MatchString(*review) || !safefs.DigestPattern.MatchString(*checkpoint) {
		return fmt.Errorf("recovery checkpoint verification requires --local, exact --operation/--review-hash/--checkpoint-hash and no positional arguments")
	}
	for _, p := range []string{*config, *state} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\x00\r\n") {
			return fmt.Errorf("recovery checkpoint verification requires clean absolute configuration/state paths")
		}
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] recovery checkpoint verification arguments/configuration validated; no state opened or proof verified")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := worker.VerifyRecoveryCheckpoint(ctx, *state, c.Worker, *operation, *review, *checkpoint)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
