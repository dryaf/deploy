package interaction

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// CommitCommandRecovery changes only the specialist ledger's identity and
// reconciliation floors. Callers retain the offline gateway session throughout.
// Reopen using the new identity afterwards; both old and new handles stay fenced.
func CommitCommandRecovery(ctx context.Context, l *Ledger, s CommandRecoveryCommitSession) (CommandRecoveryCommit, error) {
	return commitCommandRecovery(ctx, l, s, nil)
}

func commitCommandRecovery(ctx context.Context, l *Ledger, s CommandRecoveryCommitSession, fault func(string) error) (proof CommandRecoveryCommit, err error) {
	if l == nil || s == nil {
		return proof, fmt.Errorf("command recovery commit dependencies missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	boundary := func(at string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fault != nil {
			return fault(at)
		}
		return nil
	}
	transaction := func(fn func(*sql.Tx) error) error {
		tx, err := l.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err = fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	var summary CommandRecoverySummary
	err = transaction(func(tx *sql.Tx) error {
		v, _, err := l.commandCommitEvidence(tx)
		if err != nil {
			return err
		}
		summary = v
		intent, _, err := readLedgerCommandCommit(tx)
		if err != nil {
			return err
		}
		if intent == nil {
			if _, err = tx.Exec(`CREATE TABLE interactive_recovery_commit(singleton INTEGER PRIMARY KEY CHECK(singleton=1),version INTEGER NOT NULL,intent TEXT NOT NULL,proof TEXT NOT NULL);
CREATE TABLE interactive_recovery_result_floors(id TEXT PRIMARY KEY,revision INTEGER NOT NULL,command_hash TEXT NOT NULL,record_hash TEXT NOT NULL);`); err != nil {
				return err
			}
			raw, _ := json.Marshal(summary)
			if _, err = tx.Exec("INSERT INTO interactive_recovery_commit VALUES(1,1,?,'')", string(raw)); err != nil {
				return err
			}
		} else if protocol.Hash(intent) != protocol.Hash(summary) {
			return fmt.Errorf("command recovery commit intent changed")
		}
		return boundary("command_commit_intent_transaction")
	})
	if err != nil {
		return
	}
	if err = boundary("command_commit_intent"); err != nil {
		return
	}
	// No local transaction spans either gateway call.
	remote, err := s.CommandSummary(ctx, l.producer)
	if err != nil {
		return proof, err
	}
	if protocol.Hash(remote) != protocol.Hash(summary) {
		return proof, fmt.Errorf("gateway command completion changed")
	}
	proof, err = s.PrepareCommandRecoveryCommit(ctx, l.producer)
	if err != nil {
		return
	}
	if err = proof.Validate(); err != nil {
		return
	}
	if protocol.Hash(proof.Summary) != protocol.Hash(summary) {
		return proof, fmt.Errorf("gateway command commit summary changed")
	}
	if err = boundary("command_commit_gateway_prepared"); err != nil {
		return
	}
	err = transaction(func(tx *sql.Tx) error {
		current, floors, err := l.commandCommitEvidence(tx)
		if err != nil {
			return err
		}
		if protocol.Hash(current) != protocol.Hash(summary) || floors != proof.ResultFloorsHash {
			return fmt.Errorf("command recovery authority changed during commit")
		}
		intent, previous, err := readLedgerCommandCommit(tx)
		if err != nil {
			return err
		}
		if intent == nil || protocol.Hash(intent) != protocol.Hash(summary) {
			return fmt.Errorf("command commit intent missing or changed")
		}
		if previous != nil {
			if protocol.Hash(previous) != protocol.Hash(proof) {
				return fmt.Errorf("command recovery checkpoint changed")
			}
			return nil
		}
		rows, err := tx.Query("SELECT body FROM interactive_recovery_records ORDER BY ordinal")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var body string
			if err = rows.Scan(&body); err != nil {
				return err
			}
			var v CommandRecoveryRecord
			if err = protocol.Decode([]byte(body), &v, protocol.MaxRequest); err != nil {
				return err
			}
			f := RecoveryResultFloor(v)
			if _, err = tx.Exec("INSERT INTO interactive_recovery_result_floors VALUES(?,?,?,?)", f.CommandID, f.Revision, f.CommandHash, f.RecordHash); err != nil {
				return err
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE interactive_inbox SET fenced=1"); err != nil {
			return err
		}
		raw, _ := json.Marshal(proof)
		if _, err = tx.Exec("UPDATE interactive_recovery_commit SET proof=? WHERE singleton=1", string(raw)); err != nil {
			return err
		}
		pin, _ := json.Marshal(ledgerIdentityPin{Producer: l.producer, Identity: summary.Challenge.Identity, RecoveryOperation: summary.Challenge.Operation, RecoveryCommit: protocol.Hash(proof)})
		if _, err = tx.Exec("UPDATE interactive_identity SET body=? WHERE singleton=1", string(pin)); err != nil {
			return err
		}
		return boundary("command_commit_transaction")
	})
	if err == nil {
		err = boundary("command_commit")
	}
	return
}

func (l *Ledger) commandCommitEvidence(tx *sql.Tx) (summary CommandRecoverySummary, floors string, err error) {
	saved, err := l.readRecovery(tx)
	if err != nil {
		return summary, "", err
	}
	var body string
	if err = tx.QueryRow("SELECT body FROM interactive_recovery_completion WHERE singleton=1").Scan(&body); err != nil {
		return
	}
	if err = protocol.Decode([]byte(body), &summary, 16384); err != nil {
		return
	}
	if protocol.Hash(summary.Challenge) != protocol.Hash(saved.Challenge) || summary.Manifest != saved.Manifest {
		return summary, "", fmt.Errorf("command completion changed")
	}
	shape := CommandRecoveryCommit{Version: 1, Summary: summary, ResultFloorsHash: CommandRecoveryDigest(nil)}
	if err = shape.Validate(); err != nil {
		return
	}
	_, previous, err := readLedgerCommandCommit(tx)
	if err != nil {
		return summary, "", err
	}
	var commands, results, orphans uint64
	if err = tx.QueryRow(`SELECT (SELECT count(*) FROM interactive_inbox),(SELECT count(*) FROM interactive_results),
(SELECT count(*) FROM interactive_non_admission n WHERE NOT EXISTS(SELECT 1 FROM interactive_inbox i WHERE i.id=n.id))`).Scan(&commands, &results, &orphans); err != nil {
		return
	}
	if commands != saved.Manifest.Commands || results != saved.Manifest.Results || orphans != 0 {
		return summary, "", fmt.Errorf("command ledger membership changed")
	}
	rows, err := tx.Query("SELECT ordinal,id,hash,body FROM interactive_recovery_records ORDER BY ordinal")
	if err != nil {
		return summary, "", err
	}
	defer rows.Close()
	var records, floorHashes []string
	var resultCount uint64
	for rows.Next() {
		var n uint64
		var id, hash, raw string
		if err = rows.Scan(&n, &id, &hash, &raw); err != nil {
			return
		}
		var v CommandRecoveryRecord
		if err = protocol.Decode([]byte(raw), &v, protocol.MaxRequest); err != nil {
			return
		}
		if err = v.Validate(saved.Manifest); err != nil {
			return
		}
		if n != uint64(len(records))+1 || id != v.Command.ID || hash != protocol.Hash(v) {
			return summary, "", fmt.Errorf("command recovery history changed")
		}
		if err = validateCommandProjection(tx, v, previous != nil); err != nil {
			return
		}
		records = append(records, hash)
		floorHashes = append(floorHashes, protocol.Hash(RecoveryResultFloor(v)))
		resultCount += uint64(len(v.Results))
	}
	if err = rows.Err(); err != nil {
		return
	}
	if uint64(len(records)) != saved.Manifest.Commands || resultCount != saved.Manifest.Results || CommandRecoveryDigest(records) != saved.Manifest.Digest {
		return summary, "", fmt.Errorf("command recovery history incomplete")
	}
	return summary, CommandRecoveryDigest(floorHashes), nil
}
