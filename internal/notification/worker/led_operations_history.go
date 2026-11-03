package worker

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func validateLEDOperation(e ledOperationEntry, layout ledLayout, identity protocol.Identity) error {
	op := e.Intent
	fail := func() error { return fmt.Errorf("invalid LED operation journal record") }
	if op.Version != 1 || op.Identity != identity || op.Revision <= 0 || op.LayoutHash != protocol.Hash(layout) || op.IssuedAt.IsZero() {
		return fail()
	}
	target, styles := "green", standardLEDStyles()
	if op.Binding != workerLEDBinding {
		i := slices.IndexFunc(layout.Bindings, func(b LEDBinding) bool { return b.ID == op.Binding })
		if i < 0 {
			return fail()
		}
		target, styles = layout.Bindings[i].LED, layout.Bindings[i].Styles
	}
	base := ledBindingNamespace(identity.Worker, op.Binding)
	baseline, healthy := op.ID == base+".baseline", op.ID == base+".healthy"
	if baseline || healthy {
		want := "baseline"
		if healthy {
			want = "ok"
		}
		if op.Ownership != "" || op.Condition != want {
			return fail()
		}
	} else {
		if !slices.Contains([]string{"unknown", "paused", "warning", "critical"}, op.Condition) {
			return fail()
		}
		if op.Ownership == "legacy" {
			if op.Binding != workerLEDBinding || !slices.Contains(layout.Legacy, op.ID) {
				return fail()
			}
		} else {
			var owned ledStateIdentity
			if protocol.Decode([]byte(op.Ownership), &owned, 4096) != nil || owned.Worker != identity.Worker || owned.Recovery != identity.Recovery || owned.Binding != op.Binding || !safefs.ValidID(owned.Nonce, "n") || op.ID != "deploy.f."+protocol.Hash(owned)[:48] {
				return fail()
			}
		}
	}
	guarded := (!baseline && op.Kind == "set") || (!healthy && op.Kind == "clear")
	if guarded {
		if !safefs.DigestPattern.MatchString(op.EvidenceHash) || !slices.Contains([]string{"ok", "unknown", "paused", "warning", "critical", "retired"}, op.EvidenceCondition) {
			return fail()
		}
	} else if op.EvidenceHash != "" || op.EvidenceCondition != "" {
		return fail()
	}
	if op.Kind == "set" {
		if op.State == nil {
			return fail()
		}
		style, ok := styles[op.Condition]
		if !ok || op.State.ID != op.ID || op.State.LED != target || op.State.Pattern != style.Pattern || op.State.Priority != style.Priority {
			return fail()
		}
		if healthy || op.Condition == "paused" {
			if op.State.Persistent || op.State.TTL == nil || *op.State.TTL < 1 || *op.State.TTL > 90 || op.Deadline.IsZero() {
				return fail()
			}
		} else if !op.State.Persistent || op.State.TTL != nil || !op.Deadline.IsZero() {
			return fail()
		}
	} else if op.Kind == "clear" {
		if baseline || op.State != nil || !op.Deadline.IsZero() {
			return fail()
		}
		if !healthy && !slices.Contains([]string{"ok", "warning", "critical", "retired"}, op.EvidenceCondition) {
			return fail()
		}
	} else {
		return fail()
	}
	switch e.Outcome {
	case "pending", "not_sent":
		if e.Sent != nil || !e.DispatchedAt.IsZero() || (e.Outcome == "pending" && !e.FinishedAt.IsZero()) || (e.Outcome == "not_sent" && e.FinishedAt.IsZero()) {
			return fail()
		}
	case "acknowledged", "uncertain":
		if e.DispatchedAt.IsZero() || e.FinishedAt.IsZero() {
			return fail()
		}
		if op.State == nil {
			if e.Sent != nil {
				return fail()
			}
			break
		}
		if e.Sent == nil {
			return fail()
		}
		expected := *op.State
		if expected.TTL != nil {
			if e.Sent.TTL == nil || *e.Sent.TTL < 1 || *e.Sent.TTL > *expected.TTL || e.DispatchedAt.Add(time.Duration(*e.Sent.TTL)*time.Second).After(op.Deadline) {
				return fail()
			}
			expected.TTL = e.Sent.TTL
		}
		if protocol.Hash(expected) != protocol.Hash(*e.Sent) {
			return fail()
		}
	default:
		return fail()
	}
	return nil
}

type ledJournalReader interface {
	ledRowReader
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Validation also serves offline snapshot/recovery callers. They pass the
// original identity, never the new recovery epoch, for this archived history.
func checkLEDOperations(ctx context.Context, q ledJournalReader, identity protocol.Identity) error {
	_, err := readLEDOperations(ctx, q, identity)
	return err
}

func readLEDOperations(ctx context.Context, q ledJournalReader, identity protocol.Identity) (map[string]LEDOperationStatus, error) {
	var raw string
	if err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
		return nil, err
	}
	var layout ledLayout
	if err := protocol.Decode([]byte(raw), &layout, protocol.MaxRequest); err != nil {
		return nil, err
	}
	return readLEDOperationHistory(ctx, q, identity, layout, "")
}

