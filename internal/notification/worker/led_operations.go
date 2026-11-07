package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

// This is bounded audit history, never a queue or authority to replay a mutation.
// Cleanup authority remains in led_states and requires fresh source evidence.
const ledOperationRetention = 64

var errLEDObservationChanged = errors.New("LED source evidence changed or expired")

type ledOperation struct {
	Version           int               `json:"version"`
	Identity          protocol.Identity `json:"identity"`
	Binding           string            `json:"binding"`
	Revision          int64             `json:"revision"`
	LayoutHash        string            `json:"layout_hash"`
	Kind              string            `json:"kind"`
	ID                string            `json:"id"`
	Condition         string            `json:"condition,omitempty"`
	Ownership         string            `json:"ownership,omitempty"`
	State             *led.State        `json:"state,omitempty"`
	EvidenceHash      string            `json:"evidence_hash,omitempty"`
	EvidenceCondition string            `json:"evidence_condition,omitempty"`
	Deadline          time.Time         `json:"deadline"`
	IssuedAt          time.Time         `json:"issued_at"`
}

type ledOperationEntry struct {
	Intent       ledOperation `json:"intent"`
	Outcome      string       `json:"outcome"` // pending, acknowledged, uncertain, not_sent
	Sent         *led.State   `json:"sent,omitempty"`
	DispatchedAt time.Time    `json:"dispatched_at"`
	FinishedAt   time.Time    `json:"finished_at"`
}

type ledOperationHead struct {
	Issued            int64              `json:"issued"`
	PrunedThrough     int64              `json:"pruned_through"`
	PrunedUnconfirmed int64              `json:"pruned_unconfirmed"`
	LastAcknowledged  *ledOperationEntry `json:"last_acknowledged,omitempty"`
}

// LEDOperationStatus reports historical RPC acknowledgements, not current daemon
// state or physical rendering. Pending includes attempts interrupted by a crash.
type LEDOperationStatus struct {
	LayoutHash               string    `json:"layout_hash"`
	Issued                   int64     `json:"issued"`
	PrunedThrough            int64     `json:"pruned_through"`
	PrunedUnconfirmed        int64     `json:"pruned_unconfirmed"`
	Pending                  int       `json:"pending"`
	Uncertain                int       `json:"uncertain"`
	LastAcknowledgedRevision int64     `json:"last_acknowledged_revision,omitempty"`
	LastAcknowledgedID       string    `json:"last_acknowledged_id,omitempty"`
	LastAcknowledgedKind     string    `json:"last_acknowledged_kind,omitempty"`
	LastAcknowledgedAt       time.Time `json:"last_acknowledged_at"`
}

