package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func runInspectionCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Inspect an explicit local worker")
	state := f.String("state-dir", "", "Existing private worker state directory")
	socket := f.String("socket", "", "Worker API socket")
	producer := f.String("producer", "", "Enrolled producer ID")
	epoch := f.String("producer-epoch", "", "Enrolled producer epoch")
	after := f.Int64("after", 0, "Exclusive inspection position (not an acknowledgement cursor)")
	limit := f.Int("limit", 64, "Maximum records (1..128)")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	p := protocol.Producer{ID: *producer, Epoch: *epoch}
	if !*local || f.NArg() != 0 || !managedPath(*state) || !managedPath(*socket) || !p.Valid() || *after < 0 || *limit < 1 || *limit > 128 {
		return fmt.Errorf("inspection requires --local, absolute state/socket paths, producer identity and a bounded range")
	}
	d, err := privateManagedDir(*state, true)
	if err != nil {
		return err
	}
	defer d.Close()
	var identity protocol.Identity
	if err = d.ReadJSON("identity.json", &identity); err != nil {
		return err
	}
	if !identity.Valid() {
		return fmt.Errorf("invalid worker identity")
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] notification inspection: no API calls or cursor changes")
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	api := client.Client{Socket: *socket, Identity: identity}
	r := protocol.Request{Producer: p, After: *after, Limit: *limit}
	switch args[0] {
	case "queue-inspect":
		r.Operation = "inspection.queue"
		var page worker.QueueInspection
		if _, err = api.Call(ctx, r, &page); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(page)
	case "receipts-inspect":
		r.Operation = "inspection.receipts"
		var page worker.ReceiptInspection
		if _, err = api.Call(ctx, r, &page); err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(page)
	default:
		return fmt.Errorf("unknown inspection operation")
	}
}
