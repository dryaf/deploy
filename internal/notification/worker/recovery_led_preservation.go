package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Preservation retains provenance for later candidate publication. It neither
// relabels old fault identities nor grants permission to recreate/clear a state.
// The original led_states and both source databases remain unchanged.
type recoveryLEDPreservedFault struct {
	ID, Binding, Condition, Identity             string
	UID                                          int
	Snapshot, Latest                             bool
	SnapshotCleanupPending, LatestCleanupPending bool
	ObservedHash                                 string
}
type recoveryLEDUnownedState struct {
	ID        string
	UID       int
	StateHash string
}
type recoveryLEDPreservation struct {
	Version                                                                 int
	Recovery                                                                recoveryBinding
	ConfigHash, InventoryID, InventoryHash, Socket, SourcesHash, LayoutHash string
	PreparedAt                                                              time.Time
	Faults                                                                  []recoveryLEDPreservedFault
	Unowned                                                                 []recoveryLEDUnownedState
}

type RecoveryLEDPreservationView struct {
	Version            int       `json:"version"`
	Operation          string    `json:"operation"`
	InventoryID        string    `json:"inventory_id"`
	InventoryHash      string    `json:"inventory_hash"`
	RecordHash         string    `json:"record_hash"`
	State              string    `json:"state"`
	PreparedAt         time.Time `json:"prepared_at"`
	Faults             int       `json:"faults"`
	PostSnapshotFaults int       `json:"post_snapshot_faults"`
	SnapshotOnlyFaults int       `json:"snapshot_only_faults"`
	UnownedStates      int       `json:"unowned_states"`
	SourcesMatch       bool      `json:"sources_match"`
	Quarantined        bool      `json:"quarantined"`
	ActivationAllowed  bool      `json:"activation_allowed"`
	ClearAllowed       bool      `json:"clear_allowed"`
}

func (v recoveryLEDPreservation) view(state string) RecoveryLEDPreservationView {
	out := RecoveryLEDPreservationView{Version: 1, Operation: v.Recovery.Operation, InventoryID: v.InventoryID, InventoryHash: v.InventoryHash, RecordHash: protocol.Hash(v), State: state, PreparedAt: v.PreparedAt, Faults: len(v.Faults), UnownedStates: len(v.Unowned), Quarantined: true}
	for _, f := range v.Faults {
		if f.Latest && !f.Snapshot {
			out.PostSnapshotFaults++
		}
		if f.Snapshot && !f.Latest {
			out.SnapshotOnlyFaults++
		}
	}
	return out
}

