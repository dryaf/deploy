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

func runRecoveryPublicationCLI(args []string, out io.Writer) error {
	return runRecoveryPublicationMode(args, out, "recovery-publication-prepare")
}

func runRecoveryPublicationVerifyCLI(args []string, out io.Writer) error {
	return runRecoveryPublicationMode(args, out, "recovery-publication-verify")
}

func runRecoveryPublishCLI(args []string, out io.Writer) error {
	return runRecoveryPublicationMode(args, out, "recovery-publish")
}

func runRecoveryPublicationMode(args []string, out io.Writer, operationName string) error {
	requiresImage := operationName != "recovery-publication-prepare"
	f := flag.NewFlagSet("notification-worker "+operationName, flag.ContinueOnError)
	local := f.Bool("local", false, "Use an existing offline worker directory")
	config := f.String("config", "", "Private runtime JSON path")
	state := f.String("state-dir", "", "Existing offline worker state directory")
	operation := f.String("operation", "", "Exact operation from recovery-prepare")
	review := f.String("review-hash", "", "Exact review_hash from recovery-status")
	checkpoint := f.String("checkpoint-hash", "", "Exact checkpoint_hash from recovery-checkpoint")
	proof := f.String("proof-hash", "", "Exact proof_hash from recovery-checkpoint-verify")
	var imageHash string
	if requiresImage {
		f.StringVar(&imageHash, "image-hash", "", "Exact image_hash from recovery-publication-prepare")
	}
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !safefs.ValidID(*operation, "n") || !safefs.DigestPattern.MatchString(*review) || !safefs.DigestPattern.MatchString(*checkpoint) || !safefs.DigestPattern.MatchString(*proof) {
		return fmt.Errorf("%s requires --local, exact --operation/--review-hash/--checkpoint-hash/--proof-hash and no positional arguments", operationName)
	}
	if requiresImage && !safefs.DigestPattern.MatchString(imageHash) {
		return fmt.Errorf("%s requires an exact --image-hash", operationName)
	}
	for _, p := range []string{*config, *state} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\x00\r\n") {
			return fmt.Errorf("%s requires clean absolute configuration/state paths", operationName)
		}
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintf(out, "[DRY] %s arguments/configuration validated; no state opened or publication image written\n", operationName)
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var result any
	switch operationName {
	case "recovery-publication-verify":
		result, err = worker.VerifyRecoveryPublication(ctx, *state, c.Worker, *operation, *review, *checkpoint, *proof, imageHash)
	case "recovery-publication-prepare":
		result, err = worker.PrepareRecoveryPublication(ctx, *state, c.Worker, *operation, *review, *checkpoint, *proof)
	case "recovery-publish":
		result, err = worker.PublishRecovery(ctx, *state, c.Worker, *operation, *review, *checkpoint, *proof, imageHash)
	default:
		return fmt.Errorf("unknown publication operation")
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