func migrateLEDOperations(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE led_operation_heads(binding TEXT PRIMARY KEY,body TEXT NOT NULL,hash TEXT NOT NULL);
CREATE TABLE led_operations(binding TEXT NOT NULL,revision INTEGER NOT NULL,body TEXT NOT NULL,hash TEXT NOT NULL,PRIMARY KEY(binding,revision));
PRAGMA user_version=8;`)
	if err != nil {
		return err
	}
	return migrateLEDMappings(tx)
}

func ledJournalDecode(body, hash string, v any) error {
	if err := protocol.Decode([]byte(body), v, 16384); err != nil {
		return err
	}
	if protocol.Hash(v) != hash {
		return fmt.Errorf("LED operation journal digest mismatch")
	}
	return nil
}

func readLEDHead(ctx context.Context, q ledRowReader, binding string) (h ledOperationHead, err error) {
	var body, hash string
	err = q.QueryRowContext(ctx, "SELECT body,hash FROM led_operation_heads WHERE binding=?", binding).Scan(&body, &hash)
	if err == nil {
		err = ledJournalDecode(body, hash, &h)
	}
	return
}

func writeLEDHead(ctx context.Context, tx *sql.Tx, binding string, h ledOperationHead) error {
	body, err := json.Marshal(h)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO led_operation_heads(binding,body,hash) VALUES(?,?,?) ON CONFLICT(binding) DO UPDATE SET body=excluded.body,hash=excluded.hash", binding, string(body), protocol.Hash(h))
	return err
}

func readLEDEntry(ctx context.Context, q ledRowReader, binding string, revision int64) (e ledOperationEntry, err error) {
	var body, hash string
	err = q.QueryRowContext(ctx, "SELECT body,hash FROM led_operations WHERE binding=? AND revision=?", binding, revision).Scan(&body, &hash)
	if err == nil {
		err = ledJournalDecode(body, hash, &e)
	}
	return
}

func (r *LEDReconciler) beginLEDOperation(ctx context.Context, kind, id string, state *led.State, evidence *Health) (entry ledOperationEntry, err error) {
	s := r.Store
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if disabled, err := s.ledOutputAdmission(ctx, tx); err != nil {
			return err
		} else if disabled {
			return fmt.Errorf("LED output is explicitly disabled")
		}
		if err := checkLEDLayout(ctx, tx, s.config, s.identity, true); err != nil {
			return err
		}
		if err := s.checkInheritedLEDHistory(ctx, tx); err != nil {
			return err
		}
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
			return err
		}
		var layout ledLayout
		if err := protocol.Decode([]byte(raw), &layout, protocol.MaxRequest); err != nil {
			return err
		}
		h, err := readLEDHead(ctx, tx, r.bindingID())
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if h.Issued == math.MaxInt64 {
			return fmt.Errorf("LED operation revision exhausted")
		}
		op := ledOperation{Version: 1, Identity: s.identity, Binding: r.bindingID(), Revision: h.Issued + 1, LayoutHash: protocol.Hash(layout), Kind: kind, ID: id, State: state, IssuedAt: s.now().UTC()}
		baseline, healthy := r.ids()
		switch id {
		case baseline:
			op.Condition = "baseline"
		case healthy:
			op.Condition = "ok"
		default:
			var obsolete int
			if err := tx.QueryRowContext(ctx, "SELECT condition,identity,obsolete FROM led_states WHERE id=? AND binding=?", id, r.bindingID()).Scan(&op.Condition, &op.Ownership, &obsolete); err != nil {
				return err
			}
			if kind == "clear" && obsolete != 1 {
				return fmt.Errorf("LED clear lacks cleanup tombstone")
			}
			if err := s.checkLEDState(tx, id, r.bindingID(), op.Ownership); err != nil {
				return err
			}
		}
		if evidence != nil {
			source := *evidence
			source.LED, source.Bindings, source.LEDOutputs, source.LEDOperations = nil, nil, nil, nil
			op.EvidenceHash, op.EvidenceCondition = protocol.Hash(source), source.Condition
			if state != nil && state.TTL != nil {
				op.Deadline = source.ValidUntil
			}
		}
		entry = ledOperationEntry{Intent: op, Outcome: "pending"}
		if err := validateLEDOperation(entry, layout, s.identity); err != nil {
			return err
		}
		if err := s.capacity(tx, "", 16384, false); err != nil {
			return err
		}
		h.Issued = op.Revision
		if h.Issued > ledOperationRetention {
			floor := h.Issued - ledOperationRetention
			old, err := readLEDEntry(ctx, tx, op.Binding, floor)
			if err != nil {
				return err
			}
			if old.Outcome == "pending" || old.Outcome == "uncertain" {
				h.PrunedUnconfirmed++
			}
			if _, err = tx.ExecContext(ctx, "DELETE FROM led_operations WHERE binding=? AND revision=?", op.Binding, floor); err != nil {
				return err
			}
			h.PrunedThrough = floor
		}
		body, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO led_operations(binding,revision,body,hash) VALUES(?,?,?,?)", op.Binding, op.Revision, string(body), protocol.Hash(entry)); err != nil {
			return err
		}
		return writeLEDHead(ctx, tx, op.Binding, h)
	})
	return
}

func (s *Store) finishLEDOperation(entry ledOperationEntry) error {
	// A received acknowledgement must survive cancellation of the calling pass.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.transaction(ctx, func(tx *sql.Tx) error {
		old, err := readLEDEntry(ctx, tx, entry.Intent.Binding, entry.Intent.Revision)
		if err != nil {
			return err
		}
		if old.Outcome != "pending" || protocol.Hash(old.Intent) != protocol.Hash(entry.Intent) {
			return fmt.Errorf("LED operation completion conflict")
		}
		head, err := readLEDHead(ctx, tx, entry.Intent.Binding)
		if err != nil {
			return err
		}
		body, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE led_operations SET body=?,hash=? WHERE binding=? AND revision=?", string(body), protocol.Hash(entry), entry.Intent.Binding, entry.Intent.Revision); err != nil {
			return err
		}
		if entry.Outcome == "acknowledged" && (head.LastAcknowledged == nil || head.LastAcknowledged.Intent.Revision < entry.Intent.Revision) {
			head.LastAcknowledged = &entry
		}
		return writeLEDHead(ctx, tx, entry.Intent.Binding, head)
	})
}

func (r *LEDReconciler) operationBoundary(stage string) error {
	if r.Store.ledOperationFault != nil {
		return r.Store.ledOperationFault(stage)
	}
	return nil
}

// mutateLED commits intent before I/O, then rechecks evidence and clamps the
// actual TTL. Its intent TTL is an upper bound; only the receipt records what
// was sent. Neither pending nor acknowledged history is replayed on startup.
func (r *LEDReconciler) mutateLED(ctx context.Context, kind, id string, state *led.State, evidence *Health) (sent led.State, until time.Time, err error) {
	entry, err := r.beginLEDOperation(ctx, kind, id, state, evidence)
	if err != nil {
		return sent, until, err
	}
	if err = r.operationBoundary("intent_committed"); err != nil {
		return sent, until, err
	}
	err = ctx.Err()
	if err == nil {
		err = r.checkLEDClearRegistry(ctx, entry.Intent)
	}
	if err == nil && evidence != nil {
		var latest Health
		latest, err = r.sourceHealth(ctx)
		if err == nil && !ledHealthCurrent(*evidence, latest) {
			err = errLEDObservationChanged
		}
	}
	if err == nil && state != nil {
		sent = *state
		if state.TTL != nil {
			ttl := led.HealthyTTL(r.Store.now(), entry.Intent.Deadline, r.Budget, 500*time.Millisecond)
			if ttl > *state.TTL {
				ttl = *state.TTL
			}
			if ttl < 1 {
				err = errLEDObservationChanged
			} else {
				sent.TTL = &ttl
			}
		}
	}
	if err != nil {
		entry.Outcome, entry.FinishedAt = "not_sent", r.Store.now().UTC()
		if finishErr := r.Store.finishLEDOperation(entry); finishErr != nil {
			return sent, until, finishErr
		}
		return sent, until, err
	}
	entry.DispatchedAt = r.Store.now().UTC()
	if state != nil {
		entry.Sent = &sent
		if sent.TTL != nil {
			until = entry.DispatchedAt.Add(time.Duration(*sent.TTL) * time.Second)
		}
		err = r.API.Set(ctx, sent)
	} else {
		err = r.API.Clear(ctx, id)
	}
	if fault := r.operationBoundary("rpc_returned"); fault != nil {
		return sent, until, fault
	}
	entry.Outcome, entry.FinishedAt = "acknowledged", r.Store.now().UTC()
	if err != nil {
		entry.Outcome = "uncertain"
	}
	if finishErr := r.Store.finishLEDOperation(entry); finishErr != nil {
		return sent, until, finishErr
	}
	if fault := r.operationBoundary("ack_committed"); fault != nil {
		return sent, until, fault
	}
	return sent, until, err
}

func (r *LEDReconciler) clearLED(ctx context.Context, id string, evidence *Health) error {
	_, _, err := r.mutateLED(ctx, "clear", id, nil, evidence)
	return err
}
