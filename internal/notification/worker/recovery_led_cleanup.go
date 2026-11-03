package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

// LEDRecoveryCleaner cannot publish healthy state or change any daemon setting.
type LEDRecoveryCleaner interface {
	LEDRecoveryObserver
	Clear(context.Context, string) error
}

// RecoveryLEDDomainGuard is supplied by the producer coordinator, never by chat
// input. It must check the producer's fenced ownership, immutable acknowledged
// report and freshly evaluated domain basis. The coordinator retains its stable
// source leases for the complete call. The guard runs without a worker DB lock.
type RecoveryLEDDomainGuard = protocol.RecoveryLEDDomainGuard

func observeLEDClearTarget(ctx context.Context, api LEDRecoveryObserver, intent recoveryLEDClearIntent) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	registry, _, _, err := observeRecoveryLED(ctx, api)
	if err != nil {
		return false, err
	}
	for _, state := range registry {
		if state.UID == intent.UID && state.ID == intent.FaultID {
			if intent.Observed == nil || state.StateHash != intent.Observed.StateHash {
				return false, fmt.Errorf("LED registry incarnation changed since inventory")
			}
			return false, nil
		}
	}
	return true, nil
}

type recoveryLEDCleanupSession struct {
	*recoveryReconciler
	callMu sync.Mutex
	socket string
	api    LEDRecoveryCleaner
	closed bool
}

func OpenRecoveryLEDCleanupSession(ctx context.Context, path string, c Config, socket string, api LEDRecoveryCleaner) (protocol.RecoveryLEDCleanupSession, error) {
	if !validLEDSocket(socket) || api == nil {
		return nil, fmt.Errorf("LED cleanup requires pinned socket and client")
	}
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return nil, err
	}
	return &recoveryLEDCleanupSession{recoveryReconciler: r, socket: socket, api: api}, nil
}

func (s *recoveryLEDCleanupSession) ClearLED(ctx context.Context, request protocol.RecoveryLEDClearRequest, guard RecoveryLEDDomainGuard) (RecoveryLEDClearResult, error) {
	if !s.callMu.TryLock() {
		return RecoveryLEDClearResult{}, fmt.Errorf("LED cleanup session busy")
	}
	defer s.callMu.Unlock()
	if s.closed {
		return RecoveryLEDClearResult{}, fmt.Errorf("LED cleanup session closed")
	}
	return s.clearLEDFault(ctx, s.socket, request.InventoryID, request.Binding, request.FaultID, request.EvidenceHash, s.api, guard)
}

// Keep the exclusive worker lease until any bounded daemon RPC and its outcome
// persistence finish. A concurrent shutdown must not let another owner start.
func (s *recoveryLEDCleanupSession) Close() error {
	s.callMu.Lock()
	defer s.callMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.recoveryReconciler.Close()
}

// ClearRecoveryLEDFault performs one exact, freshly authorized cleanup attempt.
// The inventory and evidence hash must be taken from a reviewed proposal. This
// does not publish recovered state, activate delivery or acknowledge visual output.
func ClearRecoveryLEDFault(ctx context.Context, path string, c Config, socket string, request protocol.RecoveryLEDClearRequest, api LEDRecoveryCleaner, guard RecoveryLEDDomainGuard) (RecoveryLEDClearResult, error) {
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return RecoveryLEDClearResult{}, err
	}
	defer r.Close()
	return r.clearLEDFault(ctx, socket, request.InventoryID, request.Binding, request.FaultID, request.EvidenceHash, api, guard)
}

