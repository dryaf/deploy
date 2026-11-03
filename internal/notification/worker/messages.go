package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type Job struct {
	Message       protocol.Message `json:"message"`
	State         string           `json:"state"`
	Attempt       string           `json:"attempt,omitempty"`
	Provider      string           `json:"provider,omitempty"`
	RouteRevision string           `json:"route_revision"`
	Tries         int              `json:"tries"`
}

func readJob(tx *sql.Tx, p, id string) (j Job, err error) {
	var body string
	err = tx.QueryRow("SELECT body,state,attempt,provider,route_revision,tries FROM messages WHERE producer=? AND id=?", p, id).Scan(&body, &j.State, &j.Attempt, &j.Provider, &j.RouteRevision, &j.Tries)
	if err == nil {
		err = protocol.Decode([]byte(body), &j.Message, protocol.MaxRequest)
	}
	return
}

func (s *Store) Get(ctx context.Context, p protocol.Producer, id string) (j Job, err error) {
	if _, err = s.config.enrollment(p); err != nil {
		return
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error { j, err = readJob(tx, p.ID, id); return err })
	return
}

func (s *Store) Enqueue(ctx context.Context, m protocol.Message) (j Job, err error) {
	if err = m.Validate(); err != nil {
		return
	}
	e, err := s.config.enrollment(m.Producer)
	if err != nil {
		return j, err
	}
	route, ok := s.config.Routes[m.Route]
	if m.Context != nil && !e.InteractiveContext {
		return j, fmt.Errorf("interactive context not enrolled")
	}
	if m.Context != nil {
		for _, target := range m.Context.Targets {
			if !slices.Contains(e.ContextKinds, target.Kind) {
				return j, fmt.Errorf("interactive target kind not enrolled")
			}
		}
	}
	if !ok || !slices.Contains(e.Routes, m.Route) {
		return j, fmt.Errorf("route not enrolled")
	}
	if m.ObservedAt.After(s.now().Add(5*time.Second)) || (protocol.Guarded(m.Kind) && m.ExpiresAt.Sub(m.ObservedAt) > 5*time.Minute) {
		return j, fmt.Errorf("invalid observation horizon")
	}
	b, _ := json.Marshal(m)
	hash := protocol.Hash(m)
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.requireUnretiredProducer(m.Producer); err != nil {
			return err
		}
		var oldHash string
		err := tx.QueryRow("SELECT hash FROM messages WHERE producer=? AND id=?", m.Producer.ID, m.ID).Scan(&oldHash)
		if err == nil {
			if oldHash != hash {
				return fmt.Errorf("immutable event conflict")
			}
			j, err = readJob(tx, m.Producer.ID, m.ID)
			return err
		}
		if err != sql.ErrNoRows {
			return err
		}
		var floor uint64
		if err = tx.QueryRow("SELECT submitted FROM producers WHERE id=?", m.Producer.ID).Scan(&floor); err != nil {
			return err
		}
		if m.Sequence != floor+1 {
			return fmt.Errorf("expected submission sequence %d", floor+1)
		}
		budget := len(b)
		if m.Context != nil {
			contextBody, _ := json.Marshal(m.Context)
			budget += 2*len(contextBody) + 8192
		}
		if err = s.capacity(tx, m.Producer.ID, budget, true); err != nil {
			return err
		}
		if m.Context != nil {
			var prior string
			err = tx.QueryRow("SELECT hash FROM interaction_contexts WHERE producer=? AND id=? AND revision=?", m.Producer.ID, m.Context.ID, m.Context.Revision).Scan(&prior)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if err == nil && prior != protocol.Hash(m.Context) {
				return fmt.Errorf("context conflict")
			}
			body, _ := json.Marshal(m.Context)
			if _, err = tx.Exec("INSERT OR IGNORE INTO interaction_contexts VALUES(?,?,?,?,?)", m.Producer.ID, m.Context.ID, m.Context.Revision, protocol.Hash(m.Context), string(body)); err != nil {
				return err
			}
			if err = s.freezeReference(tx, m); err != nil {
				return err
			}
		}
		if m.ReportKey != "" {
			var revision uint64
			err = tx.QueryRow("SELECT revision FROM reports WHERE producer=? AND key=?", m.Producer.ID, m.ReportKey).Scan(&revision)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if m.ReportRevision <= revision {
				return fmt.Errorf("report revision did not advance")
			}
			if _, err = tx.Exec("INSERT INTO reports VALUES(?,?,?) ON CONFLICT(producer,key) DO UPDATE SET revision=excluded.revision", m.Producer.ID, m.ReportKey, m.ReportRevision); err != nil {
				return err
			}
		}
		for _, ref := range m.Incidents {
			var revision uint64
			err = tx.QueryRow("SELECT revision FROM incidents WHERE producer=? AND subject=? AND id=?", m.Producer.ID, ref.Subject, ref.ID).Scan(&revision)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if ref.Revision < revision {
				return fmt.Errorf("obsolete incident transition")
			}
			if _, err = tx.Exec("INSERT INTO incidents(producer,subject,id,revision) VALUES(?,?,?,?) ON CONFLICT(producer,subject,id) DO UPDATE SET revision=excluded.revision", m.Producer.ID, ref.Subject, ref.ID, ref.Revision); err != nil {
				return err
			}
		}
		if _, err = tx.Exec("INSERT INTO messages(producer,id,seq,hash,body,generation,route_revision,state) VALUES(?,?,?,?,?,?,?,'queued')", m.Producer.ID, m.ID, m.Sequence, hash, string(b), m.Generation, route.Revision); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE producers SET submitted=? WHERE id=?", m.Sequence, m.Producer.ID); err != nil {
			return err
		}
		// Supersession and admission are one transaction. In-flight reservations
		// survive supersession until the actual attempt settles.
		if err = s.sweep(tx, m.Producer.ID); err != nil {
			return err
		}
		j, err = readJob(tx, m.Producer.ID, m.ID)
		return err
	})
	return
}