func readLEDOperationHistory(ctx context.Context, q ledJournalReader, identity protocol.Identity, layout ledLayout, archive string) (map[string]LEDOperationStatus, error) {
	headsSQL, entriesSQL, args := ledHistoryQueries(archive)
	rows, err := q.QueryContext(ctx, headsSQL, args...)
	if err != nil {
		return nil, err
	}
	heads := map[string]ledOperationHead{}
	for rows.Next() {
		var binding, body, hash string
		if err = rows.Scan(&binding, &body, &hash); err != nil {
			break
		}
		var h ledOperationHead
		if err = ledJournalDecode(body, hash, &h); err != nil {
			break
		}
		if (binding != workerLEDBinding && !slices.ContainsFunc(layout.Bindings, func(b LEDBinding) bool { return b.ID == binding })) || h.Issued < 1 || h.PrunedThrough != max(int64(0), h.Issued-ledOperationRetention) || h.PrunedUnconfirmed < 0 || h.PrunedUnconfirmed > h.PrunedThrough {
			err = fmt.Errorf("invalid LED operation head")
			break
		}
		if h.LastAcknowledged != nil {
			if err = validateLEDOperation(*h.LastAcknowledged, layout, identity); err != nil {
				break
			}
			if h.LastAcknowledged.Outcome != "acknowledged" || h.LastAcknowledged.Intent.Binding != binding || h.LastAcknowledged.Intent.Revision > h.Issued {
				err = fmt.Errorf("invalid LED acknowledgement head")
				break
			}
		}
		heads[binding] = h
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(heads) > 33 {
		return nil, fmt.Errorf("LED operation head budget exceeded")
	}
	rows, err = q.QueryContext(ctx, entriesSQL, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]LEDOperationStatus{}
	counts := map[string]int64{}
	for rows.Next() {
		var binding, body, hash string
		var revision int64
		if err = rows.Scan(&binding, &revision, &body, &hash); err != nil {
			return nil, err
		}
		h, ok := heads[binding]
		if !ok || revision != h.PrunedThrough+counts[binding]+1 || revision > h.Issued {
			return nil, fmt.Errorf("LED operation history gap")
		}
		var e ledOperationEntry
		if err = ledJournalDecode(body, hash, &e); err != nil {
			return nil, err
		}
		if err = validateLEDOperation(e, layout, identity); err != nil {
			return nil, err
		}
		if e.Intent.Binding != binding || e.Intent.Revision != revision {
			return nil, fmt.Errorf("LED operation key mismatch")
		}
		counts[binding]++
		status := out[binding]
		if e.Outcome == "pending" {
			status.Pending++
		}
		if e.Outcome == "uncertain" {
			status.Uncertain++
		}
		if e.Outcome == "acknowledged" && (h.LastAcknowledged == nil || h.LastAcknowledged.Intent.Revision < revision) {
			return nil, fmt.Errorf("missing LED acknowledgement head")
		}
		if h.LastAcknowledged != nil && h.LastAcknowledged.Intent.Revision == revision && protocol.Hash(*h.LastAcknowledged) != protocol.Hash(e) {
			return nil, fmt.Errorf("LED acknowledgement mismatch")
		}
		out[binding] = status
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for binding, h := range heads {
		if counts[binding] != h.Issued-h.PrunedThrough {
			return nil, fmt.Errorf("incomplete LED operation history")
		}
		status := out[binding]
		status.LayoutHash = protocol.Hash(layout)
		status.Issued, status.PrunedThrough, status.PrunedUnconfirmed = h.Issued, h.PrunedThrough, h.PrunedUnconfirmed
		if h.LastAcknowledged != nil {
			ack := h.LastAcknowledged
			status.LastAcknowledgedRevision, status.LastAcknowledgedID, status.LastAcknowledgedKind, status.LastAcknowledgedAt = ack.Intent.Revision, ack.Intent.ID, ack.Intent.Kind, ack.FinishedAt
		}
		out[binding] = status
	}
	return out, nil
}

// LEDOperations is exposed by system.status separately from source health, so
// reconciliation does not repeatedly scan its own history while checking leases.
func (s *Store) LEDOperations(ctx context.Context) (out map[string]LEDOperationStatus, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var e error
		out, e = readLEDOperations(ctx, tx, s.identity)
		return e
	})
	return
}

func ledHistoryQueries(archive string) (string, string, []any) {
	if archive == "" {
		return "SELECT binding,body,hash FROM led_operation_heads ORDER BY binding LIMIT 34", "SELECT binding,revision,body,hash FROM led_operations ORDER BY binding,revision LIMIT 2113", nil
	}
	return "SELECT binding,body,hash FROM led_migration_heads WHERE migration=? ORDER BY binding LIMIT 34", "SELECT binding,revision,body,hash FROM led_migration_operations WHERE migration=? ORDER BY binding,revision LIMIT 2113", []any{archive}
}
