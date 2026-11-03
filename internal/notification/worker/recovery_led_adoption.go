package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type RecoveryLEDAdoptionRequest struct{ InventoryID, InventoryHash, PreservationHash string }

func (v RecoveryLEDAdoptionRequest) validate() error {
	if !safefs.ValidID(v.InventoryID, "n") || !safefs.DigestPattern.MatchString(v.InventoryHash) || !safefs.DigestPattern.MatchString(v.PreservationHash) {
		return fmt.Errorf("LED adoption requires exact inventory and committed preservation digests")
	}
	return nil
}

type RecoveryLEDAdoptionView struct {
	Operation         string    `json:"operation"`
	InventoryID       string    `json:"inventory_id"`
	InventoryHash     string    `json:"inventory_hash"`
	PreservationHash  string    `json:"preservation_hash"`
	RecordHash        string    `json:"record_hash"`
	State             string    `json:"state"`
	Faults            int       `json:"faults"`
	PreparedAt        time.Time `json:"prepared_at"`
	SourcesMatch      bool      `json:"sources_match"`
	Quarantined       bool      `json:"quarantined"`
	ActivationAllowed bool      `json:"activation_allowed"`
	ClearAllowed      bool      `json:"clear_allowed"`
}

func (e ledAdoptionEvidence) view() RecoveryLEDAdoptionView {
	return RecoveryLEDAdoptionView{Operation: e.Record.Recovery.Operation, InventoryID: e.Record.InventoryID, InventoryHash: e.Record.InventoryHash, PreservationHash: e.Record.PreservationHash, RecordHash: protocol.Hash(e.Record), State: e.Phase, Faults: len(e.After.Faults), PreparedAt: e.Record.PreparedAt, Quarantined: true}
}

