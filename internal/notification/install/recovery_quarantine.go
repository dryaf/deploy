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

func runRecoveryQuarantineCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-prepare", flag.ContinueOnError)
	local := f.Bool("local", false, "Prepare or resume offline local snapshot quarantine")
	config := f.String("config", "", "Private runtime JSON path")
	state := f.String("state-dir", "", "Existing offline worker state directory")
	from := f.String("from", "", "Completed verified snapshot directory")
	id := f.String("snapshot-id", "", "Exact ID from snapshot-verify output")
	hash := f.String("database-hash", "", "Exact database_hash from snapshot-verify output")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !safefs.ValidID(*id, "n") || !safefs.DigestPattern.MatchString(*hash) {
		return fmt.Errorf("recovery preparation requires --local, exact --snapshot-id and --database-hash, and no positional arguments")
	}
	for _, p := range []string{*config, *state, *from} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\x00\r\n") {
			return fmt.Errorf("recovery preparation requires clean absolute configuration/state/snapshot paths")
		}
	}
	if *state == *from || strings.HasPrefix(*state, *from+"/") || strings.HasPrefix(*from, *state+"/") {
		return fmt.Errorf("snapshot and worker state directories must be separate")
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] recovery preparation arguments/configuration validated; no state or snapshot opened, identity changed or services touched")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	v, err := worker.QuarantineRecovery(ctx, *state, *from, c.Worker, *id, *hash)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(v)
}
