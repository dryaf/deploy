// Package protocol defines the local notification API. Producers own domain
// decisions; these types contain no backup, provider credential or LED details.
package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/safefs"
)

const (
	Version     = 1
	MaxRequest  = 128 << 10
	MaxResponse = 256 << 10
	MaxSubjects = 128
	MaxText     = 3500 // Leaves room for a compact event reference on every provider.
)

type Identity struct {
	Worker   string `json:"worker"`
	Recovery string `json:"recovery"`
}

type Producer struct {
	ID    string `json:"id"`
	Epoch string `json:"epoch"`
}

type Incident struct {
	Subject  string `json:"subject"`
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
}

type Message struct {
	Producer       Producer            `json:"producer"`
	Sequence       uint64              `json:"sequence"`
	ID             string              `json:"id"`
	Generation     string              `json:"generation"`
	Subject        string              `json:"subject"`
	Kind           string              `json:"kind"`
	Severity       string              `json:"severity"`
	ObservedAt     time.Time           `json:"observed_at"`
	ExpiresAt      time.Time           `json:"expires_at,omitempty"`
	Text           string              `json:"text"`
	Route          string              `json:"route"`
	ReportKey      string              `json:"report_key,omitempty"`
	ReportType     string              `json:"report_type,omitempty"`
	ReportRevision uint64              `json:"report_revision,omitempty"`
	BasisRevision  uint64              `json:"basis_revision,omitempty"`
	OutageID       string              `json:"outage_id,omitempty"`
	Incidents      []Incident          `json:"incident_refs,omitempty"`
	Context        *InteractionContext `json:"interaction_context,omitempty"`
}

type Subject struct {
	Key           string    `json:"key"`
	Condition     string    `json:"condition"`
	Reason        string    `json:"reason"`
	ValidUntil    time.Time `json:"valid_until"`
	FaultRevision uint64    `json:"fault_revision,omitempty"`
}

type Snapshot struct {
	Producer       Producer            `json:"producer"`
	Sequence       uint64              `json:"sequence"`
	BasisRevision  uint64              `json:"basis_revision"`
	ConfigRevision string              `json:"config_revision"`
	EvaluatedAt    time.Time           `json:"evaluated_at"`
	Subjects       []Subject           `json:"subjects"`
	Support        []AcceptanceSupport `json:"acceptance_support,omitempty"`
}

type Cursor struct {
	Identity Identity `json:"identity"`
	Producer Producer `json:"producer"`
	Position int64    `json:"position"`
}

type Acceptance struct {
	ObservedAt     time.Time `json:"observed_at"`
	Eligible       bool      `json:"eligible"`
	Reason         string    `json:"reason,omitempty"`
	StatusSequence uint64    `json:"status_sequence"`
	BasisRevision  uint64    `json:"basis_revision"`
	ValidUntil     time.Time `json:"valid_until"`
}

type Receipt struct {
	Position   int64       `json:"position"`
	EventID    string      `json:"event_id"`
	Sequence   uint64      `json:"sequence"`
	State      string      `json:"state"`
	AttemptID  string      `json:"attempt_id,omitempty"`
	Provider   string      `json:"provider,omitempty"`
	At         time.Time   `json:"at"`
	Acceptance *Acceptance `json:"acceptance,omitempty"`
}

// OwnershipStatus is a scoped observation, never authority to activate or revoke.
type OwnershipStatus struct {
	Producer   Producer `json:"producer"`
	Generation string   `json:"generation"`
	State      string   `json:"state"`
	Active     bool     `json:"active"`
}

type Request struct {
	Generation  string           `json:"generation,omitempty"`
	Version     int              `json:"v"`
	Operation   string           `json:"op"`
	Expected    Identity         `json:"expected"`
	Producer    Producer         `json:"producer"`
	Message     *Message         `json:"message,omitempty"`
	Snapshot    *Snapshot        `json:"snapshot,omitempty"`
	EventID     string           `json:"event_id,omitempty"`
	Sequence    uint64           `json:"sequence,omitempty"`
	PayloadHash string           `json:"payload_hash,omitempty"`
	Cursor      *Cursor          `json:"cursor,omitempty"`
	Limit       int              `json:"limit,omitempty"`
	After       int64            `json:"after,omitempty"` // Administrative inspection only; never a receipt cursor.
	Transitions *TransitionBatch `json:"transitions,omitempty"`
	Interaction json.RawMessage  `json:"interaction,omitempty"`
}

type Reply struct {
	Version  int             `json:"v"`
	Identity Identity        `json:"identity"`
	OK       bool            `json:"ok"`
	Error    string          `json:"error,omitempty"`
	Result   json.RawMessage `json:"result,omitempty"`
}

var token = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)

func ValidToken(s string) bool { return token.MatchString(s) }
func Sequence(n uint64) bool   { return n > 0 && n <= math.MaxInt64 }
func (p Producer) Valid() bool { return ValidToken(p.ID) && ValidToken(p.Epoch) }
func (i Identity) Valid() bool { return ValidToken(i.Worker) && ValidToken(i.Recovery) }
func Guarded(kind string) bool { return kind == "report" || kind == "catchup" || kind == "recovery" }

