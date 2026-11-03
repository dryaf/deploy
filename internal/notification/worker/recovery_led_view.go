package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// LEDRecoveryObserver deliberately has no mutation methods. Inventory collection
// cannot clear a fault, renew a healthy lease or configure the LED daemon.
type LEDRecoveryObserver interface {
	Status(context.Context) (led.DaemonStatus, error)
	List(context.Context) ([]led.Entry, error)
}

type RecoveryLEDRegistryState struct {
	ID         string `json:"id"`
	UID        int    `json:"uid"`
	LED        string `json:"led"`
	Pattern    string `json:"pattern"`
	Priority   int    `json:"priority"`
	Persistent bool   `json:"persistent"`
	TTL        *int   `json:"ttl_seconds,omitempty"`
	Winner     bool   `json:"winner"`
	StateHash  string `json:"state_hash"`
}

type RecoveryLEDTarget struct {
	Enabled        bool   `json:"enabled"`
	Available      bool   `json:"available"`
	DefaultPattern string `json:"default_pattern"`
}

type recoveryLEDInventory struct {
	Version      int                          `json:"version"`
	ID           string                       `json:"id"`
	Sequence     int                          `json:"sequence"`
	PreviousHash string                       `json:"previous_hash,omitempty"`
	Recovery     recoveryBinding              `json:"recovery"`
	ConfigHash   string                       `json:"config_hash"`
	Socket       string                       `json:"socket"`
	ObservedAt   time.Time                    `json:"observed_at"`
	ValidUntil   time.Time                    `json:"valid_until"`
	BootID       string                       `json:"boot_id"`
	Uptime       float64                      `json:"uptime"`
	Sources      recoveryLEDSources           `json:"sources"`
	Registry     []RecoveryLEDRegistryState   `json:"registry"`
	Degraded     bool                         `json:"daemon_degraded"`
	Targets      map[string]RecoveryLEDTarget `json:"targets"`
}

type RecoveryLEDFaultView struct {
	Condition      string `json:"condition"`
	CleanupPending bool   `json:"cleanup_pending"`
	IdentityHash   string `json:"identity_hash"`
}

type RecoveryLEDItem struct {
	ID                string                    `json:"id"`
	UID               int                       `json:"uid"`
	Binding           string                    `json:"binding,omitempty"`
	Classification    string                    `json:"classification"`
	Provenance        string                    `json:"provenance"`
	OwnershipVerified bool                      `json:"ownership_verified"`
	Snapshot          *RecoveryLEDFaultView     `json:"snapshot,omitempty"`
	Latest            *RecoveryLEDFaultView     `json:"latest,omitempty"`
	Observed          *RecoveryLEDRegistryState `json:"observed,omitempty"`
	Conflict          string                    `json:"conflict,omitempty"`
}

