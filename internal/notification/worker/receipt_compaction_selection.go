package worker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type compactableReceipt struct {
	Position int64
	Body     string
}

func administrativeReceiptPins(ctx context.Context, tx *sql.Tx, identity protocol.Identity) ([]int64, error) {
	if err := verifyAdministrativeHistory(ctx, tx, identity); err != nil {
		return nil, err
	}
	keys, err := administrativeKeys(ctx, tx)
	if err != nil {
		return nil, err
	}
	var pins []int64
	for _, key := range keys {
		if !strings.HasPrefix(key, "message-decision-v1:") {
			continue
		}
		var raw string
		if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&raw); err != nil {
			return nil, err
		}
		var record MessageDecisionRecord
		if err = protocol.Decode([]byte(raw), &record, 2*protocol.MaxRequest); err != nil {
			return nil, err
		}
		pins = append(pins, record.Receipt.Position)
	}
	slices.Sort(pins)
	return slices.Compact(pins), nil
}

func selectCompactableReceipts(ctx context.Context, tx *sql.Tx, p protocol.Producer, acked int64, cutoff time.Time, limit int, pins []int64) ([]compactableReceipt, error) {
	// Aggregate once per producer rather than performing a full receipt scan for
	// each candidate. Position order bounds the returned page; all decoding occurs
	// before any deletion. The final outcome and all unacknowledged events survive.
	query := `WITH latest AS (
SELECT json_extract(body,'$.event_id') AS event,max(position) AS position
FROM receipts WHERE producer=? GROUP BY json_extract(body,'$.event_id'))
SELECT r.position,r.body,last.body,m.body,m.seq,m.state,m.hash FROM latest l
JOIN messages m ON m.producer=? AND m.id=l.event
JOIN receipts last ON last.position=l.position AND last.producer=m.producer
JOIN receipts r ON r.producer=m.producer AND json_extract(r.body,'$.event_id')=m.id
WHERE m.state IN ('provider_accepted','cancelled','superseded','expired') AND m.attempt=''
AND l.position<=? AND r.position<l.position AND r.position<>?
AND julianday(json_extract(last.body,'$.at'))<julianday(?)
AND julianday(json_extract(r.body,'$.at'))<julianday(?)`
	// SQLite dates have millisecond precision. Round the SQL prefilter down;
	// the exact Go deadline check below must never see a newer rounded-up row.
	sqlCutoff := cutoff.UTC().Truncate(time.Second).Format(time.RFC3339)
	args := []any{p.ID, p.ID, acked, acked, sqlCutoff, sqlCutoff}
	if len(pins) > 0 {
		query += " AND r.position NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(pins)), ",") + ")"
		for _, position := range pins {
			args = append(args, position)
		}
	}
	query += " ORDER BY r.position LIMIT ?"
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []compactableReceipt
	for rows.Next() {
		var item compactableReceipt
		var lastBody, messageBody, state, messageHash string
		var seq uint64
		if err = rows.Scan(&item.Position, &item.Body, &lastBody, &messageBody, &seq, &state, &messageHash); err != nil {
			return nil, err
		}
		var r, last protocol.Receipt
		var m protocol.Message
		if err = protocol.Decode([]byte(messageBody), &m, protocol.MaxRequest); err != nil {
			return nil, err
		}
		if err = m.Validate(); err != nil {
			return nil, err
		}
		if err = protocol.Decode([]byte(item.Body), &r, 8192); err != nil {
			return nil, err
		}
		if err = protocol.Decode([]byte(lastBody), &last, 8192); err != nil {
			return nil, err
		}
		knownReceipt := terminal(r.State) || slices.Contains([]string{"queued", "waiting_status", "sending", "retry_wait", "fenced", "blocked", "ambiguous"}, r.State)
		if protocol.Hash(m) != messageHash || !knownReceipt || m.Producer != p || m.Sequence != seq || r.Sequence != seq || last.Sequence != seq || r.EventID != m.ID || last.EventID != m.ID || !terminal(state) || last.State != state || r.At.IsZero() || last.At.IsZero() || r.At.After(last.At) || r.At.After(cutoff) || last.At.After(cutoff) {
			return nil, fmt.Errorf("inconsistent receipt compaction evidence")
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
