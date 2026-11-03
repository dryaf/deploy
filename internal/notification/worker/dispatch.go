package worker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
)

type Result = provider.Result

type Sender interface {
	Send(context.Context, string, protocol.Message) Result
}

func (s *Store) Grant(ctx context.Context, p protocol.Producer, id string) (j Job, granted bool, err error) {
	err = s.ownership(p, func(o *ownership) error {
		return s.transaction(ctx, func(tx *sql.Tx) error {
			var err error
			j, err = readJob(tx, p.ID, id)
			if err != nil {
				return err
			}
			if s.verificationOnly {
				return nil
			}
			if terminal(j.State) || j.State == "ambiguous" || j.State == "blocked" || j.Attempt != "" {
				return nil
			}
			generation, err := s.deliveryGeneration(tx, *o, j)
			if err != nil {
				return err
			}
			if o.Active != generation || o.Generations[o.Active] != "active" {
				return s.transition(tx, j, "fenced")
			}
			route, ok := s.config.Routes[j.Message.Route]
			if !ok || route.Revision != j.RouteRevision {
				return s.transition(tx, j, "blocked")
			}
			var due int64
			if err = tx.QueryRow("SELECT due FROM messages WHERE producer=? AND id=?", p.ID, id).Scan(&due); err != nil {
				return err
			}
			if due > s.now().UnixNano() {
				return nil
			}
			a, err := s.eligibility(tx, j.Message, attemptBudget)
			if err != nil {
				return err
			}
			if !a.Eligible {
				return s.transition(tx, j, a.Reason)
			}
			for _, ref := range j.Message.Incidents {
				var attempt string
				if err = tx.QueryRow("SELECT attempt FROM incidents WHERE producer=? AND subject=? AND id=?", p.ID, ref.Subject, ref.ID).Scan(&attempt); err != nil {
					return err
				}
				if attempt != "" {
					return nil
				}
			}
			if err = s.capacity(tx, p.ID, 8192, false); err != nil {
				return err
			}
			// Queued v3 events predate the correlation tables. Freeze their typed
			// context before the first v4 send; existing enrollment pins stay frozen.
			if j.Message.Context != nil {
				if err = s.capacity(tx, p.ID, protocol.MaxRequest+8192, false); err != nil {
					return err
				}
				if err = s.freezeReference(tx, j.Message); err != nil {
					return err
				}
			}
			attempt, err := safefs.NewID("n")
			if err != nil {
				return err
			}
			provider := route.Primary
			if j.Provider == route.Primary && route.Fallback != "" {
				provider = route.Fallback
			}
			var cooldown int64
			if err = tx.QueryRow("SELECT due FROM provider_cooldowns WHERE provider=?", provider).Scan(&cooldown); err != nil && err != sql.ErrNoRows {
				return err
			}
			if cooldown > s.now().UnixNano() {
				return nil
			}
			for _, ref := range j.Message.Incidents {
				if _, err = tx.Exec("UPDATE incidents SET attempt=? WHERE producer=? AND subject=? AND id=?", attempt, p.ID, ref.Subject, ref.ID); err != nil {
					return err
				}
			}
			if _, err = tx.Exec("UPDATE messages SET state='sending',attempt=?,provider=?,tries=tries+1 WHERE producer=? AND id=?", attempt, provider, p.ID, id); err != nil {
				return err
			}
			j.Attempt = attempt
			j.Provider = provider
			j.State = "sending"
			j.Tries++
			granted = true
			return s.receipt(tx, p.ID, protocol.Receipt{EventID: id, Sequence: j.Message.Sequence, State: "sending", AttemptID: attempt, Provider: provider, At: s.now().UTC()})
		})
	})
	if err != nil {
		granted = false
	}
	return
}

