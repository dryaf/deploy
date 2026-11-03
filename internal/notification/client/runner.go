package client

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// CommandHandler is implemented by the specialist worker. Prepare is read-only.
// Apply must validate current local policy, scope and evidence inside the given
// transaction. It may mutate domain tables or admit a durable job, never do I/O.
type CommandHandler interface {
	Descriptor() interaction.Descriptor
	Prepare(context.Context, interaction.Command) (interaction.Preparation, error)
	Apply(*sql.Tx, interaction.Command) (interaction.Result, error)
}

type Runner struct {
	Client   Client
	Producer protocol.Producer
	Ledger   *interaction.Ledger
	Handlers map[string]CommandHandler // namespaced descriptor ID
	Now      func() time.Time
}
type commandReference struct {
	ID   string `json:"id"`
	Hash string `json:"hash,omitempty"`
}

// Step is bounded and can be scheduled independently of backup/scan execution.
// Durable results are delivered before pulling more work. Every handoff is safe
// to repeat after a lost response; running jobs complete through Ledger.CompleteJob.
func (r Runner) Step(ctx context.Context) error {
	if r.Ledger == nil || !r.Producer.Valid() {
		return fmt.Errorf("invalid command runner")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := r.FlushResults(ctx); err != nil {
		return err
	}
	views, err := r.Client.Commands(ctx, r.Producer)
	if err != nil {
		return err
	}
	var failures []error
	for _, v := range views {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = r.handle(ctx, v); err != nil {
			failures = append(failures, err)
		}
	}
	if err = r.FlushResults(ctx); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (r Runner) FlushResults(ctx context.Context) error {
	items, err := r.Ledger.PendingResults(ctx)
	if err != nil {
		return err
	}
	for _, p := range items {
		if p.Receipt == nil {
			receipt, err := r.Client.SubmitCommandResult(ctx, r.Producer, p.Result)
			if err != nil {
				return err
			}
			if err = r.Ledger.ImportReceipt(ctx, receipt); err != nil {
				return err
			}
			p.Receipt = &receipt
		}
		if err = r.Client.CommandCall(ctx, r.Producer, "command.result-ack", p.Receipt, nil); err != nil {
			return err
		}
		if err = r.Ledger.Acknowledged(ctx, *p.Receipt); err != nil {
			return err
		}
	}
	return nil
}

func (r Runner) handle(ctx context.Context, v interaction.View) error {
	c := v.Command
	h := r.Handlers[c.Descriptor.ID]
	if h == nil || protocol.Hash(h.Descriptor()) != protocol.Hash(c.Descriptor) {
		return fmt.Errorf("worker command contract unavailable")
	}
	if err := r.Ledger.Import(ctx, c); err != nil {
		return err
	}
	if err := r.Client.CommandCall(ctx, r.Producer, "command.import-ack", commandReference{ID: c.ID, Hash: protocol.Hash(c)}, nil); err != nil {
		return err
	}
	latest, err := r.Ledger.Latest(ctx, c.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if latest.Phase == "execute" {
		_, err = r.Client.SubmitCommandResult(ctx, r.Producer, latest)
		return err
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	switch v.State {
	case "received":
		if latest.Phase == "prepare" {
			return nil
		}
		p, err := h.Prepare(ctx, c)
		if err != nil {
			var rejected *interaction.Rejection
			if errors.As(err, &rejected) {
				_, err = r.Ledger.RejectPreparation(ctx, c, rejected.Message)
			}
			return err
		}
		_, err = r.Ledger.Prepare(ctx, c, p, now())
		return err
	case "queued":
		var grant interaction.Grant
		if err = r.Client.CommandCall(ctx, r.Producer, "command.grant", commandReference{ID: c.ID}, &grant); err != nil {
			return err
		}
		if grant.ID == "" {
			return nil
		}
		prepared := ""
		if latest.Preparation != nil {
			prepared = latest.Preparation.Hash
		}
		_, err = r.Ledger.Execute(ctx, c, grant, prepared, now(), h.Apply)
		return err
	case "outcome_unknown":
		if v.Grant == nil {
			return fmt.Errorf("missing admission evidence")
		}
		proof, err := r.Ledger.ProveNonAdmission(ctx, c, *v.Grant)
		if err != nil {
			return err
		}
		return r.Client.CommandCall(ctx, r.Producer, "command.non-admission", proof, nil)
	case "expired", "rejected":
		return r.Ledger.FenceAdmission(ctx, c.ID)
	case "needs_confirmation", "running":
		return nil
	default:
		return fmt.Errorf("unknown command state")
	}
}
