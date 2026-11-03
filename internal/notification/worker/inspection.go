package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// Inspection pages deliberately carry no consumer Cursor. Viewing a receipt
// must never advance the producer's listed/acked watermarks or authorize an ack.
type QueueInspection struct {
	Identity protocol.Identity `json:"identity"`
	Producer protocol.Producer `json:"producer"`
	After    int64             `json:"after"`
	Next     int64             `json:"next"`
	More     bool              `json:"more"`
	Jobs     []InspectedJob    `json:"jobs"`
}
type InspectedJob struct {
	ID         string    `json:"id"`
	Sequence   uint64    `json:"sequence"`
	Generation string    `json:"generation"`
	Subject    string    `json:"subject"`
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	Tries      int       `json:"tries"`
	Attempt    string    `json:"attempt,omitempty"`
	Provider   string    `json:"provider,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}
type ReceiptInspection struct {
	Identity            protocol.Identity         `json:"identity"`
	Producer            protocol.Producer         `json:"producer"`
	After               int64                     `json:"after"`
	Next                int64                     `json:"next"`
	More                bool                      `json:"more"`
	AcknowledgedThrough int64                     `json:"acknowledged_through"`
	ListedThrough       int64                     `json:"listed_through"`
	Receipts            []protocol.Receipt        `json:"receipts"`
	Compaction          *ReceiptCompactionHistory `json:"compaction,omitempty"`
}

func (s *Store) validateInspection(p protocol.Producer, after int64, limit int) error {
	if after < 0 || limit < 1 || limit > 128 {
		return fmt.Errorf("invalid inspection range")
	}
	_, err := s.config.enrollment(p)
	return err
}

func (s *Store) InspectQueue(ctx context.Context, p protocol.Producer, after int64, limit int) (page QueueInspection, err error) {
	if err = s.validateInspection(p, after, limit); err != nil {
		return
	}
	page = QueueInspection{Identity: s.identity, Producer: p, After: after, Next: after, Jobs: []InspectedJob{}}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, "SELECT body,state,attempt,provider,tries FROM messages WHERE producer=? AND seq>? ORDER BY seq LIMIT ?", p.ID, after, limit+1)
		if e != nil {
			return e
		}
		defer rows.Close()
		bytes := 2048
		for rows.Next() {
			var body string
			var m protocol.Message
			var j InspectedJob
			if e = rows.Scan(&body, &j.State, &j.Attempt, &j.Provider, &j.Tries); e != nil {
				return e
			}
			if e = protocol.Decode([]byte(body), &m, protocol.MaxRequest); e != nil {
				return e
			}
			if m.Producer != p || !protocol.Sequence(m.Sequence) || m.Sequence <= uint64(page.Next) {
				return fmt.Errorf("invalid queue inspection row")
			}
			j.ID = m.ID
			j.Sequence = m.Sequence
			j.Generation = m.Generation
			j.Subject = m.Subject
			j.Kind = m.Kind
			j.ObservedAt = m.ObservedAt
			j.ExpiresAt = m.ExpiresAt
			raw, e := json.Marshal(j)
			if e != nil {
				return e
			}
			if len(page.Jobs) == limit || bytes+len(raw)+1 > protocol.MaxResponse-2048 {
				page.More = true
				break
			}
			page.Jobs = append(page.Jobs, j)
			page.Next = int64(m.Sequence)
			bytes += len(raw) + 1
		}
		return rows.Err()
	})
	return
}

func (s *Store) InspectReceipts(ctx context.Context, p protocol.Producer, after int64, limit int) (page ReceiptInspection, err error) {
	if err = s.validateInspection(p, after, limit); err != nil {
		return
	}
	page = ReceiptInspection{Identity: s.identity, Producer: p, After: after, Next: after, Receipts: []protocol.Receipt{}}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if e := tx.QueryRowContext(ctx, "SELECT acked,listed FROM producers WHERE id=?", p.ID).Scan(&page.AcknowledgedThrough, &page.ListedThrough); e != nil {
			return e
		}
		var e error
		page.Compaction, e = readReceiptCompaction(ctx, tx, s.identity, p)
		if e != nil {
			return e
		}
		rows, e := tx.QueryContext(ctx, "SELECT position,body FROM receipts WHERE producer=? AND position>? ORDER BY position LIMIT ?", p.ID, after, limit+1)
		if e != nil {
			return e
		}
		defer rows.Close()
		bytes := 2048
		for rows.Next() {
			var body string
			var pos int64
			var r protocol.Receipt
			if e = rows.Scan(&pos, &body); e != nil {
				return e
			}
			if e = protocol.Decode([]byte(body), &r, 8192); e != nil {
				return e
			}
			r.Position = pos
			raw, e := json.Marshal(r)
			if e != nil {
				return e
			}
			if len(page.Receipts) == limit || bytes+len(raw)+1 > protocol.MaxResponse-2048 {
				page.More = true
				break
			}
			page.Receipts = append(page.Receipts, r)
			page.Next = pos
			bytes += len(raw) + 1
		}
		return rows.Err()
	})
	return
}