func (s *Store) obsolete(tx *sql.Tx, m protocol.Message) (bool, error) {
	if m.ReportKey != "" {
		var r uint64
		if err := tx.QueryRow("SELECT revision FROM reports WHERE producer=? AND key=?", m.Producer.ID, m.ReportKey).Scan(&r); err != nil {
			return false, err
		}
		if r > m.ReportRevision {
			return true, nil
		}
	}
	for _, i := range m.Incidents {
		var r uint64
		if err := tx.QueryRow("SELECT revision FROM incidents WHERE producer=? AND subject=? AND id=?", m.Producer.ID, i.Subject, i.ID).Scan(&r); err != nil {
			return false, err
		}
		if r > i.Revision {
			return true, nil
		}
	}
	if protocol.Guarded(m.Kind) {
		var basis uint64
		err := tx.QueryRow("SELECT basis FROM status WHERE producer=?", m.Producer.ID).Scan(&basis)
		if err != nil && err != sql.ErrNoRows {
			return false, err
		}
		if basis > m.BasisRevision {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) transition(tx *sql.Tx, j Job, state string) error {
	if j.State == state {
		return nil
	}
	if _, err := tx.Exec("UPDATE messages SET state=? WHERE producer=? AND id=?", state, j.Message.Producer.ID, j.Message.ID); err != nil {
		return err
	}
	return s.receipt(tx, j.Message.Producer.ID, protocol.Receipt{EventID: j.Message.ID, Sequence: j.Message.Sequence, State: state, At: s.now().UTC(), AttemptID: j.Attempt, Provider: j.Provider})
}

func (s *Store) sweep(tx *sql.Tx, p string) error {
	if !s.clockSafe() {
		return nil
	}
	rows, err := tx.Query("SELECT body,state,attempt,provider FROM messages WHERE producer=? AND state NOT IN ('provider_accepted','expired','superseded','cancelled')", p)
	if err != nil {
		return err
	}
	var jobs []Job
	for rows.Next() {
		var j Job
		var body string
		if err = rows.Scan(&body, &j.State, &j.Attempt, &j.Provider); err != nil {
			rows.Close()
			return err
		}
		if err = protocol.Decode([]byte(body), &j.Message, protocol.MaxRequest); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, j := range jobs {
		obsolete, err := s.obsolete(tx, j.Message)
		if err != nil {
			return err
		}
		if obsolete {
			if err = s.transition(tx, j, "superseded"); err != nil {
				return err
			}
		} else if !j.Message.ExpiresAt.IsZero() && !j.Message.ExpiresAt.After(s.now().Add(attemptBudget)) {
			if err = s.transition(tx, j, "expired"); err != nil {
				return err
			}
		}
	}
	return nil
}

// Cancel also consumes a never-admitted sequence. Its hash must match the frozen
// message so an ambiguous enqueue acknowledgement can safely be cancelled.
func (s *Store) Cancel(ctx context.Context, p protocol.Producer, id string, seq uint64, hash string) error {
	if _, err := s.config.enrollment(p); err != nil {
		return err
	}
	if !protocol.ValidToken(id) || !protocol.Sequence(seq) || !safefs.DigestPattern.MatchString(hash) {
		return fmt.Errorf("invalid cancellation")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var prior string
		err := tx.QueryRow("SELECT hash FROM messages WHERE producer=? AND id=?", p.ID, id).Scan(&prior)
		if err == nil {
			if prior != hash {
				return fmt.Errorf("cancellation hash conflict")
			}
			j, err := readJob(tx, p.ID, id)
			if err != nil {
				return err
			}
			if j.Message.Sequence != seq {
				return fmt.Errorf("cancellation sequence conflict")
			}
			if terminal(j.State) {
				return nil
			}
			return s.transition(tx, j, "cancelled")
		}
		if err != sql.ErrNoRows {
			return err
		}
		var floor uint64
		if err = tx.QueryRow("SELECT submitted FROM producers WHERE id=?", p.ID).Scan(&floor); err != nil {
			return err
		}
		if seq != floor+1 {
			return fmt.Errorf("cancellation sequence gap")
		}
		if err = s.capacity(tx, p.ID, 1024, false); err != nil {
			return err
		}
		b, _ := json.Marshal(protocol.Message{Producer: p, ID: id, Sequence: seq})
		if _, err = tx.Exec("INSERT INTO messages(producer,id,seq,hash,body,generation,route_revision,state) VALUES(?,?,?,?,?,'','','cancelled')", p.ID, id, seq, hash, string(b)); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE producers SET submitted=? WHERE id=?", seq, p.ID); err != nil {
			return err
		}
		return s.receipt(tx, p.ID, protocol.Receipt{EventID: id, Sequence: seq, State: "cancelled", At: s.now().UTC()})
	})
}

type ReceiptPage struct {
	Receipts []protocol.Receipt `json:"receipts"`
	Cursor   protocol.Cursor    `json:"cursor"`
}

func (s *Store) Receipts(ctx context.Context, c protocol.Cursor, limit int) (page ReceiptPage, err error) {
	if c.Identity != s.identity || c.Position < 0 || limit < 1 || limit > 128 {
		return page, fmt.Errorf("invalid receipt cursor/limit")
	}
	if _, err = s.config.enrollment(c.Producer); err != nil {
		return page, err
	}
	page.Cursor = c
	page.Receipts = []protocol.Receipt{}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var acked, listed int64
		if err := tx.QueryRow("SELECT acked,listed FROM producers WHERE id=?", c.Producer.ID).Scan(&acked, &listed); err != nil {
			return err
		}
		if c.Position < acked || c.Position > listed {
			return fmt.Errorf("receipt cursor outside retained range")
		}
		rows, err := tx.Query("SELECT position,body FROM receipts WHERE producer=? AND position>? ORDER BY position LIMIT ?", c.Producer.ID, c.Position, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var pos int64
			var body string
			var r protocol.Receipt
			if err = rows.Scan(&pos, &body); err != nil {
				rows.Close()
				return err
			}
			if err = protocol.Decode([]byte(body), &r, 8192); err != nil {
				rows.Close()
				return err
			}
			r.Position = pos
			page.Receipts = append(page.Receipts, r)
			page.Cursor.Position = pos
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE producers SET listed=max(listed,?) WHERE id=?", page.Cursor.Position, c.Producer.ID)
		return err
	})
	return
}

func (s *Store) Ack(ctx context.Context, c protocol.Cursor) error {
	if c.Identity != s.identity || c.Position < 0 {
		return fmt.Errorf("receipt epoch mismatch")
	}
	if _, err := s.config.enrollment(c.Producer); err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var listed int64
		if err := tx.QueryRow("SELECT listed FROM producers WHERE id=?", c.Producer.ID).Scan(&listed); err != nil {
			return err
		}
		if c.Position > listed {
			return fmt.Errorf("unissued receipt cursor")
		}
		if c.Position != 0 {
			var p string
			if err := tx.QueryRow("SELECT producer FROM receipts WHERE position=?", c.Position).Scan(&p); err != nil || p != c.Producer.ID {
				return fmt.Errorf("cursor belongs to another producer")
			}
		}
		_, err := tx.Exec("UPDATE producers SET acked=max(acked,?) WHERE id=?", c.Position, c.Producer.ID)
		return err
	})
}
