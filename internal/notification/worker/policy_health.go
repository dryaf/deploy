package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func (s *Store) EnqueueTransitions(ctx context.Context, b protocol.TransitionBatch) (receipt protocol.TransitionReceipt, err error) {
	if err = b.Validate(); err != nil {
		return
	}
	e, err := s.config.enrollment(b.Producer)
	if err != nil {
		return receipt, err
	}
	if !e.PolicyHealth {
		return receipt, fmt.Errorf("policy health not enrolled")
	}
	receipt = protocol.TransitionReceipt{Identity: s.identity, Producer: b.Producer}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.requireUnretiredProducer(b.Producer); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow("SELECT count(*) FROM fault_transitions WHERE producer=? AND state='pending'", b.Producer.ID).Scan(&pending); err != nil {
			return err
		}
		for _, r := range b.Records {
			hash := protocol.Hash(r)
			var prior string
			err := tx.QueryRow("SELECT hash FROM fault_transitions WHERE producer=? AND id=?", b.Producer.ID, r.ID).Scan(&prior)
			if err == nil {
				if hash != prior {
					return fmt.Errorf("transition identity conflict")
				}
			} else if err != sql.ErrNoRows {
				return err
			} else {
				if r.ObservedAt.After(s.now().Add(5*time.Second)) || r.ValidUntil.Sub(r.ObservedAt) > time.Duration(e.MaxEvidenceSeconds)*time.Second {
					return fmt.Errorf("invalid transition horizon")
				}
				pending++
				if pending > e.MaxMessages {
					return fmt.Errorf("transition quota exhausted")
				}
				raw, _ := json.Marshal(r)
				// Transition admission leaves extra space for a complete snapshot.
				if err = s.capacity(tx, b.Producer.ID, len(raw)+(protocol.MaxRequest*2), false); err != nil {
					return err
				}
				if _, err = tx.Exec("INSERT INTO fault_transitions VALUES(?,?,?,?,'pending')", b.Producer.ID, r.ID, hash, string(raw)); err != nil {
					return err
				}
			}
			receipt.Records = append(receipt.Records, protocol.TransitionRecord{ID: r.ID, Hash: hash})
		}
		return s.applyTransitions(tx, b.Producer)
	})
	if err != nil {
		receipt = protocol.TransitionReceipt{}
	}
	return
}

