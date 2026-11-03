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

	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runSignalVerificationPrepareCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker signal-verification-prepare", flag.ContinueOnError)
	local := f.Bool("local", false, "Publish a verification-only Signal endpoint")
	path := f.String("spec", "", "Private endpoint enrollment JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*path) {
		return fmt.Errorf("Signal verification preparation requires --local --spec ABS")
	}
	raw, err := nativeSignalRead(*path, safefs.MaxRecordBytes, true)
	if err != nil {
		return err
	}
	var spec SignalVerificationSpec
	if err = safefs.DecodeJSON(raw, &spec, safefs.MaxRecordBytes); err != nil {
		return err
	}
	if _, _, err = spec.prepare(); err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] Signal verification inputs validated; no recovery state opened, socket bound or service changed")
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	m, err := PrepareSignalVerification(ctx, spec)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(m)
}
func runSignalVerificationServeCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker signal-verification-serve", flag.ContinueOnError)
	local := f.Bool("local", false, "Serve only the enrolled Signal verification health endpoint")
	root := f.String("root", "", "Private enrolled verification endpoint root")
	pin := f.String("manifest-hash", "", "Exact completed verification manifest hash")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*root) || !safefs.DigestPattern.MatchString(*pin) {
		return fmt.Errorf("Signal verifier requires --local --root ABS --manifest-hash HASH")
	}
	if ui.DryRun {
		_, err := fmt.Fprintln(out, "[DRY] Signal verifier arguments validated; no state opened or socket bound")
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := ServeSignalVerification(ctx, *root, *pin)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