func Decode(data []byte, out any, max int) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("invalid UTF-8")
	}
	return safefs.DecodeJSON(data, out, max)
}

func Hash(value any) string {
	b, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (m Message) Validate() error {
	if m.Context != nil {
		if err := m.Context.Validate(); err != nil {
			return err
		}
	}
	if b, err := json.Marshal(m); err != nil || len(b)+1024 > MaxRequest {
		return fmt.Errorf("message exceeds wire bounds")
	}
	if !m.Producer.Valid() || !Sequence(m.Sequence) || !ValidToken(m.ID) || !ValidToken(m.Generation) || !ValidToken(m.Subject) || !ValidToken(m.Route) {
		return fmt.Errorf("invalid message identity")
	}
	switch m.Kind {
	case "failure", "reminder", "recovery", "report", "catchup", "test":
	default:
		return fmt.Errorf("invalid message kind")
	}
	switch m.Severity {
	case "info", "warning", "critical":
	default:
		return fmt.Errorf("invalid severity")
	}
	if !utf8.ValidString(m.Text) || len(m.Text) == 0 || utf8.RuneCountInString(m.Text) > MaxText || m.ObservedAt.IsZero() {
		return fmt.Errorf("invalid message text/time")
	}
	if m.Kind != "failure" && m.Kind != "test" && m.ExpiresAt.IsZero() {
		return fmt.Errorf("expiry required")
	}
	if !m.ExpiresAt.IsZero() && !m.ExpiresAt.After(m.ObservedAt) {
		return fmt.Errorf("invalid expiry")
	}
	if Guarded(m.Kind) && !Sequence(m.BasisRevision) {
		return fmt.Errorf("basis required")
	}
	if m.Kind == "report" || m.Kind == "catchup" {
		if !ValidToken(m.ReportKey) || !ValidToken(m.ReportType) || !Sequence(m.ReportRevision) {
			return fmt.Errorf("report identity required")
		}
	} else if m.ReportKey != "" || m.ReportType != "" || m.ReportRevision != 0 {
		return fmt.Errorf("unexpected report fields")
	}
	if m.Kind == "catchup" && !ValidToken(m.OutageID) {
		return fmt.Errorf("outage required")
	}
	if m.Kind != "catchup" && m.OutageID != "" {
		return fmt.Errorf("unexpected outage")
	}
	if len(m.Incidents) > MaxSubjects {
		return fmt.Errorf("too many incident references")
	}
	seen := map[string]bool{}
	for _, i := range m.Incidents {
		key := i.Subject + "\x00" + i.ID
		if !ValidToken(i.Subject) || !ValidToken(i.ID) || !Sequence(i.Revision) || seen[key] {
			return fmt.Errorf("invalid or duplicate incident reference")
		}
		seen[key] = true
	}
	if (m.Kind == "failure" || m.Kind == "reminder" || m.Kind == "recovery") && (len(m.Incidents) != 1 || m.Incidents[0].Subject != m.Subject) {
		return fmt.Errorf("incident reference required")
	}
	if m.ReportType == "backlog" && (m.Kind != "report" || len(m.Incidents) == 0) {
		return fmt.Errorf("backlog incident references required")
	}
	return nil
}

func (s Snapshot) Validate() error {
	if b, err := json.Marshal(s); err != nil || len(b)+1024 > MaxRequest {
		return fmt.Errorf("snapshot exceeds wire bounds")
	}
	if !s.Producer.Valid() || !Sequence(s.Sequence) || !Sequence(s.BasisRevision) || !ValidToken(s.ConfigRevision) || s.EvaluatedAt.IsZero() || len(s.Subjects) == 0 || len(s.Subjects) > MaxSubjects {
		return fmt.Errorf("invalid snapshot")
	}
	seen := map[string]bool{}
	for _, v := range s.Subjects {
		if !ValidToken(v.Key) || seen[v.Key] || !utf8.ValidString(v.Reason) || utf8.RuneCountInString(v.Reason) > 256 {
			return fmt.Errorf("invalid or duplicate subject")
		}
		seen[v.Key] = true
		switch v.Condition {
		case "ok", "warning", "critical", "unknown", "paused", "retired":
		default:
			return fmt.Errorf("invalid condition")
		}
		if v.ValidUntil.IsZero() || (v.Condition == "ok" && !v.ValidUntil.After(s.EvaluatedAt)) {
			return fmt.Errorf("invalid evidence deadline")
		}
	}
	if len(s.Support) > MaxSubjects {
		return fmt.Errorf("too many support records")
	}
	decisions := map[string]bool{}
	for _, support := range s.Support {
		if err := support.Validate(); err != nil {
			return err
		}
		if !seen[support.Subject] || decisions[support.Decision] {
			return fmt.Errorf("invalid support subject or duplicate decision")
		}
		decisions[support.Decision] = true
	}
	return nil
}
