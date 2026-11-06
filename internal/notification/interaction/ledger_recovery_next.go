package interaction

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type commandRecoveryAncestor struct {
	Version   int
	Before    ledgerIdentityPin
	Operation string
	After     protocol.Identity
	Capture   ledgerRecovery
	Commit    CommandRecoveryCommit
	Records   []CommandRecoveryRecord
}

func readLedgerPin(tx *sql.Tx) (p ledgerIdentityPin, err error) {
	var raw string
	if err = tx.QueryRow("SELECT body FROM interactive_identity WHERE singleton=1").Scan(&raw); err != nil {
		return
	}
	err = protocol.Decode([]byte(raw), &p, 4096)
	return
}

// Each epoch retains its original receipts and result floors. The linked hashes
// are part of the identity pin, so losing history never falls back to a new ledger.
func readCommandAncestors(tx *sql.Tx, pin ledgerIdentityPin) ([]commandRecoveryAncestor, error) {
	var out []commandRecoveryAncestor
	hash := pin.Ancestor
	for hash != "" {
		if len(out) >= 32 || !safefs.DigestPattern.MatchString(hash) {
			return nil, fmt.Errorf("invalid command recovery ancestry")
		}
		var raw string
		if err := tx.QueryRow("SELECT body FROM interactive_recovery_ancestors WHERE hash=?", hash).Scan(&raw); err != nil {
			return nil, err
		}
		var a commandRecoveryAncestor
		if err := protocol.Decode([]byte(raw), &a, 40<<20); err != nil {
			return nil, err
		}
		if a.Version != 1 || a.Capture.Version != 1 || a.Before.RecoveryOperation != a.Commit.Summary.Challenge.Operation || protocol.Hash(a) != hash || a.Commit.Validate() != nil || a.Before.RecoveryCommit != protocol.Hash(a.Commit) || a.Before.Identity != a.Commit.Summary.Challenge.Identity || a.Before.Producer != pin.Producer || a.Capture.Manifest != a.Commit.Summary.Manifest || a.Capture.Challenge != a.Commit.Summary.Challenge || !safefs.ValidID(a.Operation, "n") || !a.After.Valid() || a.After.Worker != a.Before.Identity.Worker || a.After == a.Before.Identity {
			return nil, fmt.Errorf("command ancestor proof changed")
		}
		var records, floors []string
		for _, v := range a.Records {
			if err := v.Validate(a.Capture.Manifest); err != nil {
				return nil, err
			}
			records = append(records, protocol.Hash(v))
			floors = append(floors, protocol.Hash(RecoveryResultFloor(v)))
		}
		if uint64(len(records)) != a.Capture.Manifest.Commands || CommandRecoveryDigest(records) != a.Capture.Manifest.Digest || CommandRecoveryDigest(floors) != a.Commit.ResultFloorsHash {
			return nil, fmt.Errorf("command ancestor inventory changed")
		}
		if len(out) > 0 && out[len(out)-1].Capture.Manifest.Identity != a.Before.Identity {
			return nil, fmt.Errorf("command ancestor epoch discontinuity")
		}
		out = append(out, a)
		hash = a.Before.Ancestor
	}
	if len(out) > 0 {
		a := out[0]
		if pin.NextOperation != "" {
			if pin.Identity != a.Before.Identity || pin.NextOperation != a.Operation || pin.NextIdentity == nil || *pin.NextIdentity != a.After {
				return nil, fmt.Errorf("pending command ancestor binding changed")
			}
		} else if pin.RecoveryCommit != "" {
			if pin.Identity != a.After || pin.RecoveryOperation != a.Operation {
				return nil, fmt.Errorf("committed command ancestor binding changed")
			}
		} else if pin.Identity != a.Before.Identity || pin.RecoveryOperation != a.Operation {
			return nil, fmt.Errorf("captured command ancestor binding changed")
		}
	}
	return out, nil
}

func attachCommandAncestry(tx *sql.Tx, pin ledgerIdentityPin, v *CommandRecoveryRecord) error {
	ancestors, err := readCommandAncestors(tx, pin)
	if err != nil {
		return err
	}
	if len(ancestors) == 0 {
		return nil
	}
	a := ancestors[0]
	for _, old := range a.Records {
		if old.Command.ID != v.Command.ID {
			continue
		}
		if err := validateCommandProjection(tx, old, true); err != nil {
			return err
		}
		origin := a.Capture.Manifest.Identity
		if old.HistoricalIdentity != nil {
			origin = *old.HistoricalIdentity
		}
		v.HistoricalIdentity = &origin
		return nil
	}
	return nil
}