// AdoptRecoveryLED changes only the quarantined candidate's owned-fault ledger.
// It retains original IDs/epochs and strips executable cleanup tombstones. It
// never changes the daemon, publishes the candidate or permits runtime startup.
func AdoptRecoveryLED(ctx context.Context, path string, c Config, socket string, request RecoveryLEDAdoptionRequest, api LEDRecoveryObserver) (RecoveryLEDAdoptionView, error) {
	if err := request.validate(); err != nil {
		return RecoveryLEDAdoptionView{}, err
	}
	if !validLEDSocket(socket) {
		return RecoveryLEDAdoptionView{}, fmt.Errorf("LED adoption requires a pinned socket")
	}
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return RecoveryLEDAdoptionView{}, err
	}
	defer r.Close()
	return r.adoptLED(ctx, socket, request, api)
}
func (r *recoveryReconciler) ledAdoptionAuthority(ctx context.Context, tx *sql.Tx, socket string, request RecoveryLEDAdoptionRequest) (ledAdoptionEvidence, recoveryLEDPreservation, error) {
	p, phase, err := r.readLEDPreservation(ctx, tx)
	if err != nil {
		return ledAdoptionEvidence{}, recoveryLEDPreservation{}, err
	}
	if p == nil || phase != "committed" || protocol.Hash(*p) != request.PreservationHash {
		return ledAdoptionEvidence{}, recoveryLEDPreservation{}, fmt.Errorf("LED adoption needs the exact committed preservation")
	}
	inventory, err := r.preservationAuthority(ctx, tx, socket, request.InventoryID, request.InventoryHash)
	if err != nil {
		return ledAdoptionEvidence{}, *p, err
	}
	now, err := r.ledRecoveryTime(ctx, tx)
	if err != nil {
		return ledAdoptionEvidence{}, *p, err
	}
	evidence, err := r.makeLEDAdoption(inventory, *p, now.UTC())
	if err != nil {
		return evidence, *p, err
	}
	// Match current reviewed registry instances, which may reflect separately
	// authorized cleanup after preservation. Old observations never authorize I/O.
	current, err := r.makeLEDPreservation(inventory, now.UTC())
	return evidence, current, err
}
func (r *recoveryReconciler) adoptLED(ctx context.Context, socket string, request RecoveryLEDAdoptionRequest, api LEDRecoveryObserver) (out RecoveryLEDAdoptionView, err error) {
	if err = request.validate(); err != nil {
		return
	}
	if !validLEDSocket(socket) {
		return out, fmt.Errorf("invalid LED adoption socket")
	}
	// The local merge can retain 2,048 faults. Allow its bounded evidence checks
	// and durable writes to finish on slower hosts; registry I/O retains its own
	// three-second deadline and pre-intent freshness is checked independently.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var evidence *ledAdoptionEvidence
	var proposal ledAdoptionEvidence
	var observation recoveryLEDPreservation
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var e error
		evidence, e = r.readLEDAdoption(ctx, tx)
		if e != nil {
			return e
		}
		if evidence != nil {
			v := evidence.Record
			if v.InventoryID != request.InventoryID || v.InventoryHash != request.InventoryHash || v.PreservationHash != request.PreservationHash || v.Socket != socket {
				return fmt.Errorf("LED adoption retry identity changed")
			}
			return nil
		}
		proposal, observation, e = r.ledAdoptionAuthority(ctx, tx, socket, request)
		return e
	})
	if err != nil {
		return
	}
	if evidence == nil {
		if api == nil {
			return out, fmt.Errorf("LED adoption requires a registry observer")
		}
		rpc, stop := context.WithTimeout(ctx, 3*time.Second)
		registry, _, _, e := observeRecoveryLED(rpc, api)
		if e == nil {
			e = rpc.Err()
		}
		stop()
		if e != nil {
			return out, e
		}
		if err = checkPreservedRegistry(observation, registry); err != nil {
			return
		}
		err = r.transaction(ctx, func(tx *sql.Tx) error {
			var e error
			proposal, _, e = r.ledAdoptionAuthority(ctx, tx, socket, request)
			if e != nil {
				return e
			}
			raw, e := json.Marshal(proposal.After)
			if e != nil {
				return e
			}
			if e = r.capacity(tx, len(raw)+16384); e != nil {
				return e
			}
			return ensureLEDAdoptionSchema(ctx, tx)
		})
		if err != nil {
			return
		}
		err = r.transaction(ctx, func(tx *sql.Tx) error {
			existing, e := r.readLEDAdoption(ctx, tx)
			if e != nil {
				return e
			}
			if existing != nil {
				return fmt.Errorf("LED adoption appeared during preparation")
			}
			proposal, _, e = r.ledAdoptionAuthority(ctx, tx, socket, request)
			if e != nil {
				return e
			}
			if e = r.stage.WriteJSON(recoveryLEDAdoptionFile, proposal.Record, false); e != nil {
				return e
			}
			return recoveryBoundary(r.fault, "led_adoption_intent")
		})
		if err != nil {
			return
		}
		evidence = &proposal
	}
	out = evidence.view()
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		current, e := r.readLEDAdoption(ctx, tx)
		if e != nil {
			return e
		}
		if current == nil || current.Record != evidence.Record {
			return fmt.Errorf("LED adoption checkpoint changed")
		}
		sources, e := r.collectLEDSources(ctx, tx)
		if e != nil {
			return e
		}
		if protocol.Hash(sources) != current.Record.SourcesHash {
			return fmt.Errorf("LED adoption sources changed")
		}
		if current.Phase == "committed" {
			return nil
		}
		raw, e := json.Marshal(current.After)
		if e != nil {
			return e
		}
		if e = r.capacity(tx, len(raw)+16384); e != nil {
			return e
		}
		if e = commitLEDAdoption(ctx, tx, *current, r.fault); e != nil {
			return e
		}
		if _, e = r.readLEDAdoption(ctx, tx); e != nil {
			return e
		}
		return recoveryBoundary(r.fault, "led_adoption_transaction")
	})
	if err != nil {
		return
	}
	evidence.Phase = "committed"
	out = evidence.view()
	out.SourcesMatch = true
	err = recoveryBoundary(r.fault, "led_adoption_commit")
	return
}
