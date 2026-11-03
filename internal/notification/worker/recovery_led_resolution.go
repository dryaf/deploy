package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func (r *recoveryReconciler) validateLEDReport(intent recoveryLEDChallengeRecord, v RecoveryLEDReport, at time.Time) error {
	c, s := intent.Challenge, v.Snapshot
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > 2*protocol.MaxRequest-4096 || len(v.Transitions) > 128 {
		return fmt.Errorf("LED report exceeds budget")
	}
	if err = s.Validate(); err != nil {
		return err
	}
	e, err := r.config.enrollment(c.Producer)
	if err != nil {
		return err
	}
	if s.Producer != c.Producer || s.EvaluatedAt.Before(c.IssuedAt) || s.EvaluatedAt.After(at) || !s.EvaluatedAt.Add(time.Duration(e.MaxSilenceSeconds)*time.Second).After(at) {
		return fmt.Errorf("LED report needs fresh challenged evaluation")
	}
	if !e.PolicyHealth && (len(s.Support) > 0 || len(v.Transitions) > 0) {
		return fmt.Errorf("LED policy evidence not enrolled")
	}
	subjects := map[string]protocol.Subject{}
	for _, sub := range s.Subjects {
		if (sub.FaultRevision != 0 && !protocol.Sequence(sub.FaultRevision)) || (!e.PolicyHealth && sub.FaultRevision != 0) || sub.ValidUntil.After(s.EvaluatedAt.Add(time.Duration(e.MaxEvidenceSeconds)*time.Second)) {
			return fmt.Errorf("invalid LED report evidence lease")
		}
		subjects[sub.Key] = sub
	}
	for _, domain := range ledEvidenceDomains(intent) {
		if !e.PolicyHealth && (len(domain.Faults) > 0 || len(domain.Support) > 0 || len(domain.Dependencies) > 0 || slices.ContainsFunc(domain.Subjects, func(s recoveryLEDSubject) bool { return s.Subject.FaultRevision != 0 })) {
			return fmt.Errorf("policy capability removal requires migration")
		}
		if old := domain.Status; old != nil && (s.Sequence <= old.Sequence || s.BasisRevision < old.BasisRevision || s.ConfigRevision != old.ConfigRevision) {
			return fmt.Errorf("LED report regressed or changed producer configuration")
		}
		for _, old := range domain.Subjects {
			if sub, ok := subjects[old.Subject.Key]; ok && sub.FaultRevision < old.Subject.FaultRevision {
				return fmt.Errorf("LED fault revision regressed")
			}
		}
		for _, old := range domain.Faults {
			sub, ok := subjects[old.Subject]
			if !ok {
				continue
			}
			if sub.FaultRevision < old.Revision || (sub.FaultRevision == old.Revision && (sub.Condition == "critical" || sub.Condition == "warning") && (!old.Active || sub.Condition != old.Condition)) {
				return fmt.Errorf("LED fault incarnation regressed or reused")
			}
		}
		for _, old := range domain.Support {
			for _, support := range s.Support {
				if support.Decision == old.Decision && (support.Revision < old.Revision || (support.Revision == old.Revision && protocol.Hash(support) != protocol.Hash(old))) {
					return fmt.Errorf("LED acceptance revision conflict")
				}
			}
		}
	}
	seen := map[string]bool{}
	for start := 0; start < len(v.Transitions); start += 32 {
		end := min(start+32, len(v.Transitions))
		if err = (protocol.TransitionBatch{Producer: c.Producer, Records: v.Transitions[start:end]}).Validate(); err != nil {
			return err
		}
	}
	for _, tr := range v.Transitions {
		sub, ok := subjects[tr.Subject]
		if seen[tr.ID] || !ok || tr.StatusSequence != s.Sequence || tr.BasisRevision != s.BasisRevision || tr.ObservedAt.Before(c.IssuedAt) || tr.ObservedAt.After(s.EvaluatedAt) || !tr.ValidUntil.After(at) || tr.ValidUntil.After(tr.ObservedAt.Add(time.Duration(e.MaxEvidenceSeconds)*time.Second)) || tr.FaultRevision > sub.FaultRevision {
			return fmt.Errorf("LED transition does not match challenged report")
		}
		seen[tr.ID] = true
		if (tr.Cause == "resolved" && sub.Condition != "ok") || (tr.Cause == "retired" && sub.Condition != "retired") || (tr.Cause == "policy_accepted" && sub.Condition != "ok" && sub.Condition != "critical" && sub.Condition != "warning") || ((sub.Condition == "critical" || sub.Condition == "warning") && sub.FaultRevision <= tr.FaultRevision) {
			return fmt.Errorf("LED transition does not discharge its fault")
		}
		for _, ref := range tr.Support {
			if !slices.ContainsFunc(s.Support, func(a protocol.AcceptanceSupport) bool {
				return a.Subject == tr.Subject && a.Decision == ref.ID && a.Revision == ref.Revision && a.ValidUntil.After(at)
			}) {
				return fmt.Errorf("LED transition lacks current acceptance support")
			}
		}
	}
	return nil
}

