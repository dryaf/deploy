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
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func runReceiptCompactionCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker receipts-compact", flag.ContinueOnError)
	local := f.Bool("local", false, "Use an explicit stopped local worker")
	config := f.String("config", "", "Private runtime JSON")
	state := f.String("state-dir", "", "Private existing state directory")
	producer := f.String("producer", "", "Enrolled producer ID")
	limit := f.Int("limit", 128, "Maximum intermediate receipts per pass (1..1024)")
	apply := f.Bool("apply", false, "Delete eligible intermediate receipts; otherwise preview")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*config) || !managedPath(*state) || *limit < 1 || *limit > 1024 {
		return fmt.Errorf("receipt compaction requires --local, absolute config/state paths and a bounded limit")
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	var p protocol.Producer
	for _, e := range c.Worker.Producers {
		if e.Producer.ID == *producer {
			p = e.Producer
		}
	}
	if !p.Valid() {
		return fmt.Errorf("select an enrolled producer")
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] receipt compaction arguments validated; state and eligibility not inspected; no history changed")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := privateManagedDir(*state, true)
	if err != nil {
		return err
	}
	var id protocol.Identity
	err = d.ReadJSON("identity.json", &id)
	d.Close()
	if err != nil {
		return err
	}
	s, err := worker.OpenManagedVerification(*state, c.Worker, id)
	if err != nil {
		return err
	}
	defer s.Close()
	result, err := s.CompactReceipts(ctx, p, *limit, *apply)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