// recordPolicy retains fault incarnations independently of the latest full
// snapshot. A new ok observation alone cannot erase any unconsumed fault.
func (s *Store) recordPolicy(tx *sql.Tx, v protocol.Snapshot) error {
	for _, a := range v.Support {
		var rev uint64
		var hash string
		err := tx.QueryRow("SELECT revision,hash FROM policy_support WHERE producer=? AND id=?", v.Producer.ID, a.Decision).Scan(&rev, &hash)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if a.Revision < rev || (a.Revision == rev && hash != protocol.Hash(a)) {
			return fmt.Errorf("decision revision conflict")
		}
		raw, _ := json.Marshal(a)
		if _, err = tx.Exec("INSERT INTO policy_support VALUES(?,?,?,?,?) ON CONFLICT(producer,id) DO UPDATE SET revision=excluded.revision,hash=excluded.hash,body=excluded.body", v.Producer.ID, a.Decision, a.Revision, protocol.Hash(a), string(raw)); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO policy_dependencies VALUES(?,?,?,?) ON CONFLICT(producer,subject,decision) DO UPDATE SET revision=excluded.revision", v.Producer.ID, a.Subject, a.Decision, a.Revision); err != nil {
			return err
		}
	}
	for _, sub := range v.Subjects {
		var legacyBody, legacyLatch string
		err := tx.QueryRow("SELECT body,latched FROM subjects WHERE producer=? AND id=?", v.Producer.ID, sub.Key).Scan(&legacyBody, &legacyLatch)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && legacyLatch != "" {
			var old protocol.Subject
			if err = protocol.Decode([]byte(legacyBody), &old, 8192); err != nil {
				return err
			}
			if old.FaultRevision == 0 {
				return fmt.Errorf("legacy fault requires explicit policy enrollment migration")
			}
		}
		var rev uint64
		if err := tx.QueryRow("SELECT COALESCE(max(revision),0) FROM policy_faults WHERE producer=? AND subject=?", v.Producer.ID, sub.Key).Scan(&rev); err != nil {
			return err
		}
		if sub.FaultRevision < rev {
			return fmt.Errorf("fault revision decreased")
		}
		if sub.Condition != "critical" && sub.Condition != "warning" {
			continue
		}
		if !protocol.Sequence(sub.FaultRevision) {
			return fmt.Errorf("fault revision required")
		}
		var condition string
		var active int
		err = tx.QueryRow("SELECT condition,active FROM policy_faults WHERE producer=? AND subject=? AND revision=?", v.Producer.ID, sub.Key, sub.FaultRevision).Scan(&condition, &active)
		if err == nil {
			if condition != sub.Condition || active == 0 {
				return fmt.Errorf("fault incarnation reused")
			}
			continue
		}
		if err != sql.ErrNoRows {
			return err
		}
		if _, err = tx.Exec("INSERT INTO policy_faults VALUES(?,?,?,?,1)", v.Producer.ID, sub.Key, sub.FaultRevision, sub.Condition); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyTransitions(tx *sql.Tx, p protocol.Producer) error {
	var raw, session string
	err := tx.QueryRow("SELECT body,session FROM status WHERE producer=?", p.ID).Scan(&raw, &session)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	var snap protocol.Snapshot
	if err = protocol.Decode([]byte(raw), &snap, protocol.MaxRequest); err != nil {
		return err
	}
	e, _ := s.config.enrollment(p)
	if !s.clockSafe() || session != s.session || !snap.EvaluatedAt.Add(time.Duration(e.MaxSilenceSeconds)*time.Second).After(s.now()) {
		return nil
	}
	rows, err := tx.Query("SELECT body FROM fault_transitions WHERE producer=? AND state='pending'", p.ID)
	if err != nil {
		return err
	}
	var records []protocol.FaultTransition
	for rows.Next() {
		var body string
		var r protocol.FaultTransition
		if err = rows.Scan(&body); err == nil {
			err = protocol.Decode([]byte(body), &r, protocol.MaxRequest)
		}
		if err != nil {
			rows.Close()
			return err
		}
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.StatusSequence > snap.Sequence || r.BasisRevision > snap.BasisRevision {
			continue
		}
		state := ""
		if !r.ValidUntil.After(s.now()) {
			state = "expired"
		} else {
			var sub *protocol.Subject
			for i := range snap.Subjects {
				if snap.Subjects[i].Key == r.Subject {
					sub = &snap.Subjects[i]
					break
				}
			}
			if sub == nil || !sub.ValidUntil.After(s.now()) {
				continue
			}
			valid := false
			switch r.Cause {
			case "resolved":
				valid = sub.Condition == "ok"
			case "retired":
				valid = sub.Condition == "retired"
			case "policy_accepted":
				valid = sub.Condition == "ok" || sub.Condition == "warning" || sub.Condition == "critical"
				for _, ref := range r.Support {
					found := false
					for _, a := range snap.Support {
						if a.Subject == r.Subject && a.Decision == ref.ID && a.Revision == ref.Revision && a.ValidUntil.After(s.now()) {
							found = true
						}
					}
					valid = valid && found
				}
			}
			// An ongoing fault at the same revision has not been discharged by
			// this snapshot. Newer adverse revisions can coexist with clearing old ones.
			if (sub.Condition == "critical" || sub.Condition == "warning") && sub.FaultRevision <= r.FaultRevision {
				valid = false
			}
			if !valid {
				continue
			}
			if (r.Cause == "resolved" || r.Cause == "retired") && r.FaultRevision == sub.FaultRevision && r.BasisRevision == snap.BasisRevision {
				if _, err = tx.Exec("DELETE FROM policy_dependencies WHERE producer=? AND subject=?", p.ID, r.Subject); err != nil {
					return err
				}
				// Current still-active decisions remain dependencies even after a
				// different contributor has resolved.
				for _, a := range snap.Support {
					if a.Subject == r.Subject {
						if _, err = tx.Exec("INSERT INTO policy_dependencies VALUES(?,?,?,?)", p.ID, a.Subject, a.Decision, a.Revision); err != nil {
							return err
						}
					}
				}
			}
			res, err := tx.Exec("UPDATE policy_faults SET active=0 WHERE producer=? AND subject=? AND revision=? AND active=1", p.ID, r.Subject, r.FaultRevision)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			state = "consumed"
			if n == 0 {
				state = "non_applicable"
			}
		}
		if _, err = tx.Exec("UPDATE fault_transitions SET state=? WHERE producer=? AND id=?", state, p.ID, r.ID); err != nil {
			return err
		}
	}
	// Recompute the latch, never the producer's current condition, from all
	// remaining fault incarnations. The LED reconciler consumes this aggregate.
	_, err = tx.Exec(`UPDATE subjects SET latched=CASE
WHEN EXISTS(SELECT 1 FROM policy_faults f WHERE f.producer=subjects.producer AND f.subject=subjects.id AND f.active=1 AND f.condition='critical') THEN 'critical'
WHEN EXISTS(SELECT 1 FROM policy_faults f WHERE f.producer=subjects.producer AND f.subject=subjects.id AND f.active=1 AND f.condition='warning') THEN 'warning' ELSE '' END WHERE producer=?`, p.ID)
	if err != nil {
		return err
	}
	for _, sub := range snap.Subjects {
		sub = clampSupport(sub, snap.Support)
		if sub.Condition == "ok" {
			rows, err := tx.Query("SELECT decision,revision FROM policy_dependencies WHERE producer=? AND subject=?", p.ID, sub.Key)
			if err != nil {
				return err
			}
			missing := false
			for rows.Next() {
				var id string
				var rev uint64
				if err = rows.Scan(&id, &rev); err != nil {
					rows.Close()
					return err
				}
				found := false
				for _, a := range snap.Support {
					if a.Subject == sub.Key && a.Decision == id && a.Revision == rev {
						found = true
					}
				}
				missing = missing || !found
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if missing {
				sub.Condition = "unknown"
				sub.Reason = "acceptance support missing; resolution evidence required"
			}
		}
		body, _ := json.Marshal(sub)
		if _, err = tx.Exec("UPDATE subjects SET body=? WHERE producer=? AND id=?", string(body), p.ID, sub.Key); err != nil {
			return err
		}
	}
	return nil
}

func clampSupport(sub protocol.Subject, support []protocol.AcceptanceSupport) protocol.Subject {
	for _, a := range support {
		if a.Subject == sub.Key && (sub.ValidUntil.IsZero() || a.ValidUntil.Before(sub.ValidUntil)) {
			sub.ValidUntil = a.ValidUntil
		}
	}
	return sub
}