// RecoveryLEDReview is diagnostic evidence. Even a current, conflict-free review
// does not supply domain resolution, physical verification or activation authority.
type RecoveryLEDReview struct {
	CleanupImports              []RecoveryLEDImportView                 `json:"cleanup_imports,omitempty"`
	ImportableCleanup           []RecoveryLEDImportProposal             `json:"importable_cleanup,omitempty"`
	ImportableCleanupCount      int                                     `json:"importable_cleanup_count"`
	EpochHandoff                *RecoveryLEDEpochView                   `json:"epoch_handoff,omitempty"`
	Adoption                    *RecoveryLEDAdoptionView                `json:"adoption,omitempty"`
	Preservation                *RecoveryLEDPreservationView            `json:"preservation,omitempty"`
	CleanupAttempts             int                                     `json:"cleanup_attempts"`
	CleanupUnconfirmed          int                                     `json:"cleanup_unconfirmed"`
	RecentCleanup               []RecoveryLEDClearResult                `json:"recent_cleanup,omitempty"`
	Resolutions                 map[string]RecoveryLEDBindingResolution `json:"resolutions"`
	Version                     int                                     `json:"version"`
	InventoryID                 string                                  `json:"inventory_id"`
	InventoryHash               string                                  `json:"inventory_hash"`
	Sequence                    int                                     `json:"sequence"`
	Operation                   string                                  `json:"operation"`
	Identity                    protocol.Identity                       `json:"identity"`
	Socket                      string                                  `json:"socket"`
	ObservedAt                  time.Time                               `json:"observed_at"`
	ValidUntil                  time.Time                               `json:"valid_until"`
	ObservationCurrent          bool                                    `json:"observation_current"`
	SourcesMatch                bool                                    `json:"sources_match"`
	SourcesChangedSincePrevious bool                                    `json:"sources_changed_since_previous"`
	MappingsMatch               bool                                    `json:"mappings_match"`
	RuntimeUIDMatches           bool                                    `json:"runtime_uid_matches"`
	SnapshotSourceHash          string                                  `json:"snapshot_source_hash"`
	LatestSourceHash            string                                  `json:"latest_source_hash"`
	DaemonDegraded              bool                                    `json:"daemon_degraded"`
	Targets                     map[string]RecoveryLEDTarget            `json:"targets"`
	Counts                      map[string]int                          `json:"counts"`
	Items                       []RecoveryLEDItem                       `json:"items"`
	Quarantined                 bool                                    `json:"quarantined"`
	ClearAllowed                bool                                    `json:"clear_allowed"`
	ActivationAllowed           bool                                    `json:"activation_allowed"`
	VisualVerified              bool                                    `json:"visual_verified"`
}

type RecoveryLEDImportProposal struct {
	FaultID    string `json:"fault_id"`
	ResultHash string `json:"result_hash"`
	Outcome    string `json:"outcome"`
}

