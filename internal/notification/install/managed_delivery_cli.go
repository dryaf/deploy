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

	"github.com/dryaf/deploy/internal/ui"
)

func runDeliveryCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Use this user's managed installation")
	root := f.String("root", "", "Existing managed gateway installation")
	manifest := f.String("manifest-hash", "", "Exact manifest shown by installation-status")
	qualification := f.String("qualification-hash", "", "Exact latest acknowledged QualificationRecord hash")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("delivery lifecycle requires --local and exact pins")
	}
	bad := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "qualification-hash" && args[0] != "activate-delivery" {
			bad = true
		}
	})
	if bad {
		return fmt.Errorf("qualification hash is only valid for activation")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m, err := newManagedInstaller().prepareDelivery(ctx, args[0], *root, *manifest, *qualification, ui.DryRun)
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] delivery lifecycle inputs and retained evidence validated; manager, capability, state enrollment and runtime checks deferred")
		return err
	}
	return json.NewEncoder(out).Encode(m)
}