func (r *recoveryReconciler) makeLEDPreservation(v recoveryLEDInventory, at time.Time) (out recoveryLEDPreservation, err error) {
	if err = r.validateLEDInventory(v); err != nil {
		return
	}
	view := r.ledInventoryView(v, v.Sources, nil)
	if !view.MappingsMatch || !view.RuntimeUIDMatches || at.Before(v.ObservedAt) || !at.Before(v.ValidUntil) {
		return out, fmt.Errorf("LED preservation needs matching pinned layouts, UID and bounded observation")
	}
	out = recoveryLEDPreservation{Version: 1, Recovery: r.journal.binding(), ConfigHash: protocol.Hash(r.config), InventoryID: v.ID, InventoryHash: protocol.Hash(v), Socket: v.Socket, SourcesHash: protocol.Hash(v.Sources), LayoutHash: protocol.Hash(v.Sources.Snapshot.Layout), PreparedAt: at}
	faults := map[string]recoveryLEDPreservedFault{}
	for _, item := range view.Items {
		if item.Snapshot != nil || item.Latest != nil {
			if item.Classification != "known_fault" || !item.OwnershipVerified || item.Conflict != "" {
				return out, fmt.Errorf("LED fault ownership is unresolved")
			}
			faults[item.ID] = recoveryLEDPreservedFault{ID: item.ID, Binding: item.Binding, UID: item.UID, Snapshot: item.Snapshot != nil, Latest: item.Latest != nil}
			f := faults[item.ID]
			if item.Observed != nil {
				f.ObservedHash = item.Observed.StateHash
			}
			faults[item.ID] = f
		} else if item.Classification == "conflict" {
			return out, fmt.Errorf("LED reserved namespace conflicts with registry")
		} else if item.Observed != nil && (item.Classification == "foreign" || item.Classification == "unrecognized") {
			out.Unowned = append(out.Unowned, recoveryLEDUnownedState{item.ID, item.UID, item.Observed.StateHash})
		}
	}
	for index, source := range []recoveryLEDSource{v.Sources.Snapshot, v.Sources.Latest} {
		for _, f := range source.Faults {
			merged := faults[f.ID]
			merged.Condition, merged.Identity = f.Condition, f.Identity
			if index == 0 {
				merged.SnapshotCleanupPending = f.Obsolete
			} else {
				merged.LatestCleanupPending = f.Obsolete
			}
			faults[f.ID] = merged
		}
	}
	if len(faults) > 2*maxRecoveryLEDFaults {
		return out, fmt.Errorf("LED preservation fault budget exceeded")
	}
	for _, f := range faults {
		out.Faults = append(out.Faults, f)
	}
	slices.SortFunc(out.Faults, func(a, b recoveryLEDPreservedFault) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return out, nil
}

// PreserveRecoveryLED requires an explicitly reviewed inventory ID and digest.
// Retrying an intent never refreshes its observation, learns new ownership or
// repeats daemon calls. No runtime activation or daemon mutation occurs.
func PreserveRecoveryLED(ctx context.Context, path string, c Config, socket, inventory, hash string, api LEDRecoveryObserver) (RecoveryLEDPreservationView, error) {
	if !validLEDSocket(socket) || !safefs.ValidID(inventory, "n") || !safefs.DigestPattern.MatchString(hash) {
		return RecoveryLEDPreservationView{}, fmt.Errorf("LED preservation requires an exact inventory ID/hash and socket")
	}
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return RecoveryLEDPreservationView{}, err
	}
	defer r.Close()
	return r.preserveLED(ctx, socket, inventory, hash, api)
}

func (r *recoveryReconciler) preservationAuthority(ctx context.Context, tx *sql.Tx, socket, id, hash string) (recoveryLEDInventory, error) {
	set, err := r.readLEDInventorySet(ctx, tx)
	if err != nil {
		return recoveryLEDInventory{}, err
	}
	if set.Pending || set.Head == nil || set.Head.ID != id || protocol.Hash(*set.Head) != hash || set.Head.Socket != socket {
		return recoveryLEDInventory{}, fmt.Errorf("LED preservation requires the current sealed inventory")
	}
	sources, err := r.collectLEDSources(ctx, tx)
	if err != nil {
		return recoveryLEDInventory{}, err
	}
	view := r.ledInventoryView(*set.Head, sources, nil)
	if !view.SourcesMatch || !view.ObservationCurrent || !view.MappingsMatch || !view.RuntimeUIDMatches {
		return recoveryLEDInventory{}, fmt.Errorf("LED preservation inventory is stale or changed")
	}
	return *set.Head, nil
}

func checkPreservedRegistry(record recoveryLEDPreservation, registry []RecoveryLEDRegistryState) error {
	current := map[string]string{}
	for _, s := range registry {
		if s.UID == os.Getuid() {
			current[s.ID] = s.StateHash
		}
	}
	for _, f := range record.Faults {
		if current[f.ID] != f.ObservedHash {
			return fmt.Errorf("LED fault registry incarnation changed since review")
		}
	}
	return nil
}