func observeRecoveryLED(ctx context.Context, api LEDRecoveryObserver) (registry []RecoveryLEDRegistryState, targets map[string]RecoveryLEDTarget, degraded bool, err error) {
	status, err := api.Status(ctx)
	if err != nil {
		return
	}
	degraded = status.Degraded
	targets = map[string]RecoveryLEDTarget{}
	for _, name := range []string{"green", "red"} {
		var target RecoveryLEDTarget
		if raw, ok := status.LEDs[name]; ok {
			if e := json.Unmarshal(raw, &target); e != nil {
				return nil, nil, false, fmt.Errorf("invalid LED target observation")
			}
		}
		if target.DefaultPattern != "" && !ledPatternName.MatchString(target.DefaultPattern) {
			return nil, nil, false, fmt.Errorf("invalid LED default pattern")
		}
		targets[name] = target
	}
	entries, err := api.List(ctx)
	if err != nil {
		return
	}
	if len(entries) > 1024 {
		return nil, nil, false, fmt.Errorf("LED registry inventory exceeds budget")
	}
	for _, e := range entries {
		if !utf8.ValidString(e.Message) || utf8.RuneCountInString(e.Message) > 256 {
			return nil, nil, false, fmt.Errorf("invalid LED registry message")
		}
		// Record a digest instead of copying free-form messages from other applications.
		registry = append(registry, RecoveryLEDRegistryState{e.ID, e.OwnerUID, e.LED, e.Pattern, e.Priority, e.Persistent, e.TTL, e.Winner, protocol.Hash(e.State)})
	}
	slices.SortFunc(registry, func(a, b RecoveryLEDRegistryState) int {
		if a.UID < b.UID {
			return -1
		}
		if a.UID > b.UID {
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	err = validateRecoveryLEDRegistry(registry)
	return
}

func validateRecoveryLEDRegistry(entries []RecoveryLEDRegistryState) error {
	if len(entries) > 1024 {
		return fmt.Errorf("LED registry inventory exceeds budget")
	}
	for i, e := range entries {
		if e.UID < 0 || !ledPatternName.MatchString(e.ID) || (e.LED != "green" && e.LED != "red") || !ledPatternName.MatchString(e.Pattern) || e.Priority < 0 || e.Priority > 100 || !safefs.DigestPattern.MatchString(e.StateHash) || (e.TTL != nil && (*e.TTL < 1 || *e.TTL > 2592000)) {
			return fmt.Errorf("invalid LED registry inventory")
		}
		if i > 0 {
			prior := entries[i-1]
			if prior.UID > e.UID || (prior.UID == e.UID && prior.ID >= e.ID) {
				return fmt.Errorf("duplicate or unordered LED registry inventory")
			}
		}
	}
	return nil
}

func (r *recoveryReconciler) validateLEDInventory(v recoveryLEDInventory) error {
	if v.Version != 1 || !safefs.ValidID(v.ID, "n") || v.Sequence < 1 || v.Sequence > maxRecoveryLEDInventories || v.Recovery != r.journal.binding() || v.ConfigHash != protocol.Hash(r.config) || !validLEDSocket(v.Socket) || v.ObservedAt.IsZero() || !v.ValidUntil.Equal(v.ObservedAt.Add(30*time.Second)) || !protocol.ValidToken(v.BootID) || v.Uptime < 0 || math.IsNaN(v.Uptime) || math.IsInf(v.Uptime, 0) {
		return fmt.Errorf("invalid LED recovery inventory identity")
	}
	if (v.Sequence == 1 && v.PreviousHash != "") || (v.Sequence > 1 && !safefs.DigestPattern.MatchString(v.PreviousHash)) {
		return fmt.Errorf("invalid LED inventory predecessor")
	}
	if v.Sources.Snapshot.Schema != r.journal.Snapshot.Schema {
		return fmt.Errorf("LED inventory snapshot schema mismatch")
	}
	if err := r.validateLEDSource(v.Sources.Snapshot, r.journal.After); err != nil {
		return err
	}
	if err := r.validateLEDSource(v.Sources.Latest, r.journal.Before); err != nil {
		return err
	}
	if err := validateRecoveryLEDRegistry(v.Registry); err != nil {
		return err
	}
	if len(v.Targets) != 2 {
		return fmt.Errorf("incomplete LED target inventory")
	}
	for _, name := range []string{"green", "red"} {
		t, ok := v.Targets[name]
		if !ok || (t.DefaultPattern != "" && !ledPatternName.MatchString(t.DefaultPattern)) {
			return fmt.Errorf("invalid LED target inventory")
		}
	}
	return nil
}

func (r *recoveryReconciler) ledInventoryView(v recoveryLEDInventory, sources recoveryLEDSources, previous *recoveryLEDInventory) (out RecoveryLEDReview) {
	out = RecoveryLEDReview{Version: 1, InventoryID: v.ID, InventoryHash: protocol.Hash(v), Sequence: v.Sequence, Operation: v.Recovery.Operation, Identity: v.Recovery.After, Socket: v.Socket, ObservedAt: v.ObservedAt, ValidUntil: v.ValidUntil, SourcesMatch: protocol.Hash(sources) == protocol.Hash(v.Sources), SnapshotSourceHash: protocol.Hash(v.Sources.Snapshot), LatestSourceHash: protocol.Hash(v.Sources.Latest), DaemonDegraded: v.Degraded, Targets: v.Targets, Counts: map[string]int{}, Quarantined: true}
	if previous != nil {
		out.SourcesChangedSincePrevious = protocol.Hash(previous.Sources) != protocol.Hash(v.Sources)
	}
	a, b := v.Sources.Snapshot, v.Sources.Latest
	out.MappingsMatch = a.LayoutPinned && b.LayoutPinned && protocol.Hash(a.Layout) == protocol.Hash(b.Layout)
	out.RuntimeUIDMatches = a.LayoutPinned && b.LayoutPinned && a.Layout.UID == os.Getuid() && b.Layout.UID == os.Getuid()
	boot, uptime, err := r.recoveryClock()
	out.ObservationCurrent = err == nil && boot == v.BootID && uptime >= v.Uptime && uptime-v.Uptime < 30 && !r.now().Before(v.ObservedAt) && r.now().Before(v.ValidUntil) && out.SourcesMatch
	type key struct {
		uid int
		id  string
	}
	items := map[key]*RecoveryLEDItem{}
	// Sources retain their original fault identities. New recovery identity only
	// scopes this inventory; it never replaces an old fault's owner/epoch.
	for index, source := range []recoveryLEDSource{a, b} {
		for _, f := range source.Faults {
			k := key{source.Layout.UID, f.ID}
			item := items[k]
			if item == nil {
				item = &RecoveryLEDItem{ID: f.ID, UID: k.uid, Binding: f.Binding, Classification: "known_fault", OwnershipVerified: source.LayoutPinned && f.Identity != "legacy"}
				items[k] = item
			}
			if item.Binding != f.Binding {
				item.Conflict = "fault_binding_changed"
			}
			if !source.LayoutPinned || f.Identity == "legacy" {
				item.OwnershipVerified = false
				item.Classification = "legacy_fault"
			}
			view := &RecoveryLEDFaultView{f.Condition, f.Obsolete, protocol.Hash(f.Identity)}
			if index == 0 {
				item.Snapshot = view
				item.Provenance = "snapshot"
			} else {
				item.Latest = view
				item.Provenance = "latest"
				if item.Snapshot != nil {
					item.Provenance = "both"
					if item.Snapshot.Condition != view.Condition || item.Snapshot.IdentityHash != view.IdentityHash {
						item.Conflict = "fault_identity_changed"
					}
				}
			}
		}
		for _, binding := range ledLayoutOutputs(source.Layout) {
			for _, kind := range []string{"baseline", "healthy"} {
				id := ledBindingNamespace(source.Layout.Worker, binding.ID) + "." + kind
				k := key{source.Layout.UID, id}
				item := items[k]
				if item == nil {
					item = &RecoveryLEDItem{ID: id, UID: k.uid, Binding: binding.ID, Classification: "reserved_" + kind, Provenance: "snapshot", OwnershipVerified: source.LayoutPinned}
					if index == 1 {
						item.Provenance = "latest"
					}
					items[k] = item
				} else if item.Classification != "reserved_"+kind {
					item.Conflict = "reserved_identity_overlaps_fault"
				} else {
					item.Provenance = "both"
					item.OwnershipVerified = item.OwnershipVerified && source.LayoutPinned
				}
			}
		}
	}
	expectedLED := func(source recoveryLEDSource, binding string) string {
		if binding == workerLEDBinding {
			return "green"
		}
		for _, b := range source.Layout.Bindings {
			if b.ID == binding {
				return b.LED
			}
		}
		return ""
	}
	for _, observed := range v.Registry {
		observed := observed
		k := key{observed.UID, observed.ID}
		item := items[k]
		if item == nil {
			classification := "foreign"
			if observed.UID == a.Layout.UID || observed.UID == b.Layout.UID {
				classification = "unrecognized"
			}
			item = &RecoveryLEDItem{ID: observed.ID, UID: observed.UID, Classification: classification, Provenance: "daemon"}
			items[k] = item
		} else {
			for _, source := range []recoveryLEDSource{a, b} {
				if source.Layout.UID != observed.UID {
					continue
				}
				want := expectedLED(source, item.Binding)
				if want != "" && want != observed.LED {
					item.Conflict = "daemon_target_differs"
				}
			}
		}
		item.Observed = &observed
	}
	for _, item := range items {
		if item.Conflict != "" {
			item.Classification = "conflict"
			item.OwnershipVerified = false
		}
		out.Counts[item.Classification]++
		if item.Classification == "known_fault" || item.Classification == "legacy_fault" {
			if item.Provenance == "latest" {
				out.Counts["post_snapshot_faults"]++
			}
			if item.Provenance == "snapshot" {
				out.Counts["snapshot_only_faults"]++
			}
		}
		if item.Observed == nil {
			out.Counts["not_in_registry"]++
		}
		out.Items = append(out.Items, *item)
	}
	slices.SortFunc(out.Items, func(a, b RecoveryLEDItem) int {
		if a.UID < b.UID {
			return -1
		}
		if a.UID > b.UID {
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	return
}