func (r *recoveryReconciler) ledClearAuthority(ctx context.Context, tx *sql.Tx, socket, inventory, binding, id, evidence string) (intent recoveryLEDClearIntent, proof RecoveryLEDEvidence, err error) {
	set, err := r.readLEDInventorySet(ctx, tx)
	if err != nil {
		return intent, proof, err
	}
	if set.Pending || set.Head == nil || set.Head.ID != inventory || set.Head.Socket != socket {
		return intent, proof, fmt.Errorf("LED cleanup needs the current sealed inventory and socket")
	}
	v := *set.Head
	sources, err := r.collectLEDSources(ctx, tx)
	if err != nil {
		return intent, proof, err
	}
	view := r.ledInventoryView(v, sources, nil)
	if err = r.appendLEDResolutionReview(ctx, tx, v, &view); err != nil {
		return intent, proof, err
	}
	proposal, ok := view.Resolutions[binding]
	if !ok || !proposal.DomainResolved || proposal.EvidenceHash != evidence || !slices.Contains(proposal.ProposedFaultIDs, id) {
		return intent, proof, fmt.Errorf("LED cleanup lacks current exact domain resolution")
	}
	now, err := r.ledRecoveryTime(ctx, tx)
	if err != nil {
		return intent, proof, err
	}
	if !proposal.ValidUntil.After(now.Add(2500 * time.Millisecond)) {
		return intent, proof, fmt.Errorf("LED cleanup evidence cannot cover RPC budget")
	}
	for _, item := range view.Items {
		if item.ID == id && item.Binding == binding && item.OwnershipVerified && item.Classification == "known_fault" {
			intent = recoveryLEDClearIntent{Version: 1, Recovery: r.journal.binding(), InventoryID: inventory, InventoryHash: protocol.Hash(v), Socket: socket, Binding: binding, FaultID: id, EvidenceHash: evidence, UID: item.UID, Observed: item.Observed, IssuedAt: now.UTC(), Deadline: proposal.ValidUntil}
		}
	}
	for _, b := range r.config.LEDBindings {
		if b.ID == binding {
			slot, e := r.readLEDEvidenceSlot(ctx, tx, v, b.Producer, true)
			if e != nil {
				return intent, proof, e
			}
			if slot.Reply == nil || !slot.ReplyCommitted {
				return intent, proof, fmt.Errorf("LED proof missing")
			}
			proof = *slot.Reply
		}
	}
	return
}

