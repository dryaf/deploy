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
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func runRecoveryLEDCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Use an already quarantined local installation")
	config := f.String("config", "", "Private runtime JSON path")
	state := f.String("state-dir", "", "Private quarantined worker state directory")
	refresh := f.Bool("refresh", false, "Capture a new registry observation; pending captures resume first")
	inventory := f.String("inventory", "", "Exact reviewed LED inventory ID")
	hash := f.String("inventory-hash", "", "Exact reviewed LED inventory digest")
	preservation := f.String("preservation-hash", "", "Exact committed LED preservation digest")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if args[0] != "recovery-led-review" && args[0] != "recovery-led-preserve" && args[0] != "recovery-led-adopt" {
		return fmt.Errorf("unknown LED recovery operation")
	}
	invalid := false
	f.Visit(func(v *flag.Flag) {
		switch v.Name {
		case "inventory", "inventory-hash":
			invalid = invalid || args[0] == "recovery-led-review"
		case "refresh":
			invalid = invalid || args[0] != "recovery-led-review"
		case "preservation-hash":
			invalid = invalid || args[0] != "recovery-led-adopt"
		}
	})
	if invalid {
		return fmt.Errorf("LED recovery options do not match the operation")
	}
	if args[0] != "recovery-led-review" && (!safefs.ValidID(*inventory, "n") || !safefs.DigestPattern.MatchString(*hash)) {
		return fmt.Errorf("LED preservation/adoption requires --inventory and --inventory-hash from review")
	}
	if args[0] == "recovery-led-adopt" && !safefs.DigestPattern.MatchString(*preservation) {
		return fmt.Errorf("LED adoption requires --preservation-hash from committed preservation")
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("LED recovery requires --local and explicit paths")
	}
	for _, path := range []string{*config, *state} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("LED recovery requires clean absolute paths")
		}
	}
	runtime, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	if runtime.LEDSocket == "" || filepath.Clean(runtime.LEDSocket) != runtime.LEDSocket {
		return fmt.Errorf("LED recovery requires a configured absolute LED socket")
	}
	if ui.DryRun {
		_, err = fmt.Fprintf(out, "[DRY] %s: configuration validated; no registry requests or recovery files written\n", args[0])
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if args[0] == "recovery-led-adopt" {
		request := worker.RecoveryLEDAdoptionRequest{InventoryID: *inventory, InventoryHash: *hash, PreservationHash: *preservation}
		view, err := worker.AdoptRecoveryLED(ctx, *state, runtime.Worker, runtime.LEDSocket, request, led.Client{Socket: runtime.LEDSocket, Budget: 2 * time.Second, Endpoint: runtime.LEDEndpoint})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(view)
	}
	if args[0] == "recovery-led-preserve" {
		view, err := worker.PreserveRecoveryLED(ctx, *state, runtime.Worker, runtime.LEDSocket, *inventory, *hash, led.Client{Socket: runtime.LEDSocket, Budget: 2 * time.Second, Endpoint: runtime.LEDEndpoint})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(view)
	}
	review, err := worker.ReviewRecoveryLED(ctx, *state, runtime.Worker, runtime.LEDSocket, led.Client{Socket: runtime.LEDSocket, Budget: 2 * time.Second, Endpoint: runtime.LEDEndpoint}, *refresh)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(review)
}