// DomainResolved and ProposedFaultIDs describe a bounded proposal, not a clear
// grant. Execution must recheck registry ownership and all source evidence.
type RecoveryLEDBindingResolution struct {
	DomainResolved   bool      `json:"domain_resolved"`
	Reason           string    `json:"reason"`
	EvidenceHash     string    `json:"evidence_hash,omitempty"`
	ValidUntil       time.Time `json:"valid_until,omitempty"`
	ProposedFaultIDs []string  `json:"proposed_fault_ids,omitempty"`
}

func (r *recoveryReconciler) appendLEDResolutionReview(ctx context.Context, tx *sql.Tx, v recoveryLEDInventory, out *RecoveryLEDReview) error {
	exists, err := ledEvidenceSchema(ctx, tx)
	if err != nil {
		return err
	}
	out.Resolutions = map[string]RecoveryLEDBindingResolution{workerLEDBinding: {Reason: "runtime_reconciliation_required"}}
	for _, binding := range r.config.LEDBindings {
		result := RecoveryLEDBindingResolution{Reason: "producer_evidence_missing"}
		slot, err := r.readLEDEvidenceSlot(ctx, tx, v, binding.Producer, exists)
		if err != nil {
			return err
		}
		if slot.ReplyCommitted {
			result, err = r.evaluateLEDResolution(ctx, tx, binding, slot)
			if err != nil {
				return err
			}
		}
		if result.DomainResolved {
			if !out.SourcesMatch || !out.MappingsMatch || !out.RuntimeUIDMatches || !out.ObservationCurrent {
				result.DomainResolved = false
				result.Reason = "inventory_not_current_or_mapping_changed"
			}
			for _, item := range out.Items {
				if item.Binding != binding.ID || (item.Snapshot == nil && item.Latest == nil) {
					continue
				}
				if !item.OwnershipVerified || item.Classification != "known_fault" {
					result.DomainResolved = false
					result.Reason = "fault_ownership_unresolved"
					break
				}
				adverse := func(f *RecoveryLEDFaultView) bool {
					return f != nil && (f.Condition == "critical" || f.Condition == "warning")
				}
				if (adverse(item.Snapshot) || adverse(item.Latest)) && !ledAdverseProvenance(binding, slot.Intent.Domains) {
					result.DomainResolved = false
					result.Reason = "fault_provenance_incomplete"
					break
				}
				result.ProposedFaultIDs = append(result.ProposedFaultIDs, item.ID)
			}
			if !result.DomainResolved {
				result.ProposedFaultIDs = nil
			} else {
				result.ValidUntil = minLEDDeadline(result.ValidUntil, v.ValidUntil)
				result.EvidenceHash = protocol.Hash(struct{ Inventory, Report string }{protocol.Hash(v), protocol.Hash(slot.Reply)})
			}
		}
		out.Resolutions[binding.ID] = result
	}
	return nil
}

