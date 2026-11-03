package interaction

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type SignalRecoveryChallenge struct {
	Operation           string
	Gateway             protocol.Identity
	Relay               SpoolCursor // Position is always zero; this is an identity, not an ACK.
	Nonce               string
	IssuedAt, ExpiresAt time.Time
	BootID              string
	Uptime              float64
}
type SignalRecoveryManifest struct {
	Relay                         SpoolCursor
	Highest, Acknowledged, Listed uint64
	Digest                        string
	Restore                       SignalRelayRestore
	// Absent for legacy contiguous exports. Recovery pages use retained-event
	// ordinals; each event still carries its original stream position.
	History          []SignalRange `json:",omitempty"`
	BaselineHash     string        `json:",omitempty"`
	BaselinePosition uint64        `json:",omitempty"`
	RetainedEvents   uint64        `json:",omitempty"`
}

// SignalRelayRestore binds old-epoch evidence to a quarantined relay candidate.
// It grants no authority to use the new identity for intake or acknowledgements.
type SignalRelayRestore struct {
	Operation, Snapshot, SnapshotHash string
	After                             protocol.Identity
}
type SignalRecoveryPage struct {
	Events []SpoolEvent
	Next   uint64 // Retained-event ordinal, never an operational relay cursor.
	Done   bool
}
type SignalRecoverySummary struct {
	Challenge                             SignalRecoveryChallenge
	Manifest                              SignalRecoveryManifest
	Mapped, Unmapped, Rejected            uint64
	IntakeEnabled                         bool
	HistoryComplete                       bool
	GatewayHighest                        uint64
	MissingRelayFrom, MissingRelayThrough uint64
}
type SignalRecoverySource interface {
	Cursor() SpoolCursor
	Capture(context.Context, SignalRecoveryChallenge) (SignalRecoveryManifest, error)
	Page(context.Context, SignalRecoveryChallenge, uint64) (SignalRecoveryPage, error)
	RecordSummary(context.Context, SignalRecoverySummary) error
	Completed(context.Context, SignalRecoveryChallenge) (bool, error)
	Close() error
}
type SignalRecoverySession interface {
	SignalChallenge(context.Context, SpoolCursor) (SignalRecoveryChallenge, error)
	BeginSignal(context.Context, SignalRecoveryChallenge, SignalRecoveryManifest) error
	ImportSignal(context.Context, SignalRecoveryChallenge, uint64, []SpoolEvent) error
	FinishSignal(context.Context, SignalRecoveryChallenge) (SignalRecoverySummary, error)
	SignalSummary(context.Context, string) (SignalRecoverySummary, error)
	Close() error
}

func (c SignalRecoveryChallenge) Validate(now time.Time, boot string, up float64) error {
	if !safefs.ValidID(c.Operation, "n") || !safefs.ValidID(c.Nonce, "n") || !c.Gateway.Valid() ||
		!c.Relay.Identity.Valid() || !protocol.ValidToken(c.Relay.Account) || c.Relay.Position != 0 {
		return fmt.Errorf("invalid Signal recovery identity")
	}
	if c.IssuedAt.IsZero() || !c.ExpiresAt.After(c.IssuedAt) || c.ExpiresAt.Sub(c.IssuedAt) > 10*time.Minute ||
		now.Before(c.IssuedAt) || !now.Before(c.ExpiresAt) || !protocol.ValidToken(boot) || boot != c.BootID ||
		math.IsNaN(up) || math.IsInf(up, 0) || math.IsNaN(c.Uptime) || math.IsInf(c.Uptime, 0) ||
		c.Uptime < 0 || up < c.Uptime || up-c.Uptime >= 600 {
		return fmt.Errorf("Signal recovery challenge expired or clock invalid")
	}
	return nil
}
func (m SignalRecoveryManifest) Validate() error {
	if !m.Relay.Identity.Valid() || !protocol.ValidToken(m.Relay.Account) || m.Relay.Position != 0 ||
		m.Highest > math.MaxInt64 || m.Acknowledged > m.Listed || m.Listed > m.Highest || !safefs.DigestPattern.MatchString(m.Digest) {
		return fmt.Errorf("invalid Signal recovery manifest")
	}
	if err := m.validateHistory(); err != nil {
		return err
	}
	if b := m.Restore; b != (SignalRelayRestore{}) {
		if !safefs.ValidID(b.Operation, "n") || !safefs.ValidID(b.Snapshot, "n") ||
			!safefs.DigestPattern.MatchString(b.SnapshotHash) || !b.After.Valid() ||
			b.After.Worker != m.Relay.Identity.Worker || b.After.Recovery == m.Relay.Identity.Recovery {
			return fmt.Errorf("invalid restored Signal relay binding")
		}
	}
	return nil
}
func (s SignalRecoverySummary) Validate() error {
	if err := s.Challenge.Validate(s.Challenge.IssuedAt, s.Challenge.BootID, s.Challenge.Uptime); err != nil {
		return err
	}
	if err := s.Manifest.Validate(); err != nil {
		return err
	}
	if s.Challenge.Relay != s.Manifest.Relay || s.IntakeEnabled || s.HistoryComplete ||
		s.Mapped > s.Manifest.EventCount() || s.Unmapped > s.Manifest.EventCount()-s.Mapped ||
		s.Rejected != s.Manifest.EventCount()-s.Mapped-s.Unmapped || s.GatewayHighest > math.MaxInt64 {
		return fmt.Errorf("invalid Signal recovery summary")
	}
	from, through := uint64(0), uint64(0)
	if s.Manifest.Restore.Operation == "" {
		if s.GatewayHighest != 0 {
			return fmt.Errorf("ordinary relay export cannot declare rollback")
		}
	} else if s.GatewayHighest > s.Manifest.Highest {
		from, through = s.Manifest.Highest+1, s.GatewayHighest
	}
	if s.MissingRelayFrom != from || s.MissingRelayThrough != through {
		return fmt.Errorf("Signal missing-history range changed")
	}
	return nil
}
func ValidateSignalRecoveryEvent(e SpoolEvent, c SpoolCursor) error {
	raw, err := json.Marshal(e)
	if err != nil || len(raw) > 16384 || !protocol.Sequence(e.Position) || !protocol.ValidToken(e.Key) || !safefs.DigestPattern.MatchString(e.Hash) {
		return fmt.Errorf("invalid retained Signal event")
	}
	if in := e.Inbound; in != nil {
		if in.Provider != "signal" || in.Account != c.Account || in.NativeID != e.Key || len(in.Text) > 4096 || !utf8.ValidString(in.Text) ||
			!protocol.ValidToken(in.Actor) || !protocol.ValidToken(in.Conversation) || !safefs.DigestPattern.MatchString(in.DestinationFingerprint) || in.SentAt.IsZero() {
			return fmt.Errorf("invalid retained Signal request")
		}
	}
	return nil
}
