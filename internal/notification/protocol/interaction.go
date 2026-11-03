package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

type Target struct {
	Subject          string `json:"subject"`
	Kind             string `json:"kind"`
	ID               string `json:"id"`
	Occurrence       string `json:"occurrence"`
	EvidenceRevision uint64 `json:"evidence_revision"`
	DecisionRevision uint64 `json:"decision_revision"`
}

type InteractionContext struct {
	Version  int      `json:"schema_version"`
	ID       string   `json:"context_id"`
	Revision uint64   `json:"context_revision"`
	Kind     string   `json:"kind"`
	Targets  []Target `json:"targets"`
}

// NotificationReference names the frozen event/context, never its rendered prose.
func NotificationReference(m Message) string {
	return "N-" + Hash(struct {
		Producer Producer
		Event    string
		Context  *InteractionContext
	}{m.Producer, m.ID, m.Context})
}

func (c InteractionContext) Validate() error {
	if c.Version != 1 || !ValidToken(c.ID) || !Sequence(c.Revision) || (c.Kind != "single" && c.Kind != "summary") || len(c.Targets) == 0 || len(c.Targets) > 128 || (c.Kind == "single" && len(c.Targets) != 1) {
		return fmt.Errorf("invalid interactive context")
	}
	seen := map[string]bool{}
	for _, t := range c.Targets {
		if !ValidToken(t.Subject) || !ValidToken(t.Kind) || !ValidToken(t.ID) || !ValidToken(t.Occurrence) || !Sequence(t.EvidenceRevision) || !Sequence(t.DecisionRevision) {
			return fmt.Errorf("invalid context target")
		}
		key := Hash(struct{ Subject, Kind, ID, Occurrence string }{t.Subject, t.Kind, t.ID, t.Occurrence})
		if seen[key] {
			return fmt.Errorf("duplicate context target")
		}
		seen[key] = true
	}
	return nil
}

type AcceptanceSupport struct {
	Subject     string    `json:"subject"`
	Decision    string    `json:"decision_id"`
	Revision    uint64    `json:"decision_revision"`
	Occurrence  string    `json:"occurrence"`
	Assumptions uint64    `json:"assumptions_revision"`
	ValidUntil  time.Time `json:"valid_until"`
}

func (s AcceptanceSupport) Validate() error {
	if !ValidToken(s.Subject) || !ValidToken(s.Decision) || !Sequence(s.Revision) || !ValidToken(s.Occurrence) || !Sequence(s.Assumptions) || s.ValidUntil.IsZero() {
		return fmt.Errorf("invalid acceptance support")
	}
	return nil
}

type DecisionRef struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
}
type FaultTransition struct {
	ID             string        `json:"id"`
	Subject        string        `json:"subject"`
	FaultRevision  uint64        `json:"fault_revision"`
	Cause          string        `json:"cause"`
	StatusSequence uint64        `json:"status_sequence"`
	BasisRevision  uint64        `json:"basis_revision"`
	ObservedAt     time.Time     `json:"observed_at"`
	ValidUntil     time.Time     `json:"valid_until"`
	Support        []DecisionRef `json:"support,omitempty"`
}
type TransitionBatch struct {
	Producer Producer          `json:"producer"`
	Records  []FaultTransition `json:"records"`
}
type TransitionRecord struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}
type TransitionReceipt struct {
	Identity Identity           `json:"identity"`
	Producer Producer           `json:"producer"`
	Records  []TransitionRecord `json:"records"`
}

func (b TransitionBatch) Validate() error {
	raw, err := json.Marshal(b)
	if err != nil || len(raw)+1024 > MaxRequest || !b.Producer.Valid() || len(b.Records) == 0 || len(b.Records) > 32 {
		return fmt.Errorf("invalid transition batch")
	}
	seen := map[string]bool{}
	for _, r := range b.Records {
		if !ValidToken(r.ID) || seen[r.ID] || !ValidToken(r.Subject) || !Sequence(r.FaultRevision) || !Sequence(r.StatusSequence) || !Sequence(r.BasisRevision) || r.ObservedAt.IsZero() || !r.ValidUntil.After(r.ObservedAt) || len(r.Support) > 128 {
			return fmt.Errorf("invalid transition")
		}
		seen[r.ID] = true
		if r.Cause != "policy_accepted" && r.Cause != "resolved" && r.Cause != "retired" {
			return fmt.Errorf("invalid transition cause")
		}
		if (r.Cause == "policy_accepted") != (len(r.Support) > 0) {
			return fmt.Errorf("invalid transition support")
		}
		refs := map[string]bool{}
		for _, ref := range r.Support {
			if !ValidToken(ref.ID) || !Sequence(ref.Revision) || refs[ref.ID] {
				return fmt.Errorf("invalid decision reference")
			}
			refs[ref.ID] = true
		}
	}
	return nil
}
