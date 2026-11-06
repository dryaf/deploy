package worker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Recovery keeps receipts under their original identity. A missing current
// receipt is never synthesized: require the exact independently retained
// producer record and eligible acceptance in the immutable publication image.
func (s *Store) requireHistoricalLegacyAcceptance(ctx context.Context, m protocol.Message) error {
	if s.recoveryCommitHash == "" || s.installationRevision == "" {
		return fmt.Errorf("retained canonical acceptance required before rollback")
	}
	return withRetainedCheckpoint(ctx, s.dir, s.lease, s.recoveryProof.Operation, s.recoveryProof.ReviewHash, func(stage, d *safefs.Directory, check func() error) error {
		proof, err := readRetainedCheckpoint(ctx, stage, d, s.config, s.recoveryProof.Operation, s.recoveryProof.ReviewHash, s.recoveryProof.CheckpointHash)
		if err != nil {
			return err
		}
		if proof != s.recoveryProof {
			return fmt.Errorf("legacy acceptance checkpoint changed")
		}
		db, err := immutableOriginalDB(d, checkpointDatabase)
		if err != nil {
			return err
		}
		defer db.Close()
		rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type='table' AND (name='reconciliation_records' OR name GLOB 'ancestor_*_reconciliation_records') ORDER BY name LIMIT 34")
		if err != nil {
			return err
		}
		var names []string
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			names = append(names, name)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(names) > 33 {
			return fmt.Errorf("legacy acceptance ancestor budget exceeded")
		}
		for _, name := range names {
			for _, ch := range name {
				if ch != '_' && (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') {
					return fmt.Errorf("invalid retained acceptance table")
				}
			}
			var raw, hash string
			err = db.QueryRowContext(ctx, "SELECT body,evidence_hash FROM \""+name+"\" WHERE producer=? AND id=?", m.Producer.ID, m.ID).Scan(&raw, &hash)
			if err != nil {
				if err == sql.ErrNoRows {
					continue
				}
				return err
			}
			var record protocol.RecoveryRecord
			if err = protocol.Decode([]byte(raw), &record, protocol.MaxRequest); err != nil {
				return err
			}
			if protocol.Hash(record) != hash || protocol.Hash(record.Message) != protocol.Hash(m) {
				return fmt.Errorf("retained legacy acceptance envelope changed")
			}
			if record.Receipt == nil {
				continue
			}
			r := *record.Receipt
			if r.Producer != m.Producer || !r.Identity.Valid() || r.Identity.Worker != s.identity.Worker || r.Receipt.EventID != m.ID || r.Receipt.Sequence != m.Sequence || r.Receipt.State != "provider_accepted" || r.Receipt.Acceptance == nil || !r.Receipt.Acceptance.Eligible {
				return fmt.Errorf("retained receipt does not prove eligible legacy acceptance")
			}
			table := strings.TrimSuffix(name, "records") + "receipts"
			var body, receiptHash, producer string
			if err = db.QueryRowContext(ctx, "SELECT producer,hash,body FROM \""+table+"\" WHERE position=?", r.Receipt.Position).Scan(&producer, &receiptHash, &body); err != nil {
				return err
			}
			var retained protocol.RetainedReceipt
			if err = protocol.Decode([]byte(body), &retained, protocol.MaxRequest); err != nil {
				return err
			}
			if producer != m.Producer.ID || receiptHash != protocol.Hash(r) || protocol.Hash(retained) != receiptHash {
				return fmt.Errorf("historical acceptance receipt changed")
			}
			return check()
		}
		return fmt.Errorf("no independently retained eligible acceptance for legacy rollback")
	})
}
