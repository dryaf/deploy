package interaction

import (
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// ValidateDeliveryRecoveryCommit checks the independently fenced command ledger
// in the source's delivery commit transaction. It changes no execution identity,
// request mapping, result cursor or fence. Absence is accepted only on both sides.
func ValidateDeliveryRecoveryCommit(tx *sql.Tx, proof protocol.RecoveryProducerCommit) error {
	if err := proof.Validate(); err != nil {
		return err
	}
	var tables int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE 'interactive_%'").Scan(&tables); err != nil {
		return err
	}
	if tables == 0 && proof.CommandSummaryHash == "" {
		return nil
	}
	if tables == 0 || proof.CommandSummaryHash == "" {
		return fmt.Errorf("command recovery evidence missing on one side")
	}
	o := proof.Generation.Ownership
	l := &Ledger{producer: o.Producer, identity: o.Before}
	saved, err := l.readRecovery(tx)
	if err != nil {
		return err
	}
	var raw string
	if err = tx.QueryRow("SELECT body FROM interactive_recovery_completion WHERE singleton=1").Scan(&raw); err != nil {
		return err
	}
	var summary CommandRecoverySummary
	if err = protocol.Decode([]byte(raw), &summary, 16384); err != nil {
		return err
	}
	if protocol.Hash(summary) != proof.CommandSummaryHash || summary.ExecutionEnabled || protocol.Hash(summary.Challenge) != protocol.Hash(saved.Challenge) ||
		summary.Manifest != saved.Manifest || summary.Challenge.Operation != o.Operation || summary.Challenge.Identity != o.After || summary.Challenge.Producer != o.Producer ||
		summary.Terminal > summary.Manifest.Commands || summary.Uncertain != summary.Manifest.Commands-summary.Terminal {
		return fmt.Errorf("command recovery completion differs from delivery commit")
	}
	// Validate the retained pages, not just their row count: partial corruption
	// must not turn a completed summary into a reusable commit authority.
	rows, err := tx.Query("SELECT ordinal,hash,body FROM interactive_recovery_records ORDER BY ordinal")
	if err != nil {
		return err
	}
	defer rows.Close()
	var hashes []string
	var ordinal, results uint64
	for rows.Next() {
		var n uint64
		var hash, body string
		if err = rows.Scan(&n, &hash, &body); err != nil {
			return err
		}
		var record CommandRecoveryRecord
		if err = protocol.Decode([]byte(body), &record, protocol.MaxRequest); err != nil {
			return err
		}
		if err = record.Validate(saved.Manifest); err != nil {
			return err
		}
		ordinal++
		if n != ordinal || hash != protocol.Hash(record) {
			return fmt.Errorf("command recovery evidence damaged")
		}
		hashes = append(hashes, hash)
		results += uint64(len(record.Results))
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if ordinal != saved.Manifest.Commands || results != saved.Manifest.Results || CommandRecoveryDigest(hashes) != saved.Manifest.Digest {
		return fmt.Errorf("command recovery evidence no longer matches completion")
	}
	return nil
}
