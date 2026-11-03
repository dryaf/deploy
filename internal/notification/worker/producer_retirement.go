package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// ProducerRetirement is permanent for an enrolled producer epoch. It preserves
// all history and grants neither cancellation nor fault-resolution authority.
type ProducerRetirement struct {
	Version      int               `json:"version"`
	ID           string            `json:"id"`
	Identity     protocol.Identity `json:"identity"`
	Producer     protocol.Producer `json:"producer"`
	Actor        string            `json:"actor"`
	Reason       string            `json:"reason"`
	At           time.Time         `json:"at"`
	EvidenceHash string            `json:"evidence_hash"`
}

func validRetirementReason(actor, reason string) bool {
	return protocol.ValidToken(actor) && len(reason) > 0 && len(reason) <= 1024 && utf8.ValidString(reason) && strings.TrimSpace(reason) == reason && !strings.ContainsAny(reason, "\x00\r\n")
}

func ValidateProducerRetirementReason(actor, reason string) error {
	if !validRetirementReason(actor, reason) {
		return fmt.Errorf("retirement requires a bounded actor and single-line reason")
	}
	return nil
}

func (v ProducerRetirement) validate() error {
	if v.Version != 1 || !safefs.ValidID(v.ID, "n") || !v.Identity.Valid() || !v.Producer.Valid() || !validRetirementReason(v.Actor, v.Reason) || v.At.IsZero() || !safefs.DigestPattern.MatchString(v.EvidenceHash) {
		return fmt.Errorf("invalid producer retirement record")
	}
	return nil
}

type ProducerRetirementReview struct {
	Producer     protocol.Producer   `json:"producer"`
	Eligible     bool                `json:"eligible"`
	Applied      bool                `json:"applied"`
	EvidenceHash string              `json:"evidence_hash"`
	Retirement   *ProducerRetirement `json:"retirement,omitempty"`
}

// requireUnretiredProducer is used inside the state transaction. Retirement
// holds that same mutex through its independent ownership publication, so new
// work cannot slip between the final prerequisite check and the durable fence.
// This reads both ownership records but never repairs an incomplete transition.
func (s *Store) requireUnretiredProducer(p protocol.Producer) error {
	o, err := s.readOwnership(p)
	if err != nil {
		return err
	}
	if o.Retirement != nil {
		return fmt.Errorf("producer epoch is permanently retired")
	}
	return nil
}

func (s *Store) retirementEvidence(ctx context.Context, tx *sql.Tx, p protocol.Producer, checkLED bool) (string, error) {
	var evidence struct {
		Producer                     protocol.Producer
		Submitted                    uint64
		Acked, Latest                int64
		Messages, Commands, Subjects int
		StatusHash                   string
	}
	evidence.Producer = p
	if err := tx.QueryRowContext(ctx, "SELECT submitted,acked FROM producers WHERE id=? AND epoch=?", p.ID, p.Epoch).Scan(&evidence.Submitted, &evidence.Acked); err != nil {
		return "", err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(max(position),0) FROM receipts WHERE producer=?", p.ID).Scan(&evidence.Latest); err != nil {
		return "", err
	}
	if evidence.Acked != evidence.Latest {
		return "", fmt.Errorf("producer must acknowledge every retained receipt before retirement")
	}
	checks := []struct{ query, why string }{
		{"SELECT count(*) FROM messages WHERE producer=? AND (state NOT IN ('provider_accepted','cancelled','superseded','expired') OR attempt<>'')", "delivery obligations remain"},
		{"SELECT count(*) FROM subjects WHERE producer=? AND (latched<>'' OR json_extract(body,'$.condition') IS NOT 'retired')", "domain subjects must be explicitly retired first"},
		{"SELECT count(*) FROM policy_faults WHERE producer=? AND active<>0", "policy faults remain"},
		{"SELECT count(*) FROM fault_transitions WHERE producer=? AND state='pending'", "policy transitions remain"},
		{"SELECT count(*) FROM commands WHERE producer=? AND state NOT IN ('succeeded','failed','rejected','expired')", "command outcomes remain unsettled"},
		{"SELECT count(*) FROM command_results r JOIN commands c ON c.id=r.command_id WHERE c.producer=? AND r.acked<>1", "command result acknowledgements remain"},
		{"SELECT count(*) FROM command_replies r JOIN commands c ON c.id=r.command_id WHERE c.producer=? AND (r.state NOT IN ('provider_accepted','expired','superseded') OR r.attempt<>'')", "command replies remain unsettled"},
	}
	for _, check := range checks {
		var count int
		if err := tx.QueryRowContext(ctx, check.query, p.ID).Scan(&count); err != nil {
			return "", err
		}
		if count != 0 {
			return "", fmt.Errorf("producer retirement refused: %s", check.why)
		}
	}
	// The aggregate cannot attribute an old LED fault to one producer. Preserve
	// it conservatively; retirement itself never invents clear authority.
	bindings := []string{workerLEDBinding}
	for _, b := range s.config.LEDBindings {
		if b.Producer == p {
			bindings = append(bindings, b.ID)
		}
	}
	for _, b := range bindings {
		if !checkLED {
			break
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM led_states WHERE binding=?", b).Scan(&count); err != nil {
			return "", err
		}
		if count != 0 {
			return "", fmt.Errorf("LED fault/cleanup obligations remain for binding %s", b)
		}
	}
	for _, item := range []struct {
		table string
		out   *int
	}{{"messages", &evidence.Messages}, {"commands", &evidence.Commands}, {"subjects", &evidence.Subjects}} {
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM "+item.table+" WHERE producer=?", p.ID).Scan(item.out); err != nil {
			return "", err
		}
	}
	var body string
	err := tx.QueryRowContext(ctx, "SELECT body,hash FROM status WHERE producer=?", p.ID).Scan(&body, &evidence.StatusHash)
	if err != nil && err != sql.ErrNoRows {
		return "", err
	}
	if err == nil {
		var snapshot protocol.Snapshot
		if protocol.Decode([]byte(body), &snapshot, protocol.MaxRequest) != nil || snapshot.Validate() != nil || snapshot.Producer != p || protocol.Hash(snapshot) != evidence.StatusHash {
			return "", fmt.Errorf("retirement status evidence is inconsistent")
		}
	}
	return protocol.Hash(evidence), nil
}

