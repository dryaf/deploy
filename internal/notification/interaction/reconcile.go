package interaction

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// ProveNonAdmission closes the local admission gate in the same transaction as
// the evidence returned to the gateway. A delayed grant then remains unusable.
func (l *Ledger) ProveNonAdmission(ctx context.Context, c Command, g Grant) (proof NonAdmissionProof, err error) {
	if c.Producer != l.producer || g.Identity != l.identity || g.CommandID != c.ID || g.CommandHash != protocol.Hash(c) || !protocol.ValidToken(g.ID) {
		return proof, fmt.Errorf("invalid reconciliation request")
	}
	proof = NonAdmissionProof{Identity: l.identity, Producer: l.producer, CommandID: c.ID, CommandHash: protocol.Hash(c), GrantID: g.ID}
	proof.ID = protocol.Hash(proof)
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return proof, err
	}
	defer tx.Rollback()
	if err = l.requireCommandLedgerActive(tx); err != nil {
		return proof, err
	}
	var old string
	err = tx.QueryRow("SELECT body FROM interactive_non_admission WHERE id=?", c.ID).Scan(&old)
	if err == nil {
		var saved NonAdmissionProof
		if err = protocol.Decode([]byte(old), &saved, 8192); err != nil {
			return proof, err
		}
		if saved != proof {
			return proof, fmt.Errorf("reconciliation identity conflict")
		}
		return saved, nil
	}
	if err != sql.ErrNoRows {
		return proof, err
	}
	var hash, phase string
	if err = tx.QueryRow("SELECT hash,phase FROM interactive_inbox WHERE id=?", c.ID).Scan(&hash, &phase); err != nil {
		return proof, err
	}
	if hash != proof.CommandHash || phase == "execute" {
		return proof, fmt.Errorf("execution admitted or command changed")
	}
	if _, err = tx.Exec("UPDATE interactive_inbox SET fenced=1,state='expired' WHERE id=?", c.ID); err != nil {
		return proof, err
	}
	body, _ := json.Marshal(proof)
	if _, err = tx.Exec("INSERT INTO interactive_non_admission VALUES(?,?)", c.ID, string(body)); err != nil {
		return proof, err
	}
	err = tx.Commit()
	return
}