func (r *recoveryReconciler) clearLEDFault(ctx context.Context, socket, inventory, binding, id, evidence string, api LEDRecoveryCleaner, guard RecoveryLEDDomainGuard) (out RecoveryLEDClearResult, err error) {
	if binding == workerLEDBinding || !validLEDSocket(socket) {
		return out, fmt.Errorf("LED cleanup scope unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err = r.ensureLEDClears(ctx); err != nil {
		return
	}
	var intent recoveryLEDClearIntent
	var proof RecoveryLEDEvidence
	var replay bool
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		records, e := r.syncLEDClears(ctx, tx)
		if e != nil {
			return e
		}
		for _, record := range records {
			v := record.Intent
			if v.InventoryID == inventory && v.Binding == binding && v.FaultID == id {
				if v.EvidenceHash != evidence || v.Socket != socket {
					return fmt.Errorf("LED cleanup retry identity changed")
				}
				replay = true
				out = v.result("uncertain", time.Time{})
				if record.Result != nil {
					out = *record.Result
				}
				return nil
			}
		}
		if len(records) >= maxRecoveryLEDClears {
			return fmt.Errorf("LED cleanup history exhausted")
		}
		intent, proof, e = r.ledClearAuthority(ctx, tx, socket, inventory, binding, id, evidence)
		return e
	})
	if err != nil || replay {
		return
	}
	if api == nil || guard == nil {
		return out, fmt.Errorf("LED cleanup requires registry and producer domain guard")
	}
	if err = guard(ctx, proof); err != nil {
		return
	}
	// Status/list are fresh observations, outside the worker transaction. Only the
	// exact owned ID is inspected; unrelated states and winner changes are harmless.
	absent, err := observeLEDClearTarget(ctx, api, intent)
	if err != nil {
		return out, err
	}
	// Persist intent before the only mutation. The independent head survives SQL
	// rollback; seeing it on retry means historical uncertainty, never permission.
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		fresh, _, e := r.ledClearAuthority(ctx, tx, socket, inventory, binding, id, evidence)
		if e != nil {
			return e
		}
		records, e := r.syncLEDClears(ctx, tx)
		if e != nil {
			return e
		}
		if len(records) >= maxRecoveryLEDClears {
			return fmt.Errorf("LED cleanup history exhausted")
		}
		for _, old := range records {
			if old.Intent.InventoryID == inventory && old.Intent.Binding == binding && old.Intent.FaultID == id {
				return fmt.Errorf("LED cleanup appeared during preparation")
			}
		}
		if protocol.Hash(fresh.Observed) != protocol.Hash(intent.Observed) || fresh.InventoryHash != intent.InventoryHash {
			return fmt.Errorf("LED cleanup authority changed")
		}
		intent = fresh
		intent.Sequence = len(records) + 1
		if len(records) > 0 {
			intent.PreviousHash = protocol.Hash(records[len(records)-1].Intent)
		}
		if e = r.capacity(tx, 16384); e != nil {
			return e
		}
		if e = r.stage.WriteJSON(recoveryLEDClearHead, intent, len(records) > 0); e != nil {
			return e
		}
		if e = recoveryBoundary(r.fault, "led_clear_intent"); e != nil {
			return e
		}
		if e = r.stage.WriteJSON(ledClearFile(intent, "intent"), intent, false); e != nil {
			return e
		}
		raw, _ := json.Marshal(intent)
		if _, e = tx.ExecContext(ctx, "INSERT INTO reconciliation_led_clears(sequence,hash,body) VALUES(?,?,?)", intent.Sequence, protocol.Hash(intent), string(raw)); e != nil {
			return e
		}
		return recoveryBoundary(r.fault, "led_clear_transaction")
	})
	if err != nil {
		return
	}
	if err = recoveryBoundary(r.fault, "led_clear_committed"); err != nil {
		return
	}
	out = intent.result("not_sent", time.Time{})
	// Producer drift during registry reads or journal persistence blocks dispatch.
	absent, err = observeLEDClearTarget(ctx, api, intent)
	if err == nil {
		err = guard(ctx, proof)
	}
	if err == nil {
		err = r.transaction(ctx, func(tx *sql.Tx) error {
			_, _, e := r.ledClearAuthority(ctx, tx, socket, inventory, binding, id, evidence)
			return e
		})
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && absent {
		out.Outcome = "observed_absent"
	} else if err == nil {
		rpcCtx, stopRPC := context.WithTimeout(ctx, 2*time.Second)
		err = api.Clear(rpcCtx, id)
		if err == nil {
			err = rpcCtx.Err()
		}
		stopRPC()
		out.Outcome = "uncertain"
		if err == nil {
			out.Outcome = "acknowledged"
		}
		if e := recoveryBoundary(r.fault, "led_clear_returned"); e != nil {
			return out, e
		}
	}
	// Cancellation cannot erase knowledge that a mutation may have been sent.
	finishCtx, stopFinish := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopFinish()
	finishErr := r.transaction(finishCtx, func(tx *sql.Tx) error {
		records, e := r.readLEDClears(finishCtx, tx)
		if e != nil {
			return e
		}
		if len(records) != intent.Sequence || protocol.Hash(records[len(records)-1].Intent) != protocol.Hash(intent) {
			return fmt.Errorf("LED cleanup journal changed during dispatch")
		}
		out.FinishedAt = r.now().UTC()
		if out.FinishedAt.Before(intent.IssuedAt) {
			out.FinishedAt = intent.IssuedAt
		}
		if e = r.stage.WriteJSON(ledClearFile(intent, "result"), out, false); e != nil {
			return e
		}
		if e = recoveryBoundary(r.fault, "led_clear_result"); e != nil {
			return e
		}
		raw, _ := json.Marshal(out)
		if _, e = tx.ExecContext(finishCtx, "UPDATE reconciliation_led_clears SET result=? WHERE sequence=?", string(raw), intent.Sequence); e != nil {
			return e
		}
		return recoveryBoundary(r.fault, "led_clear_result_transaction")
	})
	if finishErr == nil {
		finishErr = recoveryBoundary(r.fault, "led_clear_result_committed")
	}
	return out, errors.Join(err, finishErr)
}
