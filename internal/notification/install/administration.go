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

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
	"golang.org/x/sys/unix"
)

func readMessageReview(path string) (v worker.MessageReview, err error) {
	if !managedPath(path) {
		return v, fmt.Errorf("review requires a clean absolute path")
	}
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return v, err
	}
	defer d.Close()
	f, err := d.Open(filepath.Base(path), unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return v, err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return v, err
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Geteuid()) || i.Mode().Perm() != 0600 || i.Size() > protocol.MaxRequest+16384 {
		return v, fmt.Errorf("review must be a private owned file (0600) within the size limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, protocol.MaxRequest+16385))
	if err != nil {
		return v, err
	}
	if err = protocol.Decode(b, &v, protocol.MaxRequest+16384); err != nil {
		return v, err
	}
	return v, v.Validate()
}

func runMessageAdministrationCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Use an explicit stopped local worker")
	config := f.String("config", "", "Private runtime JSON")
	state := f.String("state-dir", "", "Private existing state directory; worker must be stopped")
	producer := f.String("producer", "", "Enrolled producer ID")
	message := f.String("message", "", "Exact event ID to review")
	review := f.String("review", "", "Private JSON from message-review")
	action := f.String("action", "", "retry or cancel")
	reason := f.String("reason", "", "Reason recorded in the durable audit")
	risk := f.Bool("accept-duplicate-risk", false, "Acknowledge that an ambiguous retry can duplicate a delivery")
	id := f.String("decision", "", "Decision ID from the review")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*config) || !managedPath(*state) {
		return fmt.Errorf("administration requires --local and explicit absolute config/state paths")
	}
	allowed := map[string]bool{"local": true, "config": true, "state-dir": true}
	switch args[0] {
	case "message-review":
		allowed["producer"], allowed["message"] = true, true
	case "message-decide":
		allowed["review"], allowed["action"], allowed["reason"], allowed["accept-duplicate-risk"] = true, true, true, true
	case "message-decision-status":
		allowed["decision"] = true
	default:
		return fmt.Errorf("unknown message administration command")
	}
	invalid := false
	f.Visit(func(v *flag.Flag) {
		if !allowed[v.Name] {
			invalid = true
		}
	})
	if invalid {
		return fmt.Errorf("option not valid for message administration command")
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	var p protocol.Producer
	var d worker.MessageDecision
	switch args[0] {
	case "message-review":
		for _, e := range c.Worker.Producers {
			if e.Producer.ID == *producer {
				p = e.Producer
			}
		}
		if !p.Valid() || !protocol.ValidToken(*message) {
			return fmt.Errorf("select an enrolled producer and exact event")
		}
	case "message-decide":
		v, e := readMessageReview(*review)
		if e != nil {
			return e
		}
		d = worker.MessageDecision{Version: 1, Review: v, Action: *action, Reason: *reason, AcceptDuplicateRisk: *risk}
		if err = d.Validate(); err != nil {
			return err
		}
		found := false
		for _, e := range c.Worker.Producers {
			if e.Producer == v.Job.Message.Producer {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("review producer is not enrolled")
		}
	case "message-decision-status":
		if !safefs.ValidID(*id, "n") {
			return fmt.Errorf("invalid decision ID")
		}
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] administrative arguments validated; current state not checked; no decision, cursor or service changed")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dir, err := privateManagedDir(*state, true)
	if err != nil {
		return err
	}
	var expected protocol.Identity
	err = dir.ReadJSON("identity.json", &expected)
	dir.Close()
	if err != nil {
		return err
	}
	s, err := worker.OpenManagedVerification(*state, c.Worker, expected)
	if err != nil {
		return err
	}
	defer s.Close()
	var value any
	switch args[0] {
	case "message-review":
		value, err = s.ReviewMessage(ctx, p, *message)
	case "message-decide":
		value, err = s.DecideMessage(ctx, d)
	case "message-decision-status":
		value, err = s.MessageDecision(ctx, *id)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(value)
}
