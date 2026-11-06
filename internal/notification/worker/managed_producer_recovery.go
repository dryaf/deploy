package worker

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// RequireRecoveredProducer verifies a producer's retained handoff against the
// immutable publication checkpoint. Operational cursors may already have moved;
// they cannot substitute for the zero-cursor handoff recorded before publication.
func (s *Store) RequireRecoveredProducer(ctx context.Context, proof protocol.RecoveryProducerCommit) error {
	p := proof.Cursor.Producer
	g := proof.Generation
	o := g.Ownership
	if proof.Validate() != nil || s.installationRevision == "" || s.recoveryCommitHash == "" || proof.Cursor.Identity != s.identity || o.Operation != s.recoveryProof.Operation || o.Before != s.recoveryProof.Before || o.After != s.identity {
		return fmt.Errorf("producer handoff does not match managed recovery")
	}
	if err := s.ownership(p, func(current *ownership) error {
		if err := s.verifyOwnershipPrefix(*current); err != nil {
			return err
		}
		if err := s.checkOwnershipHistoryTail(p, current.Revision); err != nil {
			return err
		}
		var stage, migration ownershipTransition
		if err := s.dir.ReadJSON(ownershipHistoryPath(p, g.Revision), &stage); err != nil {
			return err
		}
		if err := s.dir.ReadJSON(ownershipHistoryPath(p, o.Revision), &migration); err != nil {
			return err
		}
		if current.Identity != s.identity || current.Revision < g.Revision || current.Generations[g.Generation] == "" || stage.Operation != "stage" || stage.Generation != g.Generation || stage.Previous != o.History || protocol.Hash(stage) != g.History || protocol.Hash(migration) != o.History || migration.Recovery == nil || migration.Recovery.SummaryHash != o.SummaryHash || migration.Recovery.Binding.Operation != o.Operation || migration.Recovery.Binding.Before != o.Before || migration.Recovery.Binding.After != o.After {
			return fmt.Errorf("recovered producer lost ownership provenance")
		}
		return nil
	}); err != nil {
		return err
	}
	return withRetainedCheckpoint(ctx, s.dir, s.lease, s.recoveryProof.Operation, s.recoveryProof.ReviewHash, func(stage, d *safefs.Directory, check func() error) error {
		retained, err := readRetainedCheckpoint(ctx, stage, d, s.config, s.recoveryProof.Operation, s.recoveryProof.ReviewHash, s.recoveryProof.CheckpointHash)
		if err != nil {
			return err
		}
		if retained != s.recoveryProof {
			return fmt.Errorf("producer checkpoint changed")
		}
		db, err := immutableOriginalDB(d, checkpointDatabase)
		if err != nil {
			return err
		}
		defer db.Close()
		var raw string
		if err = db.QueryRowContext(ctx, "SELECT completion FROM reconciliation_producers WHERE producer=?", p.ID).Scan(&raw); err != nil {
			return err
		}
		var summary protocol.RecoverySummary
		if err = protocol.Decode([]byte(raw), &summary, protocol.MaxRequest+8192); err != nil {
			return err
		}
		if protocol.Hash(summary) != o.SummaryHash || summary.Manifest.HighestIssued != proof.HighestIssued || summary.Challenge.Producer != p {
			return fmt.Errorf("producer recovery summary differs from source commit")
		}
		if proof.CommandSummaryHash != "" {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			commands, err := readCommandRecoverySession(tx, p)
			if err != nil {
				return err
			}
			if commands.Summary == nil || protocol.Hash(commands.Summary) != proof.CommandSummaryHash {
				return fmt.Errorf("producer command handoff changed")
			}
			if err = validateCommandCommitRecords(tx, p, *commands.Summary); err != nil {
				return err
			}
		} else {
			// A caller cannot omit an interactive stream from its source proof.
			if s.config.Interaction != nil {
				for _, w := range s.config.Interaction.Workers {
					if w.Producer == p {
						return fmt.Errorf("producer command handoff missing")
					}
				}
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err = requireRecoveryCommandsAbsent(tx, p); err != nil {
				return err
			}
		}
		return check()
	})
}