func (r *recoveryReconciler) preserveLED(ctx context.Context, socket, id, hash string, api LEDRecoveryObserver) (out RecoveryLEDPreservationView, err error) {
	if !validLEDSocket(socket) || !safefs.ValidID(id, "n") || !safefs.DigestPattern.MatchString(hash) {
		return out, fmt.Errorf("invalid LED preservation request")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var record *recoveryLEDPreservation
	var state string
	var inventory recoveryLEDInventory
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var e error
		record, state, e = r.readLEDPreservation(ctx, tx)
		if e != nil {
			return e
		}
		if record != nil {
			if record.InventoryID != id || record.InventoryHash != hash || record.Socket != socket {
				return fmt.Errorf("LED preservation is already pinned to another reviewed inventory")
			}
			return nil
		}
		inventory, e = r.preservationAuthority(ctx, tx, socket, id, hash)
		return e
	})
	if err != nil {
		return
	}
	if record == nil {
		if api == nil {
			return out, fmt.Errorf("LED preservation requires a registry observer")
		}
		var proposed recoveryLEDPreservation
		proposed, err = r.makeLEDPreservation(inventory, r.now().UTC())
		if err != nil {
			return
		}
		// Registry I/O never holds the worker database mutex.
		rpc, cancel := context.WithTimeout(ctx, 3*time.Second)
		registry, _, _, e := observeRecoveryLED(rpc, api)
		if e == nil {
			e = rpc.Err()
		}
		cancel()
		if e != nil {
			return out, e
		}
		if err = checkPreservedRegistry(proposed, registry); err != nil {
			return
		}
		err = r.transaction(ctx, func(tx *sql.Tx) error {
			fresh, e := r.preservationAuthority(ctx, tx, socket, id, hash)
			if e != nil {
				return e
			}
			now, e := r.ledRecoveryTime(ctx, tx)
			if e != nil {
				return e
			}
			proposed, e = r.makeLEDPreservation(fresh, now.UTC())
			if e != nil {
				return e
			}
			existing, _, e := r.readLEDPreservation(ctx, tx)
			if e != nil {
				return e
			}
			if existing != nil {
				return fmt.Errorf("LED preservation appeared during preparation")
			}
			raw, e := json.Marshal(proposed)
			if e != nil || len(raw) > maxRecoveryLEDInventoryBytes {
				return fmt.Errorf("LED preservation exceeds storage budget")
			}
			if e = r.capacity(tx, len(raw)+8192); e != nil {
				return e
			}
			if e = ensureLEDPreservationSchema(ctx, tx); e != nil {
				return e
			}
			return nil
		})
		if err != nil {
			return
		}
		// The schema commits before its independent filesystem intent. Reopening can
		// distinguish a new preparation from a database rollback that lost its schema.
		err = r.transaction(ctx, func(tx *sql.Tx) error {
			fresh, e := r.preservationAuthority(ctx, tx, socket, id, hash)
			if e != nil {
				return e
			}
			now, e := r.ledRecoveryTime(ctx, tx)
			if e != nil {
				return e
			}
			proposed, e = r.makeLEDPreservation(fresh, now.UTC())
			if e != nil {
				return e
			}
			existing, _, e := r.readLEDPreservation(ctx, tx)
			if e != nil {
				return e
			}
			if existing != nil {
				return fmt.Errorf("LED preservation appeared before intent publication")
			}
			if e = r.stage.WriteJSON(recoveryLEDPreservationFile, proposed, false); e != nil {
				return e
			}
			return recoveryBoundary(r.fault, "led_preservation_intent")
		})
		if err != nil {
			return
		}

		record = &proposed
		state = "prepared"
	}
	out = record.view(state)
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		saved, phase, e := r.readLEDPreservation(ctx, tx)
		if e != nil {
			return e
		}
		if saved == nil || protocol.Hash(*saved) != protocol.Hash(*record) {
			return fmt.Errorf("LED preservation intent changed")
		}
		sources, e := r.collectLEDSources(ctx, tx)
		if e != nil {
			return e
		}
		if protocol.Hash(sources) != record.SourcesHash {
			return fmt.Errorf("LED preservation source ownership changed")
		}
		if phase == "committed" {
			out.SourcesMatch = true
			return nil
		}
		raw, e := json.Marshal(record)
		if e != nil {
			return e
		}
		if e = r.capacity(tx, len(raw)+8192); e != nil {
			return e
		}
		if e = commitLEDPreservation(ctx, tx, *record); e != nil {
			return e
		}
		return recoveryBoundary(r.fault, "led_preservation_transaction")
	})
	if err != nil {
		return
	}
	out = record.view("committed")
	out.SourcesMatch = true
	err = recoveryBoundary(r.fault, "led_preservation_commit")
	return
}
