package interaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type CommandRecoveryManifest struct {
	Identity protocol.Identity
	Producer protocol.Producer
	Commands uint64
	Results  uint64
	Digest   string
}
type RecoveryResult struct {
	Result       Result
	Receipt      *ResultReceipt
	Acknowledged bool
}
type CommandRecoveryRecord struct {
	Command               Command
	Phase, State, GrantID string
	Revision              uint64
	Fenced                bool
	Results               []RecoveryResult
	NonAdmission          *NonAdmissionProof
	HistoricalIdentity    *protocol.Identity `json:"historical_identity,omitempty"`
}
type CommandRecoverySummary struct {
	Challenge                            protocol.RecoveryChallenge
	Manifest                             CommandRecoveryManifest
	Terminal, Uncertain, MissingRestored uint64
	ExecutionEnabled                     bool
}
type CommandRecoverySession interface {
	CommandChallenge(context.Context, protocol.Producer) (protocol.RecoveryChallenge, error)
	BeginCommands(context.Context, protocol.RecoveryChallenge, CommandRecoveryManifest) error
	ImportCommands(context.Context, protocol.RecoveryChallenge, uint64, []CommandRecoveryRecord) error
	FinishCommands(context.Context, protocol.RecoveryChallenge) (CommandRecoverySummary, error)
	CommandSummary(context.Context, protocol.Producer) (CommandRecoverySummary, error)
	Close() error
}

// The digest binds the ordered full evidence, including receipts and their old
// epochs. An empty collection has a real digest; it is not proof of no execution.
func CommandRecoveryDigest(hashes []string) string {
	return RecoveryEvidenceDigest(hashes)
}

func RecoveryEvidenceDigest(hashes []string) string {
	h := sha256.New()
	for _, hash := range hashes {
		h.Write([]byte(hash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ValidateCommandRecoveryChallenge(ch protocol.RecoveryChallenge, old protocol.Identity, p protocol.Producer, now time.Time, boot string, uptime float64) error {
	if !old.Valid() || !p.Valid() || !ch.Identity.Valid() || ch.Identity.Worker != old.Worker || ch.Identity.Recovery == old.Recovery ||
		ch.Producer != p || !safefs.ValidID(ch.Operation, "n") || !safefs.ValidID(ch.Nonce, "n") {
		return fmt.Errorf("command recovery challenge binding mismatch")
	}
	if ch.IssuedAt.IsZero() || !ch.ExpiresAt.After(ch.IssuedAt) || ch.ExpiresAt.Sub(ch.IssuedAt) > 10*time.Minute ||
		now.Before(ch.IssuedAt) || !now.Before(ch.ExpiresAt) || !protocol.ValidToken(boot) || boot != ch.BootID ||
		math.IsNaN(uptime) || math.IsInf(uptime, 0) || math.IsNaN(ch.Uptime) || math.IsInf(ch.Uptime, 0) ||
		ch.Uptime < 0 || uptime < ch.Uptime || uptime-ch.Uptime >= 600 {
		return fmt.Errorf("command recovery challenge expired or clock invalid")
	}
	return nil
}

func (v CommandRecoveryRecord) Validate(m CommandRecoveryManifest) error {
	c := v.Command
	if v.HistoricalIdentity != nil {
		if !v.Fenced || !v.HistoricalIdentity.Valid() || v.HistoricalIdentity.Worker != m.Identity.Worker || *v.HistoricalIdentity == m.Identity {
			return fmt.Errorf("invalid historical command epoch")
		}
		m.Identity = *v.HistoricalIdentity
	}
	raw, err := json.Marshal(v)
	if err != nil || len(raw)+4098 > protocol.MaxRequest || c.Producer != m.Producer || !protocol.ValidToken(c.ID) ||
		!protocol.ValidToken(c.RequestKey) || !protocol.ValidToken(c.InputHash) || c.Scope.Validate() != nil || c.Deadline.IsZero() ||
		v.Revision != uint64(len(v.Results)) || len(v.Results) > 8 {
		return fmt.Errorf("invalid retained command")
	}
	if v.Phase != "" && v.Phase != "prepare" && v.Phase != "execute" {
		return fmt.Errorf("invalid retained command phase")
	}
	if v.State != "imported" && v.State != "needs_confirmation" && v.State != "running" && v.State != "outcome_unknown" && !Terminal(v.State) {
		return fmt.Errorf("invalid retained command state")
	}
	if (v.Phase == "") != (v.Revision == 0) || (v.Phase == "execute" && !protocol.ValidToken(v.GrantID)) || (v.Phase != "execute" && v.GrantID != "") {
		return fmt.Errorf("invalid retained admission")
	}
	if v.Phase == "" && v.State != "imported" && (!v.Fenced || (v.State != "expired" && v.State != "rejected")) {
		return fmt.Errorf("unexplained retained outcome")
	}
	for i, e := range v.Results {
		r := e.Result
		if r.Validate() != nil || r.CommandID != c.ID || r.CommandHash != protocol.Hash(c) || r.Revision != uint64(i+1) || !c.Scope.Allows(r.Scope) {
			return fmt.Errorf("retained command result mismatch")
		}
		if (r.Phase == "prepare" && (!c.Descriptor.Confirm || i != 0)) || (r.Phase == "execute" && c.Descriptor.Confirm && i == 0) {
			return fmt.Errorf("retained confirmation history missing or inconsistent")
		}
		if i > 0 {
			previous := v.Results[i-1].Result
			if Terminal(previous.State) || !previous.Scope.Allows(r.Scope) || r.Phase != "execute" || (previous.Phase == "execute" && (previous.State != "running" || r.JobID != previous.JobID || !Terminal(r.State))) {
				return fmt.Errorf("invalid retained result progression")
			}
		}
		if e.Acknowledged && e.Receipt == nil {
			return fmt.Errorf("retained result acknowledgement lacks receipt")
		}
		if e.Receipt != nil {
			a := e.Receipt
			if a.Identity != m.Identity || a.Producer != m.Producer || a.CommandID != c.ID || a.Revision != r.Revision || a.Hash != protocol.Hash(r) {
				return fmt.Errorf("retained result receipt mismatch")
			}
			switch a.Disposition {
			case "applied":
			case "execution_conflict":
				if !v.Fenced {
					return fmt.Errorf("unfenced execution conflict")
				}
			case "superseded_no_effect":
				if (r.Phase != "prepare" && r.State != "rejected") || !a.NonAdmission || !protocol.ValidToken(a.TerminalDecisionID) ||
					(a.TerminalState != "expired" && a.TerminalState != "rejected") || !v.Fenced || v.Phase == "execute" {
					return fmt.Errorf("invalid retained supersession")
				}
			default:
				return fmt.Errorf("invalid retained result receipt disposition")
			}
		}
	}
	if v.Revision > 0 {
		last := v.Results[len(v.Results)-1].Result
		if last.Phase != v.Phase || (last.State != v.State && (!v.Fenced || v.Phase == "execute" || (v.State != "expired" && v.State != "rejected"))) {
			return fmt.Errorf("retained command head mismatch")
		}
	}
	if v.NonAdmission != nil {
		p := *v.NonAdmission
		p.ID = ""
		if v.Phase == "execute" || !v.Fenced || p.Identity != m.Identity || p.Producer != m.Producer || p.CommandID != c.ID ||
			p.CommandHash != protocol.Hash(c) || !protocol.ValidToken(p.GrantID) || v.NonAdmission.ID != protocol.Hash(p) {
			return fmt.Errorf("invalid retained non-admission proof")
		}
	}
	return nil
}
