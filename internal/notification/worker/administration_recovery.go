package worker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const administrativeKeysPredicate = "key LIKE 'message-decision-v1:%' OR key LIKE 'message-decision-archive-v1:%'"

func archivedMessageDecisionKey(v MessageDecisionRecord) string {
	return "message-decision-archive-v1:" + protocol.Hash(v.Decision.Review.Identity) + ":" + v.Decision.Review.ID
}

func administrativeKeys(ctx context.Context, q ledJournalReader) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT key FROM metadata WHERE "+administrativeKeysPredicate+" ORDER BY key LIMIT ?", maxMessageDecisions+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	seen := map[string]bool{}
	for rows.Next() {
		var key string
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		id := key[strings.LastIndex(key, ":")+1:]
		if !safefs.ValidID(id, "n") || seen[id] {
			return nil, fmt.Errorf("duplicate or invalid administrative key")
		}
		seen[id] = true
		keys = append(keys, key)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(keys) > maxMessageDecisions {
		return nil, fmt.Errorf("administrative history budget exceeded")
	}
	return keys, nil
}

func verifyMessageDecisionReceipt(ctx context.Context, q ledRowReader, v MessageDecisionRecord) error {
	var p, body string
	if err := q.QueryRowContext(ctx, "SELECT producer,body FROM receipts WHERE position=?", v.Receipt.Position).Scan(&p, &body); err != nil {
		return err
	}
	var receipt protocol.Receipt
	if err := protocol.Decode([]byte(body), &receipt, 8192); err != nil {
		return err
	}
	receipt.Position = v.Receipt.Position
	if p != v.Decision.Review.Job.Message.Producer.ID || protocol.Hash(receipt) != protocol.Hash(v.Receipt) {
		return fmt.Errorf("administrative completion receipt changed")
	}
	return nil
}

func verifyAdministrativeHistory(ctx context.Context, q ledJournalReader, identity protocol.Identity) error {
	keys, err := administrativeKeys(ctx, q)
	if err != nil {
		return err
	}
	for _, key := range keys {
		var body string
		if err = q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&body); err != nil {
			return err
		}
		var v MessageDecisionRecord
		if err = protocol.Decode([]byte(body), &v, 2*protocol.MaxRequest); err != nil {
			return err
		}
		if err = v.Validate(); err != nil {
			return err
		}
		if v.Decision.Review.Identity.Worker != identity.Worker {
			return fmt.Errorf("administrative audit belongs to another worker")
		}
		if strings.HasPrefix(key, "message-decision-v1:") {
			if key != messageDecisionKey(v.Decision.Review.ID) || v.Decision.Review.Identity != identity {
				return fmt.Errorf("active administrative audit epoch mismatch")
			}
			if err = verifyMessageDecisionReceipt(ctx, q, v); err != nil {
				return err
			}
		} else if key != archivedMessageDecisionKey(v) || v.Decision.Review.Identity == identity {
			return fmt.Errorf("archived administrative audit epoch mismatch")
		}
	}
	return nil
}

// Restore moves completed decisions into old-epoch audit before replacing the
// receipt stream. Their full original receipts remain embedded. Neither an old
// retry instruction nor its position can authorize work or ACK the new epoch.
func archiveAdministrativeDecisions(ctx context.Context, tx *sql.Tx, before protocol.Identity) error {
	if err := verifyAdministrativeHistory(ctx, tx, before); err != nil {
		return err
	}
	keys, err := administrativeKeys(ctx, tx)
	if err != nil {
		return err
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, "message-decision-v1:") {
			continue
		}
		var body string
		if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&body); err != nil {
			return err
		}
		var v MessageDecisionRecord
		if err = protocol.Decode([]byte(body), &v, 2*protocol.MaxRequest); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?)", archivedMessageDecisionKey(v), body); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM metadata WHERE key=?", key); err != nil {
			return err
		}
	}
	return nil
}
