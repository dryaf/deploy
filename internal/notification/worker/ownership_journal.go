package worker

import (
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// History lives outside SQLite: restoring an old queue must not erase evidence
// that a generation once authorized attempts. A baseline adopted from an older
// installation is explicitly incomplete evidence, never proof of no delivery.
type ownershipTransition struct {
	Schema     int                 `json:"schema"`
	Identity   protocol.Identity   `json:"identity"`
	Producer   protocol.Producer   `json:"producer"`
	Revision   uint64              `json:"revision"`
	Previous   string              `json:"previous"`
	BeforeHash string              `json:"before_hash"`
	Operation  string              `json:"operation"`
	Generation string              `json:"generation,omitempty"`
	Baseline   *ownership          `json:"baseline,omitempty"`
	Recovery   *ownershipRecovery  `json:"recovery,omitempty"`
	Retirement *ProducerRetirement `json:"retirement,omitempty"`
}
type ownershipRecovery struct {
	Binding     recoveryBinding
	SummaryHash string
}

// Publishing this intent authorizes only its exact transition. On restart, under
// worker.lock and the producer lock, recovery completes a lost acknowledgement.
// Keep the latest intent after completion to detect a rolled-back owner file.
type ownershipIntent struct {
	Before     ownership           `json:"before"`
	Transition ownershipTransition `json:"transition"`
}

func ownershipName(p protocol.Producer) string { return "owner-" + safefs.LockName(p.ID) }
func ownershipHistoryPath(p protocol.Producer, revision uint64) string {
	return fmt.Sprintf("ownership/%s/%020d.json", safefs.LockName(p.ID), revision)
}

func emptyOwnership(id protocol.Identity, p protocol.Producer) ownership {
	return ownership{Identity: id, Producer: p, Generations: map[string]string{}}
}

func validOwnership(o ownership) bool {
	if !o.Identity.Valid() || !o.Producer.Valid() || o.Generations == nil ||
		(o.Revision == 0 && o.History != "") || (o.Revision > 0 && !safefs.DigestPattern.MatchString(o.History)) {
		return false
	}
	if o.Retirement != nil {
		if o.Retirement.validate() != nil || o.Retirement.Identity.Worker != o.Identity.Worker || o.Retirement.Producer != o.Producer || o.Active != "" || o.Revision == 0 {
			return false
		}
		for _, state := range o.Generations {
			if state != "revoked" {
				return false
			}
		}
	}
	if o.RecoveryPending != nil {
		if !validOwnershipRecoveryBinding(*o.RecoveryPending) || o.RecoveryPending.After != o.Identity || o.Active != "" {
			return false
		}
		for _, state := range o.Generations {
			if state != "revoked" {
				return false
			}
		}
	}
	for generation, state := range o.Generations {
		if !protocol.ValidToken(generation) {
			return false
		}
		switch state {
		case "active":
			if o.Active != generation {
				return false
			}
		case "staged", "revoked":
			if o.Active == generation {
				return false
			}
		case "draining":
		default:
			return false
		}
	}
	return o.Active == "" || o.Generations[o.Active] == "active" || o.Generations[o.Active] == "draining"
}

func sameOwnership(a, b ownership) bool {
	return a.Identity == b.Identity && a.Producer == b.Producer && a.Active == b.Active &&
		a.Revision == b.Revision && a.History == b.History && maps.Equal(a.Generations, b.Generations) &&
		((a.Retirement == nil && b.Retirement == nil) || (a.Retirement != nil && b.Retirement != nil && *a.Retirement == *b.Retirement)) &&
		((a.RecoveryPending == nil && b.RecoveryPending == nil) ||
			(a.RecoveryPending != nil && b.RecoveryPending != nil && *a.RecoveryPending == *b.RecoveryPending))
}

func validOwnershipRecoveryBinding(b recoveryBinding) bool {
	return safefs.ValidID(b.Operation, "n") && safefs.ValidID(b.Snapshot, "n") && b.Before.Valid() && b.After.Valid() &&
		b.Before.Worker == b.After.Worker && b.Before.Recovery != b.After.Recovery
}

func applyOwnershipTransition(before ownership, event ownershipTransition) (ownership, error) {
	if !validOwnership(before) || (event.Schema < 1 || event.Schema > 4) || event.Identity != before.Identity || event.Producer != before.Producer ||
		before.Revision == math.MaxUint64 || event.Revision != before.Revision+1 || event.Previous != before.History || event.BeforeHash != protocol.Hash(before) {
		return ownership{}, fmt.Errorf("invalid ownership transition identity or history")
	}
	recovery := event.Operation == "recover" || event.Operation == "recovery-fence"
	retire := event.Operation == "retire"
	if (event.Schema == 2 || event.Schema == 3) != recovery || (event.Recovery != nil) != recovery ||
		(event.Schema == 4) != retire || (event.Retirement != nil) != retire ||
		(before.Retirement != nil && !recovery) ||
		(event.Operation == "recovery-fence" && event.Schema != 3) ||
		(before.RecoveryPending != nil && (!recovery || event.Schema != 3)) ||
		(event.Operation == "recover" && (event.Schema == 3) != (before.RecoveryPending != nil)) {
		return ownership{}, fmt.Errorf("invalid ownership recovery schema")
	}
	after := before
	after.Generations = maps.Clone(before.Generations)
	g := event.Generation
	if recovery {
		b := event.Recovery.Binding
		if before.Revision == 0 || g != "" || event.Baseline != nil ||
			!validOwnershipRecoveryBinding(b) ||
			((before.RecoveryPending == nil || event.Operation == "recovery-fence") && b.Before != before.Identity) ||
			(before.RecoveryPending != nil && event.Operation == "recover" && *before.RecoveryPending != b) ||
			(event.Operation == "recover" && !safefs.DigestPattern.MatchString(event.Recovery.SummaryHash)) ||
			(event.Operation == "recovery-fence" && event.Recovery.SummaryHash != "") {
			return ownership{}, fmt.Errorf("invalid ownership recovery binding")
		}
	} else if retire {
		if before.Revision == 0 || before.Active != "" || g != "" || event.Baseline != nil || event.Retirement.validate() != nil || event.Retirement.Identity != before.Identity || event.Retirement.Producer != before.Producer {
			return ownership{}, fmt.Errorf("invalid producer retirement")
		}
		for _, state := range before.Generations {
			if state != "revoked" {
				return ownership{}, fmt.Errorf("producer generation not revoked")
			}
		}
	} else if event.Operation != "initialize" && event.Operation != "adopt" {
		if before.Revision == 0 || !protocol.ValidToken(g) || event.Baseline != nil {
			return ownership{}, fmt.Errorf("invalid ownership transition generation")
		}
	} else if g != "" || before.Revision != 0 {
		return ownership{}, fmt.Errorf("invalid ownership baseline")
	}
	switch event.Operation {
	case "retire":
		v := *event.Retirement
		after.Retirement = &v
	case "recover", "recovery-fence":
		after.Identity = event.Recovery.Binding.After
		after.Active = ""
		after.RecoveryPending = nil
		if event.Operation == "recovery-fence" {
			binding := event.Recovery.Binding
			after.RecoveryPending = &binding
		}
		for generation := range after.Generations {
			after.Generations[generation] = "revoked"
		}
	case "initialize":
		if len(before.Generations) != 0 || before.Active != "" || event.Baseline != nil {
			return ownership{}, fmt.Errorf("nonempty initial ownership")
		}
	case "adopt":
		if event.Baseline == nil || !sameOwnership(before, *event.Baseline) {
			return ownership{}, fmt.Errorf("ownership baseline changed")
		}
	case "stage":
		if before.Generations[g] != "" {
			return ownership{}, fmt.Errorf("generation already used")
		}
		after.Generations[g] = "staged"
	case "activate":
		if before.Active != "" || before.Generations[g] != "staged" {
			return ownership{}, fmt.Errorf("generation cannot activate")
		}
		after.Active, after.Generations[g] = g, "active"
	case "drain":
		if before.Generations[g] != "active" && before.Generations[g] != "staged" {
			return ownership{}, fmt.Errorf("generation cannot drain")
		}
		after.Generations[g] = "draining"
	case "revoke":
		if before.Generations[g] != "draining" {
			return ownership{}, fmt.Errorf("generation is not draining")
		}
		after.Generations[g] = "revoked"
		if after.Active == g {
			after.Active = ""
		}
	default:
		return ownership{}, fmt.Errorf("unknown ownership transition")
	}
	after.Revision, after.History = event.Revision, protocol.Hash(event)
	return after, nil
}

func (s *Store) ownershipIntent(p protocol.Producer) (ownershipIntent, ownership, error) {
	var intent ownershipIntent
	if err := s.dir.ReadJSON(ownershipName(p)+".journal.json", &intent); err != nil {
		return intent, ownership{}, err
	}
	if intent.Before.Producer != p {
		return intent, ownership{}, fmt.Errorf("ownership journal identity mismatch")
	}
	after, err := applyOwnershipTransition(intent.Before, intent.Transition)
	if err == nil && after.Identity != s.identity {
		err = fmt.Errorf("ownership journal epoch mismatch")
	}
	return intent, after, err
}

// Producer observations and grants are read-only here. An incomplete lifecycle
// intent fences them until the exclusive startup recovery finishes it.
func (s *Store) readOwnership(p protocol.Producer) (ownership, error) {
	var current ownership
	if err := s.dir.ReadJSON(ownershipName(p)+".json", &current); err != nil {
		return current, err
	}
	intent, after, err := s.ownershipIntent(p)
	if err != nil {
		return current, err
	}
	if !validOwnership(current) || !sameOwnership(current, after) {
		return current, fmt.Errorf("ownership transition incomplete or inconsistent; restart recovery required")
	}
	var recorded ownershipTransition
	if err = s.dir.ReadJSON(ownershipHistoryPath(p, current.Revision), &recorded); err != nil {
		return current, err
	}
	if protocol.Hash(recorded) != protocol.Hash(intent.Transition) {
		return current, fmt.Errorf("ownership history changed")
	}
	if err = s.noLaterOwnershipTransition(p, current.Revision); err != nil {
		return current, err
	}
	return current, nil
}

func (s *Store) noLaterOwnershipTransition(p protocol.Producer, revision uint64) error {
	if revision == math.MaxUint64 {
		return nil
	}
	var event ownershipTransition
	err := s.dir.ReadJSON(ownershipHistoryPath(p, revision+1), &event)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("ownership head is older than durable history")
}

// Startup checks the entire directory, including tails beyond an old head.
// Checking only the linked prefix would permit a rolled-back owner/head pair to
// forget a later revocation. Memory stays bounded even for long-lived producers.
func (s *Store) checkOwnershipHistoryTail(p protocol.Producer, maximum uint64) error {
	dir, err := s.dir.Sub("ownership/" + safefs.LockName(p.ID))
	if os.IsNotExist(err) && maximum == 1 {
		return nil
	} // Pending initial/adopt intent.
	if err != nil {
		return err
	}
	defer dir.Close()
	for {
		entries, err := dir.File().ReadDir(128)
		for _, entry := range entries {
			if safefs.IsJSONTemporary(entry.Name()) {
				continue
			}
			n, parseErr := strconv.ParseUint(strings.TrimSuffix(entry.Name(), ".json"), 10, 64)
			if parseErr != nil || n == 0 || entry.Name() != fmt.Sprintf("%020d.json", n) || entry.IsDir() || n > maximum {
				return fmt.Errorf("unexpected or newer ownership history")
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Store) commitOwnership(current *ownership, operation, generation string) error {
	event := ownershipTransition{Schema: 1, Identity: current.Identity, Producer: current.Producer,
		Revision: current.Revision + 1, Previous: current.History, BeforeHash: protocol.Hash(current), Operation: operation, Generation: generation}
	if operation == "adopt" {
		baseline := *current
		baseline.Generations = maps.Clone(current.Generations)
		event.Baseline = &baseline
	}
	return s.commitOwnershipEvent(current, event)
}
func (s *Store) commitOwnershipEvent(current *ownership, event ownershipTransition) error {
	if err := s.dir.Capacity(s.config.MinFreeBytes + 3*safefs.MaxRecordBytes); err != nil {
		return err
	}
	intent := ownershipIntent{Before: *current, Transition: event}
	after, err := applyOwnershipTransition(*current, event)
	if err != nil {
		return err
	}
	// Preflight immutable file conflicts before publishing a new authorization.
	if err = s.checkOwnershipHistoryTail(current.Producer, event.Revision); err != nil {
		return err
	}
	if err = s.checkOwnershipHistory(event, false); err != nil {
		return err
	}
	if err = s.dir.WriteJSON(ownershipName(current.Producer)+".journal.json", intent, current.Revision != 0); err != nil {
		return err
	}
	if err = s.ownershipBoundary("intent"); err != nil {
		return err
	}
	if err = s.checkOwnershipHistory(event, true); err != nil {
		return err
	}
	if err = s.ownershipBoundary("history"); err != nil {
		return err
	}
	if err = s.dir.WriteJSON(ownershipName(current.Producer)+".json", after, event.Operation != "initialize"); err != nil {
		return err
	}
	*current = after
	return s.ownershipBoundary("owner")
}

func (s *Store) checkOwnershipHistory(event ownershipTransition, create bool) error {
	path := ownershipHistoryPath(event.Producer, event.Revision)
	var recorded ownershipTransition
	err := s.dir.ReadJSON(path, &recorded)
	if err == nil {
		if protocol.Hash(recorded) != protocol.Hash(event) {
			return fmt.Errorf("immutable ownership history conflict")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if !create {
		return nil
	}
	if err = s.dir.Mkdir("ownership/" + safefs.LockName(event.Producer.ID)); err != nil {
		return err
	}
	return s.dir.WriteJSON(path, event, false)
}

// Open holds the exclusive worker lease. Keep the producer lock inode stable:
// neither an ordinary reopen nor a failed read may recreate missing ownership.
func (s *Store) prepareOwnership(create, legacy bool) error {
	for _, enrollment := range s.config.Producers {
		p := enrollment.Producer
		lock, err := s.dir.Lock(ownershipName(p)+".lock", true, create)
		if err != nil {
			return err
		}
		err = s.prepareProducerOwnership(p, create, legacy)
		lock.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) prepareProducerOwnership(p protocol.Producer, create, legacy bool) error {
	if create {
		current := emptyOwnership(s.identity, p)
		return s.commitOwnership(&current, "initialize", "")
	}
	var current ownership
	if err := s.dir.ReadJSON(ownershipName(p)+".json", &current); err != nil {
		return err
	}
	if !validOwnership(current) || current.Producer != p {
		return fmt.Errorf("invalid canonical ownership")
	}
	intent, after, err := s.ownershipIntent(p)
	if os.IsNotExist(err) && legacy && current.Revision == 0 {
		if current.Identity != s.identity {
			return fmt.Errorf("legacy ownership epoch mismatch")
		}
		return s.commitOwnership(&current, "adopt", "")
	}
	if err != nil {
		return err
	}
	if err = s.checkOwnershipHistoryTail(p, intent.Transition.Revision); err != nil {
		return err
	}
	if !sameOwnership(current, intent.Before) && !sameOwnership(current, after) {
		return fmt.Errorf("ownership differs from durable lifecycle intent")
	}
	// Verify the entire prefix before completing an intent. Missing history is
	// uncertainty, not permission to construct a replacement audit trail.
	if err = s.verifyOwnershipPrefix(intent.Before); err != nil {
		return err
	}
	if err = s.checkOwnershipHistory(intent.Transition, true); err != nil {
		return err
	}
	if err = s.ownershipBoundary("history"); err != nil {
		return err
	}
	if !sameOwnership(current, after) {
		if err = s.dir.WriteJSON(ownershipName(p)+".json", after, true); err != nil {
			return err
		}
	}
	return s.ownershipBoundary("owner")
}

func (s *Store) verifyOwnershipPrefix(want ownership) error {
	if want.Revision == 0 {
		return nil // An explicitly recorded legacy baseline.
	}
	var first ownershipTransition
	if err := s.dir.ReadJSON(ownershipHistoryPath(want.Producer, 1), &first); err != nil {
		return err
	}
	if first.Identity.Worker != want.Identity.Worker {
		return fmt.Errorf("ownership origin changed worker")
	}
	current := emptyOwnership(first.Identity, want.Producer)
	for revision := uint64(1); revision <= want.Revision; revision++ {
		var event ownershipTransition
		if err := s.dir.ReadJSON(ownershipHistoryPath(want.Producer, revision), &event); err != nil {
			return err
		}
		if revision == 1 && event.Operation == "adopt" && event.Baseline != nil {
			current = *event.Baseline
		}
		var err error
		current, err = applyOwnershipTransition(current, event)
		if err != nil {
			return err
		}
	}
	if !sameOwnership(current, want) {
		return fmt.Errorf("ownership history does not match canonical state")
	}
	return nil
}

// A private fault hook exercises durable boundaries in subprocess tests. Normal
// production stores leave it nil; there is no environment or CLI bypass.
func (s *Store) ownershipBoundary(stage string) error {
	if s.ownershipFault != nil {
		return s.ownershipFault(stage)
	}
	return nil
}
