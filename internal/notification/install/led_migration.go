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
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func runLEDMigrationCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker led-migrate", flag.ContinueOnError)
	local := f.Bool("local", false, "Use a stopped local staging installation")
	beforePath := f.String("config", "", "Original private runtime JSON")
	afterPath := f.String("next-config", "", "New private runtime JSON; only LED bindings may differ")
	state := f.String("state-dir", "", "Existing private worker state directory")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("LED migration requires --local and explicit paths")
	}
	for _, path := range []string{*beforePath, *afterPath, *state} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("LED migration requires clean absolute paths")
		}
	}
	before, err := ReadRuntime(*beforePath)
	if err != nil {
		return err
	}
	after, err := ReadRuntime(*afterPath)
	if err != nil {
		return err
	}
	// Socket, provider, account and all non-LED worker configuration stay pinned.
	oldCompare, newCompare := before, after
	oldCompare.Worker.LEDBindings, newCompare.Worker.LEDBindings = nil, nil
	if before.LEDSocket == "" || protocol.Hash(oldCompare) != protocol.Hash(newCompare) {
		return fmt.Errorf("only worker.led_bindings may change during LED migration")
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] LED binding migration: configurations validated; worker state and daemon have not been inspected or changed")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	result, err := worker.MigrateLEDBindings(ctx, *state, before.Worker, after.Worker, before.LEDSocket, led.Client{Socket: before.LEDSocket, Budget: 2 * time.Second, Endpoint: before.LEDEndpoint})
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
