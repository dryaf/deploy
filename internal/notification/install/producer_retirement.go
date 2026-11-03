package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func runProducerRetirementCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker producer-retire", flag.ContinueOnError)
	local := f.Bool("local", false, "Use an existing stopped local worker")
	config := f.String("config", "", "Private runtime JSON")
	state := f.String("state-dir", "", "Existing private state directory")
	producer := f.String("producer", "", "Enrolled producer ID")
	reason := f.String("reason", "", "Audit reason for permanent producer retirement")
	apply := f.Bool("apply", false, "Permanently retire this producer epoch; otherwise inspect prerequisites")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*config) || !managedPath(*state) {
		return fmt.Errorf("producer retirement requires --local and absolute configuration/state paths")
	}
	actor := "uid-" + strconv.Itoa(os.Getuid())
	if err := worker.ValidateProducerRetirementReason(actor, *reason); err != nil {
		return err
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
		_, err = fmt.Fprintln(out, "[DRY] retirement arguments validated; state/prerequisites not inspected; no producer retired")
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
	view, err := s.RetireProducer(ctx, p, actor, *reason, *apply)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(view)
}
