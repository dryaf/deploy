package interaction

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// RejectPreparation records a permanent domain refusal before any grant or
// mutation. Storage/transport errors must remain retryable and never call this.
func (l *Ledger) RejectPreparation(ctx context.Context, c Command, text string) (r Result, err error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	if err = l.requireCommandLedgerActive(tx); err != nil {
		return r, err
	}
	var hash, phase, state string
	var revision uint64
	if err = tx.QueryRow("SELECT hash,phase,state,revision FROM interactive_inbox WHERE id=?", c.ID).Scan(&hash, &phase, &state, &revision); err != nil {
		return
	}
	if c.Producer != l.producer || hash != protocol.Hash(c) || !c.Descriptor.Confirm {
		return r, fmt.Errorf("preparation not applicable")
	}
	if phase != "" {
		var body string
		if err = tx.QueryRow("SELECT body FROM interactive_results WHERE id=? AND revision=?", c.ID, revision).Scan(&body); err == nil {
			err = protocol.Decode([]byte(body), &r, protocol.MaxResponse)
		}
		return
	}
	if Terminal(state) {
		return r, fmt.Errorf("command already closed")
	}
	r = Result{CommandID: c.ID, CommandHash: hash, Revision: 1, Phase: "prepare", State: "rejected", Scope: c.Scope, Text: text}
	if err = storeLedgerResult(tx, r); err != nil {
		return
	}
	if _, err = tx.Exec("UPDATE interactive_inbox SET phase='prepare',state='rejected',revision=1,fenced=1 WHERE id=?", c.ID); err != nil {
		return
	}
	err = tx.Commit()
	return
}
