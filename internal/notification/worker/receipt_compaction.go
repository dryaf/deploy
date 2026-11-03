package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Compaction retains complete messages and each event's latest outcome. It never
// turns discarded intermediate history into a producer acknowledgement or an
// outcome checkpoint. Sequence/event replay protection therefore remains intact.
type ReceiptCompaction struct {
	Identity      protocol.Identity         `json:"identity"`
	Producer      protocol.Producer         `json:"producer"`
	Cutoff        time.Time                 `json:"cutoff"`
	RetentionDays int                       `json:"retention_days"`
	Candidates    []int64                   `json:"candidate_positions"`
	LogicalBytes  int64                     `json:"logical_bytes"`
	More          bool                      `json:"more"`
	Applied       bool                      `json:"applied"`
	History       *ReceiptCompactionHistory `json:"history,omitempty"`
}

// History describes holes in a retained receipt stream. Through is the largest
// discarded position, not a contiguous floor, delivery proof or consumer cursor.
// One bounded cumulative record is retained per worker/producer epoch.
type ReceiptCompactionHistory struct {
	Version       int               `json:"version"`
	Identity      protocol.Identity `json:"identity"`
	Producer      protocol.Producer `json:"producer"`
	Passes        uint64            `json:"passes"`
	Removed       uint64            `json:"removed"`
	Through       int64             `json:"through"`
	RecordedAt    time.Time         `json:"recorded_at"`
	Cutoff        time.Time         `json:"cutoff"`
	RetentionDays int               `json:"retention_days"`
	Previous      string            `json:"previous,omitempty"`
	BatchHash     string            `json:"batch_hash"`
}

func receiptCompactionKey(id protocol.Identity, p protocol.Producer) string {
	return "receipt-compaction-v1:" + protocol.Hash(struct {
		Identity protocol.Identity
		Producer protocol.Producer
	}{id, p})
}
func (h ReceiptCompactionHistory) validate() error {
	if h.Version != 1 || !h.Identity.Valid() || !h.Producer.Valid() || h.Passes == 0 || h.Removed < h.Passes || h.Through <= 0 || h.Removed > uint64(h.Through) || h.RecordedAt.IsZero() || h.RetentionDays < 90 || h.RetentionDays > 3650 || !h.Cutoff.Equal(h.RecordedAt.Add(-time.Duration(h.RetentionDays)*24*time.Hour)) || !safefs.DigestPattern.MatchString(h.BatchHash) || (h.Passes == 1 && h.Previous != "") || (h.Passes > 1 && !safefs.DigestPattern.MatchString(h.Previous)) {
		return fmt.Errorf("invalid receipt compaction history")
	}
	return nil
}
func readReceiptCompaction(ctx context.Context, tx ledRowReader, id protocol.Identity, p protocol.Producer) (*ReceiptCompactionHistory, error) {
	var raw string
	err := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", receiptCompactionKey(id, p)).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h ReceiptCompactionHistory
	if err = protocol.Decode([]byte(raw), &h, 8192); err != nil {
		return nil, err
	}
	if err = h.validate(); err != nil {
		return nil, err
	}
	if h.Identity != id || h.Producer != p {
		return nil, fmt.Errorf("receipt compaction epoch mismatch")
	}
	var acked int64
	if err = tx.QueryRowContext(ctx, "SELECT acked FROM producers WHERE id=? AND epoch=?", p.ID, p.Epoch).Scan(&acked); err != nil {
		return nil, err
	}
	var present int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM receipts WHERE position=?", h.Through).Scan(&present); err != nil {
		return nil, err
	}
	if h.Through >= acked || present != 0 {
		return nil, fmt.Errorf("receipt compaction history contradicts retained stream")
	}
	return &h, nil
}

func verifyReceiptCompaction(ctx context.Context, q ledRowReader, c Config, id protocol.Identity) error {
	for _, enrollment := range c.Producers {
		if _, err := readReceiptCompaction(ctx, q, id, enrollment.Producer); err != nil {
			return err
		}
	}
	return nil
}

type receiptCompactor struct {
	s     *Store
	fault func(string) error
}

// CompactReceipts performs one bounded offline pass. Preview runs its entire
// selection under a transaction but writes neither audit nor cursor state.
// The CLI holds the exclusive worker lease and exposes apply as an explicit flag.
func (s *Store) CompactReceipts(ctx context.Context, p protocol.Producer, limit int, apply bool) (ReceiptCompaction, error) {
	return (receiptCompactor{s: s}).run(ctx, p, limit, apply)
}

