package worker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type commandRef struct {
	ID   string `json:"id"`
	Hash string `json:"hash,omitempty"`
}

func (s *Store) handleCommand(ctx context.Context, r protocol.Request) (any, error) {
	if s.config.Interaction == nil {
		return nil, fmt.Errorf("interaction disabled")
	}
	decode := func(out any) error { return protocol.Decode(r.Interaction, out, protocol.MaxRequest) }
	if r.Operation == "command.ingest" || r.Operation == "command.query" {
		if r.Producer != (protocol.Producer{}) {
			return nil, fmt.Errorf("unexpected producer")
		}
		var in interaction.Inbound
		if err := decode(&in); err != nil {
			return nil, err
		}
		if r.Operation == "command.query" {
			handled, err := s.AdmitGatewayQuery(ctx, in)
			if err != nil {
				return nil, err
			}
			if !handled {
				return nil, fmt.Errorf("gateway query required")
			}
			return struct {
				ID       string `json:"id"`
				Recorded bool   `json:"recorded"`
			}{"Q-" + in.RequestKey(), true}, nil
		}
		return s.AdmitCommand(ctx, in)
	}
	if _, err := s.config.enrollment(r.Producer); err != nil {
		return nil, err
	}
	switch r.Operation {
	case "command.manifest":
		var empty struct{}
		if err := decode(&empty); err != nil {
			return nil, err
		}
		for _, w := range s.config.Interaction.Workers {
			if w.Producer == r.Producer {
				return w, nil
			}
		}
		return nil, fmt.Errorf("worker not enrolled")
	case "command.list":
		var empty struct{}
		if err := decode(&empty); err != nil {
			return nil, err
		}
		return s.Commands(ctx, r.Producer)
	case "command.get", "command.import-ack", "command.grant":
		var ref commandRef
		if err := decode(&ref); err != nil {
			return nil, err
		}
		if !protocol.ValidToken(ref.ID) {
			return nil, fmt.Errorf("invalid command reference")
		}
		if r.Operation == "command.import-ack" {
			return nil, s.ImportCommand(ctx, r.Producer, ref.ID, ref.Hash)
		}
		if ref.Hash != "" {
			return nil, fmt.Errorf("unexpected hash")
		}
		if r.Operation == "command.grant" {
			return s.GrantCommand(ctx, r.Producer, ref.ID)
		}
		var v interaction.View
		err := s.transaction(ctx, func(tx *sql.Tx) error {
			var err error
			v, err = readCommand(tx, ref.ID)
			if err != nil {
				return err
			}
			if v.Command.Producer != r.Producer {
				return fmt.Errorf("producer mismatch")
			}
			return expireCommand(tx, &v, s.now())
		})
		return v, err
	case "command.result":
		var result interaction.Result
		if err := decode(&result); err != nil {
			return nil, err
		}
		return s.SubmitCommandResult(ctx, r.Producer, result)
	case "command.result-ack":
		var receipt interaction.ResultReceipt
		if err := decode(&receipt); err != nil {
			return nil, err
		}
		return nil, s.AckCommandResult(ctx, r.Producer, receipt)
	case "command.non-admission":
		var proof interaction.NonAdmissionProof
		if err := decode(&proof); err != nil {
			return nil, err
		}
		return s.ReconcileCommand(ctx, r.Producer, proof)
	default:
		return nil, fmt.Errorf("unknown command operation")
	}
}
