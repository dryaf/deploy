package interaction

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type PendingResult struct {
	Result  Result
	Receipt *ResultReceipt
}

// PendingResults also includes imported receipts whose final gateway ACK was
// lost. Replaying that ACK cannot repeat a domain operation. Recovery fences the
// read itself so a runner cannot transmit historical results before rejecting ACKs.
func (l *Ledger) PendingResults(ctx context.Context) (out []PendingResult, err error) {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = l.requireCommandLedgerActive(tx); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT body,receipt FROM interactive_results WHERE acked=0 ORDER BY id,revision LIMIT 32")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bytes := 0
	for rows.Next() {
		var body, receipt string
		if err = rows.Scan(&body, &receipt); err != nil {
			return nil, err
		}
		if bytes+len(body)+len(receipt) > protocol.MaxResponse-2048 {
			break
		}
		bytes += len(body) + len(receipt)
		var p PendingResult
		if err = protocol.Decode([]byte(body), &p.Result, protocol.MaxRequest); err != nil {
			return nil, err
		}
		if receipt != "" {
			p.Receipt = &ResultReceipt{}
			if err = protocol.Decode([]byte(receipt), p.Receipt, 8192); err != nil {
				return nil, err
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (l *Ledger) Acknowledged(ctx context.Context, receipt ResultReceipt) error {
	if receipt.Identity != l.identity || receipt.Producer != l.producer {
		return fmt.Errorf("receipt identity mismatch")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = l.requireCommandLedgerActive(tx); err != nil {
		return err
	}
	var body string
	if err = tx.QueryRow("SELECT receipt FROM interactive_results WHERE id=? AND revision=?", receipt.CommandID, receipt.Revision).Scan(&body); err != nil {
		return err
	}
	var stored ResultReceipt
	if err = protocol.Decode([]byte(body), &stored, 8192); err != nil {
		return err
	}
	if stored != receipt {
		return fmt.Errorf("receipt changed")
	}
	if _, err = tx.Exec("UPDATE interactive_results SET acked=1 WHERE id=? AND revision=?", receipt.CommandID, receipt.Revision); err != nil {
		return err
	}
	return tx.Commit()
}

func (l *Ledger) Latest(ctx context.Context, id string) (r Result, err error) {
	var body string
	err = l.db.QueryRowContext(ctx, "SELECT r.body FROM interactive_results r JOIN interactive_inbox i ON i.id=r.id AND i.revision=r.revision WHERE i.id=?", id).Scan(&body)
	if err == nil {
		err = protocol.Decode([]byte(body), &r, protocol.MaxRequest)
	}
	return
}
