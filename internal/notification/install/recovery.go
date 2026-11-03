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

func readRecoveryReview(path string) (v worker.RecoveryMessageReview, err error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return v, fmt.Errorf("review requires a clean absolute path")
	}
	dir, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return v, err
	}
	defer dir.Close()
	f, err := dir.Open(filepath.Base(path), os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return v, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return v, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	// Reviews may retain the decision, application and retry-generation proofs.
	const limit = protocol.MaxRequest + 64*1024
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 || info.Size() > limit {
		return v, fmt.Errorf("review must be an owned private regular file (0600), without aliases, within the size limit")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return v, err
	}
	if err = protocol.Decode(raw, &v, limit); err != nil {
		return v, err
	}
	return v, v.Validate()
}

func runRecoveryCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker "+args[0], flag.ContinueOnError)
	local := f.Bool("local", false, "Use an explicit local staging installation")
	config := f.String("config", "", "Private runtime JSON path")
	state := f.String("state-dir", "", "Private quarantined worker directory")
	producer := f.String("producer", "", "Enrolled producer ID")
	message := f.String("message", "", "Exact ambiguous message ID to review")
	review := f.String("review", "", "Private JSON output from recovery-review")
	action := f.String("action", "", "Record retry or cancel; does not dispatch")
	reason := f.String("reason", "", "Operator reason for the recorded choice")
	risk := f.Bool("accept-duplicate-risk", false, "Acknowledge that retry can duplicate a previously delivered message")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 {
		return fmt.Errorf("recovery staging commands require --local and explicit paths")
	}
	allowed := map[string]bool{"local": true, "config": true, "state-dir": true}
	switch args[0] {
	case "recovery-fence-producer":
		allowed["producer"] = true
	case "recovery-review":
		allowed["producer"], allowed["message"] = true, true
	case "recovery-decide":
		allowed["review"], allowed["action"], allowed["reason"], allowed["accept-duplicate-risk"] = true, true, true, true
	default:
		return fmt.Errorf("unsupported recovery operation")
	}
	invalid := false
	f.Visit(func(v *flag.Flag) {
		if !allowed[v.Name] {
			invalid = true
		}
	})
	if invalid {
		return fmt.Errorf("option not valid for recovery operation")
	}
	for _, p := range []string{*config, *state} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return fmt.Errorf("config and state-dir require clean absolute paths")
		}
	}
	c, err := ReadRuntime(*config)
	if err != nil {
		return err
	}
	var p protocol.Producer
	var d worker.RecoveryDisposition
	if args[0] == "recovery-review" || args[0] == "recovery-fence-producer" {
		for _, enrollment := range c.Worker.Producers {
			if enrollment.Producer.ID == *producer {
				p = enrollment.Producer
			}
		}
		if !p.Valid() {
			return fmt.Errorf("select an enrolled producer")
		}
		if args[0] == "recovery-review" && !protocol.ValidToken(*message) {
			return fmt.Errorf("select an exact message")
		}
	} else {
		v, err := readRecoveryReview(*review)
		if err != nil {
			return err
		}
		d = worker.RecoveryDisposition{ID: v.RequestID, Operation: v.Operation, Identity: v.Identity,
			Producer: v.Message.Producer, MessageID: v.Message.ID, MessageHash: v.MessageHash, SummaryHash: v.SummaryHash,
			Action: *action, Reason: *reason, Actor: fmt.Sprintf("uid:%d", os.Geteuid()), AcceptDuplicateRisk: *risk}
		if err = d.Validate(); err != nil {
			return err
		}
		enrolled := false
		for _, enrollment := range c.Worker.Producers {
			if enrollment.Producer == d.Producer {
				enrolled = true
			}
		}
		if !enrolled {
			return fmt.Errorf("review producer is not enrolled")
		}
	}
	if ui.DryRun {
		_, err = fmt.Fprintf(out, "[DRY] notification-worker %s: arguments validated; current recovery state not checked; no files or delivery changed\n", args[0])
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if args[0] == "recovery-fence-producer" {
		v, err := worker.FenceRecoveryOwnership(ctx, *state, c.Worker, p)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(v)
	}
	session, err := worker.OpenRecoveryDispositionSession(ctx, *state, c.Worker)
	if err != nil {
		return err
	}
	defer session.Close()
	if args[0] == "recovery-review" {
		v, err := session.ReviewRecoveryMessage(ctx, p, *message)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(v)
	}
	record, err := session.DecideRecoveryMessage(ctx, d)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(record)
}