// RetireProducer requires the offline worker lease. Preview validates the same
// prerequisites without publishing ownership. Apply is a single durable ownership
// transition; exact retries retain its ID, reason, timestamp and evidence.
func (s *Store) RetireProducer(ctx context.Context, p protocol.Producer, actor, reason string, apply bool) (out ProducerRetirementReview, err error) {
	out.Producer = p
	if !validRetirementReason(actor, reason) {
		return out, fmt.Errorf("retirement requires a bounded actor and single-line reason")
	}
	if err = s.checkRetirementLease(); err != nil {
		return out, err
	}
	err = s.ownership(p, func(o *ownership) error {
		return s.transaction(ctx, func(tx *sql.Tx) error {
			if err := s.checkRetirementLease(); err != nil {
				return err
			}
			if o.Retirement != nil {
				if err := s.verifyRetirement(ctx, tx, o.Retirement); err != nil {
					return err
				}
				if apply && (o.Retirement.Actor != actor || o.Retirement.Reason != reason) {
					return fmt.Errorf("producer already retired with a different decision")
				}
				out.Applied, out.Retirement, out.EvidenceHash = true, o.Retirement, o.Retirement.EvidenceHash
				return nil
			}
			if o.RecoveryPending != nil || o.Active != "" {
				return fmt.Errorf("producer recovery/delivery ownership must be settled before retirement")
			}
			for _, state := range o.Generations {
				if state != "revoked" {
					return fmt.Errorf("all producer generations must be explicitly revoked before retirement")
				}
			}
			if !s.clockSafe() {
				return fmt.Errorf("retirement requires a reliable clock")
			}
			hash, e := s.retirementEvidence(ctx, tx, p, true)
			if e != nil {
				return e
			}
			out.Eligible, out.EvidenceHash = true, hash
			if !apply {
				return nil
			}
			id, e := safefs.NewID("n")
			if e != nil {
				return e
			}
			r := &ProducerRetirement{1, id, s.identity, p, actor, reason, s.now().UTC(), hash}
			event := ownershipTransition{Schema: 4, Identity: o.Identity, Producer: p, Revision: o.Revision + 1, Previous: o.History, BeforeHash: protocol.Hash(*o), Operation: "retire", Retirement: r}
			if e = ctx.Err(); e != nil {
				return e
			}
			if e = s.checkRetirementLease(); e != nil {
				return e
			}
			if e = s.commitOwnershipEvent(o, event); e != nil {
				return e
			}
			out.Applied, out.Retirement = true, o.Retirement
			return nil
		})
	})
	return
}

func (s *Store) checkRetirementLease() error {
	if s.lease == nil || s.lease.File() == nil {
		return fmt.Errorf("retirement requires the exclusive worker lease")
	}
	held, err := s.lease.File().Stat()
	if err != nil {
		return err
	}
	f, err := s.dir.Open("worker.lock", os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	current, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	if !os.SameFile(held, current) {
		return fmt.Errorf("retirement worker lease changed")
	}
	var id protocol.Identity
	if err = s.dir.ReadJSON("identity.json", &id); err != nil {
		return err
	}
	if id != s.identity {
		return fmt.Errorf("retirement worker identity changed")
	}
	return nil
}

func (s *Store) verifyRetirement(ctx context.Context, tx *sql.Tx, v *ProducerRetirement) error {
	if v == nil {
		return nil
	}
	if err := v.validate(); err != nil {
		return err
	}
	hash, err := s.retirementEvidence(ctx, tx, v.Producer, false)
	if err != nil {
		return err
	}
	if hash != v.EvidenceHash {
		return fmt.Errorf("retired producer ledger changed; recovery required")
	}
	return nil
}

func (s *Store) verifyRetirements(ctx context.Context) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		for _, e := range s.config.Producers {
			o, err := s.readOwnership(e.Producer)
			if err != nil {
				return err
			}
			if err = s.verifyRetirement(ctx, tx, o.Retirement); err != nil {
				return err
			}
		}
		return nil
	})
}
