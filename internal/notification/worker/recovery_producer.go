package worker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func OpenRecoveryProducerSession(ctx context.Context, path string, c Config) (protocol.RecoveryProducerSession, error) {
	s, err := OpenRecoveryGenerationSession(ctx, path, c)
	if err != nil {
		return nil, err
	}
	return s.(*recoveryReconciler), nil
}

// Read-only evidence for a source's atomic delivery-ledger commit. The caller
// retains this offline session; no receipt is listed/acked and no job is enabled.
func (r *recoveryReconciler) PrepareRecoveryProducerCommit(ctx context.Context, p protocol.Producer) (proof protocol.RecoveryProducerCommit, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	lease, check, err := r.lockRecoveryProducer(ctx, p)
	if err != nil {
		return proof, err
	}
	defer lease.Close()
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		if err := check(); err != nil {
			return err
		}
		hash, err := recoverySummaryHash(tx, p)
		if err != nil {
			return err
		}
		generation, err := r.readRecoveryGeneration(p, hash)
		if err != nil {
			return err
		}
		var raw string
		if err = tx.QueryRow("SELECT completion FROM reconciliation_producers WHERE producer=?", p.ID).Scan(&raw); err != nil {
			return err
		}
		var summary protocol.RecoverySummary
		if err = protocol.Decode([]byte(raw), &summary, protocol.MaxRequest+8192); err != nil {
			return err
		}
		var epoch string
		var submitted uint64
		var listed, acked int64
		if err = tx.QueryRow("SELECT epoch,submitted,listed,acked FROM producers WHERE id=?", p.ID).Scan(&epoch, &submitted, &listed, &acked); err != nil {
			return err
		}
		if epoch != p.Epoch || submitted != summary.Manifest.HighestIssued || listed != 0 || acked != 0 || summary.UnknownFirst != 0 || summary.UnknownLast != 0 {
			return fmt.Errorf("recovery producer stream is not ready for a zero-cursor commit")
		}
		proof = protocol.RecoveryProducerCommit{Version: 1, Generation: generation, HighestIssued: submitted, Cursor: protocol.Cursor{Identity: r.journal.After, Producer: p}}
		interactive := false
		if r.config.Interaction != nil {
			for _, w := range r.config.Interaction.Workers {
				if w.Producer != p {
					continue
				}
				interactive = true
				s, err := readCommandRecoverySession(tx, p)
				if err != nil {
					return err
				}
				if s.Summary == nil || s.Manifest == nil {
					return protocol.ErrRecoveryIncomplete
				}
				v := s.Summary
				if v.Challenge.Operation != r.journal.Operation || v.Challenge.Identity != r.journal.After || v.Challenge.Producer != p ||
					protocol.Hash(v.Challenge) != protocol.Hash(s.Challenge) || v.Manifest != *s.Manifest || v.Manifest.Identity != r.journal.Before || v.Manifest.Producer != p ||
					v.ExecutionEnabled || v.Terminal > v.Manifest.Commands || v.Uncertain != v.Manifest.Commands-v.Terminal {
					return fmt.Errorf("command reconciliation changed before producer commit")
				}
				if err = validateCommandCommitRecords(tx, p, *v); err != nil {
					return err
				}
				proof.CommandSummaryHash = protocol.Hash(v)
			}
		}
		if !interactive {
			if err = requireRecoveryCommandsAbsent(tx, p); err != nil {
				return err
			}
		}
		if err = proof.Validate(); err != nil {
			return err
		}
		return check()
	})
	return
}

func requireRecoveryCommandsAbsent(tx *sql.Tx, p protocol.Producer) error {
	var count int
	if err := tx.QueryRow("SELECT (SELECT count(*) FROM commands WHERE producer=?)+(SELECT count(*) FROM recovery_commands WHERE producer=?)", p.ID, p.ID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("retained commands require explicit command recovery")
	}
	var tables, markers int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('reconciliation_command_sessions','reconciliation_command_records')").Scan(&tables); err != nil {
		return err
	}
	if err := tx.QueryRow("SELECT count(*) FROM metadata WHERE key='command_reconciliation_schema'").Scan(&markers); err != nil {
		return err
	}
	if tables != 0 || markers != 0 {
		var version string
		if tables != 2 || markers != 1 {
			return fmt.Errorf("incomplete command recovery history")
		}
		if err := tx.QueryRow("SELECT value FROM metadata WHERE key='command_reconciliation_schema'").Scan(&version); err != nil {
			return err
		}
		if version != "1" {
			return fmt.Errorf("unknown command recovery schema")
		}
		if err := tx.QueryRow("SELECT (SELECT count(*) FROM reconciliation_command_sessions WHERE producer=?)+(SELECT count(*) FROM reconciliation_command_records WHERE producer=?)", p.ID, p.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("retained command recovery session requires reconciliation")
		}
	}
	exists, err := commandInstallationSchema(tx)
	if err != nil {
		return err
	}
	if exists {
		if err = tx.QueryRow("SELECT count(*) FROM reconciliation_command_installations WHERE producer=?", p.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("retained command installation requires reconciliation")
		}
	}
	return nil
}

func validateCommandCommitRecords(tx *sql.Tx, p protocol.Producer, summary interaction.CommandRecoverySummary) error {
	var schema string
	if err := tx.QueryRow("SELECT value FROM metadata WHERE key='command_reconciliation_schema'").Scan(&schema); err != nil {
		return err
	}
	if schema != "1" {
		return fmt.Errorf("unknown command recovery schema")
	}
	rows, err := tx.Query("SELECT ordinal,hash,body,outcome FROM reconciliation_command_records WHERE producer=? ORDER BY ordinal", p.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var hashes []string
	var results, terminal, uncertain uint64
	for rows.Next() {
		var n uint64
		var hash, body, outcome string
		if err = rows.Scan(&n, &hash, &body, &outcome); err != nil {
			return err
		}
		var record interaction.CommandRecoveryRecord
		if err = protocol.Decode([]byte(body), &record, protocol.MaxRequest); err != nil {
			return err
		}
		if err = record.Validate(summary.Manifest); err != nil {
			return err
		}
		expected, _, e := recoveryCommandOutcome(tx, record)
		if e != nil {
			return e
		}
		if outcome != expected {
			return fmt.Errorf("command recovery outcome differs from retained evidence")
		}

		if n != uint64(len(hashes))+1 || hash != protocol.Hash(record) {
			return fmt.Errorf("command recovery commit evidence damaged")
		}
		hashes = append(hashes, hash)
		results += uint64(len(record.Results))
		if interaction.Terminal(outcome) {
			terminal++
		} else {
			uncertain++
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	var missing uint64
	if err = tx.QueryRow("SELECT count(*) FROM recovery_commands c WHERE producer=? AND NOT EXISTS(SELECT 1 FROM reconciliation_command_records r WHERE r.id=c.id)", p.ID).Scan(&missing); err != nil {
		return err
	}
	if uint64(len(hashes)) != summary.Manifest.Commands || results != summary.Manifest.Results || interaction.CommandRecoveryDigest(hashes) != summary.Manifest.Digest ||
		terminal != summary.Terminal || uncertain != summary.Uncertain || missing != summary.MissingRestored {
		return fmt.Errorf("command recovery commit evidence differs from completion")
	}
	return nil
}