// OpenNextRecoveryLedger is explicitly for a previously committed recovery.
// The caller holds the specialist's execution lease. One transaction archives
// the completed epoch, retires its working tables, and pins the next operation.
// Ordinary command admission remains fenced throughout, including before capture.
func OpenNextRecoveryLedger(ctx context.Context, db *sql.DB, p protocol.Producer, before, after protocol.Identity, operation string) (*Ledger, error) {
	return openNextRecoveryLedger(ctx, db, p, before, after, operation, nil)
}
func openNextRecoveryLedger(ctx context.Context, db *sql.DB, p protocol.Producer, before, after protocol.Identity, operation string, fault func(string) error) (*Ledger, error) {
	if db == nil || !p.Valid() || !before.Valid() || !after.Valid() || before.Worker != after.Worker || before == after || !safefs.ValidID(operation, "n") {
		return nil, fmt.Errorf("invalid next command recovery binding")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	pin, err := readLedgerPin(tx)
	if err != nil {
		return nil, err
	}
	if pin.Producer != p {
		return nil, fmt.Errorf("command recovery producer changed")
	}
	if pin.NextOperation == operation || pin.RecoveryOperation == operation {
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return OpenRecoveryLedger(ctx, db, p, before, after, operation)
	}
	l := &Ledger{db: db, producer: p, identity: before}
	if pin.Identity != before || pin.RecoveryCommit == "" {
		return nil, fmt.Errorf("next command recovery requires committed predecessor")
	}
	if err = l.checkRecoveryPin(tx); err != nil {
		return nil, err
	}
	saved, err := l.readRecovery(tx)
	if err != nil {
		return nil, err
	}
	summary, floors, err := l.commandCommitEvidence(tx)
	if err != nil {
		return nil, err
	}
	_, proof, err := readLedgerCommandCommit(tx)
	if err != nil {
		return nil, err
	}
	if proof == nil || protocol.Hash(summary) != protocol.Hash(proof.Summary) || floors != proof.ResultFloorsHash {
		return nil, fmt.Errorf("command predecessor checkpoint changed")
	}
	a := commandRecoveryAncestor{Version: 1, Before: pin, Operation: operation, After: after, Capture: saved, Commit: *proof}
	rows, err := tx.Query("SELECT body FROM interactive_recovery_records ORDER BY ordinal")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw string
		var v CommandRecoveryRecord
		if err = rows.Scan(&raw); err != nil {
			break
		}
		if err = protocol.Decode([]byte(raw), &v, protocol.MaxRequest); err != nil {
			break
		}
		a.Records = append(a.Records, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	if len(raw) > 40<<20 {
		return nil, fmt.Errorf("command ancestor exceeds capacity")
	}
	hash := protocol.Hash(a)
	if _, err = tx.Exec("CREATE TABLE IF NOT EXISTS interactive_recovery_ancestors(hash TEXT PRIMARY KEY,body TEXT NOT NULL)"); err != nil {
		return nil, err
	}
	if _, err = tx.Exec("INSERT INTO interactive_recovery_ancestors VALUES(?,?)", hash, string(raw)); err != nil {
		return nil, err
	}
	for _, name := range []string{"interactive_recovery_result_floors", "interactive_recovery_commit", "interactive_recovery_completion", "interactive_recovery_records", "interactive_recovery"} {
		if _, err = tx.Exec("DROP TABLE " + name); err != nil {
			return nil, err
		}
	}
	next := ledgerIdentityPin{Producer: p, Identity: before, Ancestor: hash, NextOperation: operation, NextIdentity: &after}
	raw, _ = json.Marshal(next)
	if _, err = tx.Exec("UPDATE interactive_identity SET body=? WHERE singleton=1", string(raw)); err != nil {
		return nil, err
	}
	if _, err = readCommandAncestors(tx, next); err != nil {
		return nil, err
	}
	if fault != nil {
		if err = fault("command_next_transaction"); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	if fault != nil {
		if err = fault("command_next_commit"); err != nil {
			return nil, err
		}
	}
	return OpenRecoveryLedger(ctx, db, p, before, after, operation)
}