func ledAdverseProvenance(b LEDBinding, domains recoveryLEDDomains) bool {
	selected := func(key string) bool { return b.Subjects[0] == "*" || slices.Contains(b.Subjects, key) }
	for _, d := range []recoveryLEDDomain{domains.Snapshot, domains.Latest} {
		for _, s := range d.Subjects {
			if selected(s.Subject.Key) && (s.Latch != "" || s.Subject.Condition == "critical" || s.Subject.Condition == "warning") {
				return true
			}
		}
		for _, f := range d.Faults {
			if selected(f.Subject) {
				return true
			}
		}
	}
	return false
}
func minLEDDeadline(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

func (r *recoveryReconciler) evaluateLEDResolution(ctx context.Context, tx *sql.Tx, b LEDBinding, slot recoveryLEDEvidenceSlot) (out RecoveryLEDBindingResolution, err error) {
	out.Reason = "producer_evidence_expired"
	proof := slot.Reply
	c := proof.Challenge
	report := proof.Report
	s := report.Snapshot
	now := r.now()
	if r.currentLEDChallenge(c) != nil || now.Before(proof.ReceivedAt) {
		return out, nil
	}
	now, err = r.ledRecoveryTime(ctx, tx)
	if err != nil {
		return out, err
	}
	e, _ := r.config.enrollment(c.Producer)
	deadline := minLEDDeadline(c.ExpiresAt, s.EvaluatedAt.Add(time.Duration(e.MaxSilenceSeconds)*time.Second))
	if !deadline.After(now) {
		return out, nil
	}
	domains, err := r.collectLEDDomains(ctx, tx, c.Producer)
	if err != nil {
		return out, err
	}
	if protocol.Hash(domains) != c.DomainHash {
		out.Reason = "producer_domain_changed"
		return out, nil
	}
	keys := map[string]bool{}
	if b.Subjects[0] != "*" {
		for _, key := range b.Subjects {
			keys[key] = true
		}
	} else {
		for _, sub := range s.Subjects {
			keys[sub.Key] = true
		}
		for _, d := range ledEvidenceDomains(*slot.Intent) {
			for _, old := range d.Subjects {
				keys[old.Subject.Key] = true
			}
			for _, old := range d.Faults {
				keys[old.Subject] = true
			}
			for _, old := range d.Dependencies {
				keys[old.Subject] = true
			}
		}
	}
	if len(keys) == 0 || len(keys) > protocol.MaxSubjects {
		out.Reason = "subject_coverage_incomplete"
		return out, nil
	}
	// Sort so a blocked proposal reports a deterministic reason across restarts.
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	slices.Sort(ordered)
	for _, key := range ordered {
		var sub protocol.Subject
		found := false
		for _, candidate := range s.Subjects {
			if candidate.Key == key {
				sub = candidate
				found = true
				break
			}
		}
		if !found {
			out.Reason = "subject_coverage_incomplete"
			return out, nil
		}
		if sub.Condition != "ok" && sub.Condition != "retired" {
			out.Reason = "subject_not_resolved"
			return out, nil
		}
		if !sub.ValidUntil.After(now) {
			out.Reason = "subject_evidence_expired"
			return out, nil
		}
		deadline = minLEDDeadline(deadline, sub.ValidUntil)
		for _, a := range s.Support {
			if a.Subject == key {
				if !a.ValidUntil.After(now) {
					out.Reason = "acceptance_support_expired"
					return out, nil
				}
				deadline = minLEDDeadline(deadline, a.ValidUntil)
			}
		}
		if !e.PolicyHealth {
			continue
		}
		revisions := map[uint64]bool{}
		deps := []recoveryLEDDependency{}
		for _, d := range ledEvidenceDomains(*slot.Intent) {
			for _, old := range d.Subjects {
				if old.Subject.Key == key {
					if old.Latch != "" && old.Subject.FaultRevision == 0 {
						out.Reason = "legacy_policy_fault"
						return out, nil
					}
					if old.Subject.FaultRevision > 0 {
						revisions[old.Subject.FaultRevision] = true
					}
				}
			}
			for _, old := range d.Faults {
				if old.Subject == key && old.Active {
					revisions[old.Revision] = true
				}
			}
			for _, dep := range d.Dependencies {
				if dep.Subject == key {
					deps = append(deps, dep)
				}
			}
		}
		for revision := range revisions {
			matched := false
			for _, tr := range report.Transitions {
				if tr.Subject == key && tr.FaultRevision == revision && tr.ValidUntil.After(now) {
					matched = true
					deadline = minLEDDeadline(deadline, tr.ValidUntil)
					break
				}
			}
			if !matched {
				out.Reason = "exact_fault_transition_missing"
				return out, nil
			}
		}
		for _, dep := range deps {
			supported := slices.ContainsFunc(s.Support, func(a protocol.AcceptanceSupport) bool {
				return a.Subject == key && a.Decision == dep.Decision && a.Revision >= dep.Revision && a.ValidUntil.After(now)
			})
			discharged := slices.ContainsFunc(report.Transitions, func(tr protocol.FaultTransition) bool {
				return tr.Subject == key && tr.FaultRevision == sub.FaultRevision && (tr.Cause == "resolved" || tr.Cause == "retired") && tr.ValidUntil.After(now)
			})
			if !supported && !discharged {
				out.Reason = "acceptance_dependency_unresolved"
				return out, nil
			}
		}
	}
	out.DomainResolved = true
	out.Reason = "fresh_domain_resolution"
	out.ValidUntil = deadline
	return out, nil
}

// ledRecoveryTime never extends an evidence lease after wall-clock rollback.
func (r *recoveryReconciler) ledRecoveryTime(ctx context.Context, q ledJournalReader) (time.Time, error) {
	now := r.now()
	// Wall time can move backwards without crossing the challenge's issue
	// time. Retained same-boot observations supply a monotonic lower bound,
	// including across explicit inventory refreshes.
	boot, uptime, clockErr := r.recoveryClock()
	if clockErr != nil {
		return time.Time{}, fmt.Errorf("LED recovery monotonic clock unavailable")
	}
	set, err := r.readLEDInventorySet(ctx, q)
	if err != nil {
		return time.Time{}, err
	}
	for _, observation := range set.Records {
		if observation.BootID == boot {
			elapsed := uptime - observation.Uptime
			if elapsed < 0 || elapsed >= float64((1<<63-1)/int64(time.Second)) {
				return time.Time{}, fmt.Errorf("LED recovery monotonic clock unavailable")
			}
			floor := observation.ObservedAt.Add(time.Duration(elapsed * float64(time.Second)))
			if floor.After(now) {
				now = floor
			}
		}
	}
	return now, nil
}