func (c receiptCompactor) boundary(point string) error {
	if c.fault != nil {
		return c.fault(point)
	}
	return nil
}

func (c receiptCompactor) run(ctx context.Context, p protocol.Producer, limit int, apply bool) (out ReceiptCompaction, err error) {
	s := c.s
	if limit < 1 || limit > 1024 {
		return out, fmt.Errorf("receipt compaction limit must be 1..1024")
	}
	if _, err = s.config.enrollment(p); err != nil {
		return
	}
	out = ReceiptCompaction{Identity: s.identity, Producer: p, Candidates: []int64{}, RetentionDays: s.config.ReceiptAuditDays}
	if out.RetentionDays == 0 {
		out.RetentionDays = 90
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if !s.clockSafe() {
			return fmt.Errorf("receipt compaction requires a reliable clock")
		}
		now := s.now()
		out.Cutoff = now.UTC().Add(-time.Duration(out.RetentionDays) * 24 * time.Hour)
		old, e := readReceiptCompaction(ctx, tx, s.identity, p)
		if e != nil {
			return e
		}
		out.History = old
		if old != nil && now.Before(old.RecordedAt) {
			return fmt.Errorf("receipt compaction clock moved backwards")
		}
		var acked, listed int64
		if e = tx.QueryRowContext(ctx, "SELECT acked,listed FROM producers WHERE id=? AND epoch=?", p.ID, p.Epoch).Scan(&acked, &listed); e != nil {
			return e
		}
		if acked < 0 || listed < acked {
			return fmt.Errorf("invalid receipt acknowledgement floors")
		}
		// Pin administrative receipts even after their messages progress. Their
		// full provenance is verified before compaction and must remain queryable.
		pins, e := administrativeReceiptPins(ctx, tx, s.identity)
		if e != nil {
			return e
		}
		selected, e := selectCompactableReceipts(ctx, tx, p, acked, out.Cutoff, limit, pins)
		if e != nil {
			return e
		}
		if len(selected) > limit {
			out.More = true
			selected = selected[:limit]
		}
		for _, item := range selected {
			out.Candidates = append(out.Candidates, item.Position)
			out.LogicalBytes += int64(len(item.Body))
		}
		if !apply || len(selected) == 0 {
			return nil
		}
		// Deletions also write WAL pages. Reserve conservative leaf/index and
		// audit headroom instead of assuming removal needs no temporary space.
		reserve := 16384
		for _, item := range selected {
			reserve += len(item.Body) + 16384
		}
		if e = s.capacity(tx, p.ID, reserve, false); e != nil {
			return e
		}
		h := ReceiptCompactionHistory{Version: 1, Identity: s.identity, Producer: p, Passes: 1, Removed: uint64(len(selected)), Through: selected[len(selected)-1].Position, RecordedAt: now.UTC(), Cutoff: out.Cutoff, RetentionDays: out.RetentionDays, BatchHash: protocol.Hash(selected)}
		if old != nil {
			if old.Passes == math.MaxUint64 || old.Removed > math.MaxUint64-h.Removed {
				return fmt.Errorf("receipt compaction counters exhausted")
			}
			h.Passes += old.Passes
			h.Removed += old.Removed
			h.Through = max(h.Through, old.Through)
			h.Previous = protocol.Hash(old)
		}
		if e = h.validate(); e != nil {
			return e
		}
		for _, item := range selected {
			result, e := tx.ExecContext(ctx, "DELETE FROM receipts WHERE position=? AND producer=? AND body=?", item.Position, p.ID, item.Body)
			if e != nil {
				return e
			}
			count, e := result.RowsAffected()
			if e != nil {
				return e
			}
			if count != 1 {
				return fmt.Errorf("receipt changed during compaction")
			}
		}
		if e = c.boundary("receipt_compaction_deleted"); e != nil {
			return e
		}
		body, e := json.Marshal(h)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", receiptCompactionKey(s.identity, p), string(body)); e != nil {
			return e
		}
		if e = c.boundary("receipt_compaction_transaction"); e != nil {
			return e
		}
		end := s.now()
		if !s.clockSafe() || end.Before(now) || time.Duration(end.UnixNano()-now.UnixNano())-end.Sub(now) > 2*time.Second || end.Sub(now)-time.Duration(end.UnixNano()-now.UnixNano()) > 2*time.Second {
			return fmt.Errorf("clock changed during receipt compaction")
		}
		out.History = &h
		return nil
	})
	if err != nil {
		return ReceiptCompaction{}, err
	}
	if apply && len(out.Candidates) > 0 {
		out.Applied = true
		err = c.boundary("receipt_compaction_commit")
	}
	return
}
