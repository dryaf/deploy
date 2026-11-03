package interaction

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// OpenRecoveryLedger opens only an existing specialist ledger under one exact
// recovery binding. It accepts the old identity before commit or the new identity
// with its committed proof, never treating an arbitrary open failure as permission
// to retry under another identity. The caller holds its stable execution lease.
func OpenRecoveryLedger(ctx context.Context, db *sql.DB, p protocol.Producer, before, after protocol.Identity, operation string) (*Ledger, error) {
	if db == nil || !p.Valid() || !before.Valid() || !after.Valid() || before.Worker != after.Worker || before == after || !safefs.ValidID(operation, "n") {
		return nil, fmt.Errorf("invalid command recovery binding")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRow("SELECT body FROM interactive_identity WHERE singleton=1").Scan(&raw); err != nil {
		return nil, err
	}
	var pin ledgerIdentityPin
	if err = protocol.Decode([]byte(raw), &pin, 4096); err != nil {
		return nil, err
	}
	if pin.Producer != p || (pin.Identity != before && pin.Identity != after) || (pin.RecoveryOperation != "" && pin.RecoveryOperation != operation) {
		return nil, fmt.Errorf("command ledger belongs to another recovery")
	}
	l := &Ledger{db: db, producer: p, identity: pin.Identity}
	if err = l.checkRecoveryPin(tx); err != nil {
		return nil, err
	}
	if pin.RecoveryOperation == "" {
		var artifacts int
		if err = tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN
('interactive_recovery','interactive_recovery_records','interactive_recovery_completion','interactive_recovery_commit','interactive_recovery_result_floors')`).Scan(&artifacts); err != nil {
			return nil, err
		}
		if pin.Identity != before || pin.RecoveryCommit != "" || artifacts != 0 {
			return nil, fmt.Errorf("command recovery authority missing")
		}
	} else {
		saved, err := l.readRecovery(tx)
		if err != nil {
			return nil, err
		}
		if saved.Manifest.Identity != before || saved.Manifest.Producer != p || saved.Challenge.Operation != operation || saved.Challenge.Identity != after || saved.Challenge.Producer != p {
			return nil, fmt.Errorf("command recovery export binding changed")
		}
		_, proof, err := readLedgerCommandCommit(tx)
		if err != nil {
			return nil, err
		}
		if pin.Identity == after && proof == nil {
			return nil, fmt.Errorf("new command identity lacks a committed recovery proof")
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return l, nil
}