func (s *Store) Finish(ctx context.Context, j Job, result Result) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		current, err := readJob(tx, j.Message.Producer.ID, j.Message.ID)
		if err != nil {
			return err
		}
		if current.Attempt == "" || current.Attempt != j.Attempt {
			return fmt.Errorf("attempt is no longer owned")
		}
		r := protocol.Receipt{EventID: j.Message.ID, Sequence: j.Message.Sequence, State: current.State, AttemptID: j.Attempt, Provider: j.Provider, At: s.now().UTC()}
		due := s.now()
		if result.RetryAfter > 0 && result.RetryAfter <= 24*time.Hour {
			if _, err = tx.Exec("INSERT INTO provider_cooldowns VALUES(?,?) ON CONFLICT(provider) DO UPDATE SET due=max(due,excluded.due)", j.Provider, s.now().Add(result.RetryAfter).UnixNano()); err != nil {
				return err
			}
		}
		if result.Accepted {
			if err = s.acceptReference(tx, j.Message, result.Binding); err != nil {
				return err
			}
			a, err := s.eligibility(tx, j.Message, 0)
			if err != nil {
				return err
			}
			if terminal(current.State) {
				a.Eligible = false
				a.Reason = current.State
			}
			r.State = "provider_accepted"
			r.Acceptance = &a
		} else if !terminal(current.State) {
			switch {
			case result.Ambiguous:
				r.State = "ambiguous"
			case result.Permanent:
				route := s.config.Routes[j.Message.Route]
				if j.Provider == route.Primary && route.Fallback != "" {
					r.State = "retry_wait"
				} else {
					r.State = "blocked"
				}
			default:
				r.State = "retry_wait"
				delay := time.Second * time.Duration(1<<min(j.Tries, 10))
				if result.RetryAfter > delay {
					delay = result.RetryAfter
				}
				if delay > 24*time.Hour {
					r.State = "blocked"
				} else {
					due = due.Add(delay)
				}
			}
		}
		if _, err = tx.Exec("UPDATE messages SET state=?,attempt='',due=? WHERE producer=? AND id=?", r.State, due.UnixNano(), j.Message.Producer.ID, j.Message.ID); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE incidents SET attempt='' WHERE producer=? AND attempt=?", j.Message.Producer.ID, j.Attempt); err != nil {
			return err
		}
		return s.receipt(tx, j.Message.Producer.ID, r)
	})
}

// DispatchOne rotates enrolled producers in the caller and never holds a state
// lock during Send. Status ingestion and LED reconciliation use separate loops.
func (s *Store) DispatchOne(ctx context.Context, p protocol.Producer, sender Sender) (bool, error) {
	type candidate struct {
		id       string
		sequence uint64
	}
	var ids []candidate
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.sweep(tx, p.ID); err != nil {
			return err
		}
		if s.scanFloor == nil {
			s.scanFloor = map[string]uint64{}
		}
		rows, err := tx.Query("SELECT id,seq FROM messages WHERE producer=? AND state IN ('queued','waiting_status','retry_wait','fenced') AND attempt='' AND due<=? ORDER BY (seq<=?),seq LIMIT 128", p.ID, s.now().UnixNano(), s.scanFloor[p.ID])
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id candidate
			if err = rows.Scan(&id.id, &id.sequence); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		j, ok, err := s.Grant(ctx, p, id.id)
		if err != nil {
			return false, err
		}
		s.mu.Lock()
		s.scanFloor[p.ID] = id.sequence
		s.mu.Unlock()
		if !ok {
			continue
		}
		attempt, cancel := context.WithTimeout(ctx, attemptBudget)
		result := sender.Send(attempt, j.Provider, j.Message)
		cancel()
		persist, done := context.WithTimeout(context.Background(), 2*time.Second)
		err = s.Finish(persist, j, result)
		done()
		return true, err
	}
	return false, nil
}

func (s *Store) RunDispatcher(ctx context.Context, sender Sender) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		for _, p := range s.config.Producers {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := s.DispatchOne(ctx, p.Producer, sender); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
