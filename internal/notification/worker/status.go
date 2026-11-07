package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func (s *Store) Replace(ctx context.Context, v protocol.Snapshot) error {
	if err := v.Validate(); err != nil {
		return err
	}
	e, err := s.config.enrollment(v.Producer)
	if err != nil {
		return err
	}
	if !e.PolicyHealth && len(v.Support) > 0 {
		return fmt.Errorf("policy health not enrolled")
	}
	for _, sub := range v.Subjects {
		if !e.PolicyHealth && sub.FaultRevision != 0 {
			return fmt.Errorf("policy health not enrolled")
		}
	}
	now := s.now()
	if v.EvaluatedAt.After(now.Add(5*time.Second)) || now.Sub(v.EvaluatedAt) > time.Duration(e.MaxSilenceSeconds)*time.Second {
		return fmt.Errorf("stale or future evaluation")
	}
	for _, sub := range v.Subjects {
		if sub.ValidUntil.After(v.EvaluatedAt.Add(time.Duration(e.MaxEvidenceSeconds) * time.Second)) {
			return fmt.Errorf("evidence horizon exceeds enrollment")
		}
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := s.requireUnretiredProducer(v.Producer); err != nil {
			return err
		}
		var seq, basis uint64
		var hash string
		err := tx.QueryRow("SELECT seq,basis,hash FROM status WHERE producer=?", v.Producer.ID).Scan(&seq, &basis, &hash)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if v.Sequence == seq {
			if hash == protocol.Hash(v) {
				return nil
			}
			return fmt.Errorf("conflicting snapshot replay")
		}
		if v.Sequence < seq || v.BasisRevision < basis {
			return fmt.Errorf("snapshot revision decreased")
		}
		b, _ := json.Marshal(v)
		if err = s.capacity(tx, v.Producer.ID, len(b), false); err != nil {
			return err
		}
		if e.PolicyHealth {
			if err = s.recordPolicy(tx, v); err != nil {
				return err
			}
		} else {
			var enrolled int
			if err = tx.QueryRow("SELECT (SELECT count(*) FROM policy_faults WHERE producer=?)+(SELECT count(*) FROM policy_support WHERE producer=?)", v.Producer.ID, v.Producer.ID).Scan(&enrolled); err != nil {
				return err
			}
			if enrolled > 0 {
				return fmt.Errorf("policy capability removal requires migration")
			}
		}
		// Missing subjects are unknown, never implicitly healthy or retired.
		rows, err := tx.Query("SELECT id,body FROM subjects WHERE producer=?", v.Producer.ID)
		if err != nil {
			return err
		}
		present := map[string]bool{}
		for _, sub := range v.Subjects {
			present[sub.Key] = true
		}
		var omitted []protocol.Subject
		for rows.Next() {
			var key, body string
			if err = rows.Scan(&key, &body); err != nil {
				rows.Close()
				return err
			}
			if !present[key] {
				var sub protocol.Subject
				if err = protocol.Decode([]byte(body), &sub, 8192); err != nil {
					rows.Close()
					return err
				}
				if sub.Condition != "retired" {
					sub.Condition = "unknown"
					sub.Reason = "omitted from current snapshot"
					omitted = append(omitted, sub)
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO status VALUES(?,?,?,?,?,?,?) ON CONFLICT(producer) DO UPDATE SET seq=excluded.seq,basis=excluded.basis,hash=excluded.hash,body=excluded.body,received=excluded.received,session=excluded.session", v.Producer.ID, v.Sequence, v.BasisRevision, protocol.Hash(v), string(b), now.UnixNano(), s.session); err != nil {
			return err
		}
		for _, sub := range append(append([]protocol.Subject{}, v.Subjects...), omitted...) {
			sub = clampSupport(sub, v.Support)
			data, _ := json.Marshal(sub)
			latch := ""
			if sub.Condition == "critical" || sub.Condition == "warning" {
				latch = sub.Condition
			}
			if e.PolicyHealth {
				if err = tx.QueryRow(`SELECT CASE WHEN EXISTS(SELECT 1 FROM policy_faults WHERE producer=? AND subject=? AND active=1 AND condition='critical') THEN 'critical' WHEN EXISTS(SELECT 1 FROM policy_faults WHERE producer=? AND subject=? AND active=1 AND condition='warning') THEN 'warning' ELSE '' END`, v.Producer.ID, sub.Key, v.Producer.ID, sub.Key).Scan(&latch); err != nil {
					return err
				}
			}
			_, err = tx.Exec(`INSERT INTO subjects VALUES(?,?,?,?) ON CONFLICT(producer,id) DO UPDATE SET body=excluded.body,latched=CASE WHEN ? IN ('unknown','paused') THEN subjects.latched ELSE excluded.latched END`, v.Producer.ID, sub.Key, string(data), latch, sub.Condition)
			if err != nil {
				return err
			}
		}
		if e.PolicyHealth {
			if err = s.applyTransitions(tx, v.Producer); err != nil {
				return err
			}
		}
		return s.sweep(tx, v.Producer.ID)
	})
}

// eligibility evaluates observed evidence, not the time a receipt is imported.
func (s *Store) eligibility(tx *sql.Tx, m protocol.Message, budget time.Duration) (a protocol.Acceptance, err error) {
	a.ObservedAt = s.now().UTC()
	if !s.clockSafe() {
		a.Reason = "waiting_status"
		return a, nil
	}
	a.ValidUntil = m.ExpiresAt
	if !m.ExpiresAt.IsZero() && !m.ExpiresAt.After(a.ObservedAt.Add(budget)) {
		a.Reason = "expired"
		return
	}
	old, err := s.obsolete(tx, m)
	if err != nil {
		return a, err
	}
	if old {
		a.Reason = "superseded"
		return
	}
	if !protocol.Guarded(m.Kind) {
		a.Eligible = true
		return
	}
	var body, session string
	var received int64
	err = tx.QueryRow("SELECT body,received,session FROM status WHERE producer=?", m.Producer.ID).Scan(&body, &received, &session)
	if err == sql.ErrNoRows {
		a.Reason = "waiting_status"
		return a, nil
	}
	if err != nil {
		return a, err
	}
	var snap protocol.Snapshot
	if err = protocol.Decode([]byte(body), &snap, protocol.MaxRequest); err != nil {
		return a, err
	}
	a.StatusSequence = snap.Sequence
	a.BasisRevision = snap.BasisRevision
	e, _ := s.config.enrollment(m.Producer)
	deadline := snap.EvaluatedAt.Add(time.Duration(e.MaxSilenceSeconds) * time.Second)
	if received > a.ObservedAt.UnixNano() || session != s.session || snap.BasisRevision != m.BasisRevision || !deadline.After(a.ObservedAt.Add(budget)) {
		a.Reason = "waiting_status"
		return a, nil
	}
	// Stale/failed evidence can be reported freshly. Positive subjects additionally
	// cap the claims' validity; refresh does not extend their original evidence.
	for _, sub := range snap.Subjects {
		sub = clampSupport(sub, snap.Support)
		if e.PolicyHealth && sub.Condition == "ok" {
			var effectiveBody, latch string
			if err = tx.QueryRow("SELECT body,latched FROM subjects WHERE producer=? AND id=?", m.Producer.ID, sub.Key).Scan(&effectiveBody, &latch); err != nil {
				return a, err
			}
			var effective protocol.Subject
			if err = protocol.Decode([]byte(effectiveBody), &effective, 8192); err != nil {
				return a, err
			}
			if effective.Condition != "ok" || latch != "" {
				a.Reason = "waiting_status"
				return a, nil
			}
		}
		if sub.Condition == "ok" && sub.ValidUntil.Before(deadline) {
			deadline = sub.ValidUntil
		}
	}
	if a.ValidUntil.IsZero() || deadline.Before(a.ValidUntil) {
		a.ValidUntil = deadline
	}
	if !a.ValidUntil.After(a.ObservedAt.Add(budget)) {
		a.Reason = "expired"
		return a, nil
	}
	a.Eligible = true
	return a, nil
}

type Health struct {
	LEDOutputDisposition   *LEDOutputStatus              `json:"led_output_disposition,omitempty"`
	CommandAdmissionHash   string                        `json:"command_admission_hash,omitempty"`
	CommandMode            string                        `json:"command_mode,omitempty"`
	DeliveryActivationHash string                        `json:"delivery_activation_hash,omitempty"`
	SignalProfilesHash     string                        `json:"signal_profiles_hash,omitempty"`
	FreshSignalServiceHash string                        `json:"fresh_signal_service_hash,omitempty"`
	SignalVerificationHash string                        `json:"signal_verification_hash,omitempty"`
	TelegramRenewalID      string                        `json:"telegram_renewal_id,omitempty"`
	RecoveryCommitHash     string                        `json:"recovery_commit_hash,omitempty"`
	InheritedLEDFaults     map[string]int                `json:"inherited_led_faults,omitempty"`
	RetiredProducers       []string                      `json:"retired_producers,omitempty"`
	InstallationRevision   string                        `json:"installation_revision,omitempty"`
	VerificationOnly       bool                          `json:"verification_only,omitempty"`
	LEDOperations          map[string]LEDOperationStatus `json:"led_operations,omitempty"`
	Bindings               map[string]LEDBindingHealth   `json:"led_bindings,omitempty"`
	LEDOutputs             map[string]LEDObservation     `json:"led_outputs,omitempty"`
	LED                    *LEDObservation               `json:"led,omitempty"`
	Loops                  map[string]RuntimeLoopHealth  `json:"loops,omitempty"`
	Identity               protocol.Identity             `json:"identity"`
	Condition              string                        `json:"condition"`
	ValidUntil             time.Time                     `json:"valid_until"`
	Subjects               map[string][]protocol.Subject `json:"subjects"`
}

func rank(c string) int {
	switch c {
	case "critical":
		return 5
	case "unknown":
		return 4
	case "warning":
		return 3
	case "paused":
		return 2
	case "ok":
		return 1
	}
	return 0
}

// Called with the state mutex held. time.Time.Sub uses monotonic readings when
// available, while UnixNano observes wall-clock corrections.
func (s *Store) clockSafe() bool {
	now := s.now()
	if !s.clockLast.IsZero() {
		wall := time.Duration(now.UnixNano() - s.clockLast.UnixNano())
		elapsed := now.Sub(s.clockLast)
		if wall < -time.Second || wall-elapsed > 2*time.Second || elapsed-wall > 2*time.Second {
			s.clockUncertain = true
		}
	}
	s.clockLast = now
	return !s.clockUncertain
}

func (s *Store) Health(ctx context.Context) (Health, error) { return s.health(ctx, false) }

func (s *Store) health(ctx context.Context, resolveInheritedWorker bool) (h Health, err error) {
	h = Health{FreshSignalServiceHash: s.freshSignalServiceHash, DeliveryActivationHash: s.deliveryActivationHash, SignalProfilesHash: s.signalProfilesHash, SignalVerificationHash: s.signalVerificationHash, Identity: s.identity, Condition: "ok", Subjects: map[string][]protocol.Subject{}, VerificationOnly: s.verificationOnly, InstallationRevision: s.installationRevision, RecoveryCommitHash: s.recoveryCommitHash, TelegramRenewalID: s.telegramRenewalID}
	if len(s.config.Producers) == 0 {
		// An unbound gateway has no production liveness evidence.
		h.Condition = "unknown"
	}
	h.CommandMode, h.CommandAdmissionHash = s.managedCommandMode, s.managedCommandHash
	active := map[string]bool{}
	retired := map[string]*ProducerRetirement{}
	for _, p := range s.config.Producers {
		if err = s.ownership(p.Producer, func(o *ownership) error {
			active[p.Producer.ID] = o.Active != "" && o.Generations[o.Active] == "active"
			retired[p.Producer.ID] = o.Retirement
			return nil
		}); err != nil {
			return h, err
		}
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		output, present, recorded, outputErr := s.readLEDOutput(ctx, tx)
		if outputErr != nil {
			return outputErr
		}
		if present {
			status := ledOutputStatus(output, recorded)
			h.LEDOutputDisposition = &status
		}
		clockOK := s.clockSafe()
		inherited, e := s.recoveredLEDFaults(ctx, tx)
		if e != nil {
			return e
		}
		inheritedConditions := map[string]string{}
		if len(inherited) > 0 {
			h.InheritedLEDFaults = map[string]int{}
			for _, f := range inherited {
				h.InheritedLEDFaults[f.Binding]++
				if resolveInheritedWorker && f.Binding == workerLEDBinding {
					continue
				}
				condition := "unknown"
				if f.Condition == "critical" {
					condition = "critical"
				}
				if rank(condition) > rank(inheritedConditions[f.Binding]) {
					inheritedConditions[f.Binding] = condition
				}
			}
		}
		bindingSubjects := map[string][]protocol.Subject{}
		if err := s.capacity(tx, "", 0, false); err != nil {
			return err
		}
		for _, p := range s.config.Producers {
			if retired[p.Producer.ID] != nil {
				if err := s.verifyRetirement(ctx, tx, retired[p.Producer.ID]); err != nil {
					return err
				}
				h.RetiredProducers = append(h.RetiredProducers, p.Producer.ID)
				continue
			}
			var body, session string
			var received int64
			e := tx.QueryRow("SELECT body,received,session FROM status WHERE producer=?", p.Producer.ID).Scan(&body, &received, &session)
			fresh := false
			var deadline time.Time
			if e == nil {
				var snap protocol.Snapshot
				if e = protocol.Decode([]byte(body), &snap, protocol.MaxRequest); e != nil {
					return e
				}
				deadline = snap.EvaluatedAt.Add(time.Duration(p.MaxSilenceSeconds) * time.Second)
				fresh = clockOK && active[p.Producer.ID] && session == s.session && deadline.After(s.now()) && received <= s.now().UnixNano()
			}
			if e != nil && e != sql.ErrNoRows {
				return e
			}
			if p.Required {
				if !fresh && rank(h.Condition) < rank("unknown") {
					h.Condition = "unknown"
				}
				if h.ValidUntil.IsZero() || deadline.Before(h.ValidUntil) {
					h.ValidUntil = deadline
				}
			}
			rows, e := tx.Query("SELECT body,latched FROM subjects WHERE producer=? ORDER BY id", p.Producer.ID)
			if e != nil {
				return e
			}
			for rows.Next() {
				var b, latch string
				var sub protocol.Subject
				if e = rows.Scan(&b, &latch); e != nil {
					rows.Close()
					return e
				}
				if e = protocol.Decode([]byte(b), &sub, 8192); e != nil {
					rows.Close()
					return e
				}
				selected := sub
				if sub.Condition == "retired" && latch == "" {
					// Retirement is durable domain state. A fresh producer session
					// renews liveness, not the retired subject's old healthy lease.
					selected.ValidUntil = deadline
				}
				if !fresh || !selected.ValidUntil.After(s.now()) {
					selected.Condition = "unknown"
				}
				if rank(latch) > rank(selected.Condition) {
					selected.Condition = latch
				}
				if deadline.Before(selected.ValidUntil) {
					selected.ValidUntil = deadline
				}
				bindingSubjects[p.Producer.ID] = append(bindingSubjects[p.Producer.ID], selected)
				if sub.Condition == "retired" && latch == "" {
					continue
				}
				if !fresh || !sub.ValidUntil.After(s.now()) {
					sub.Condition = "unknown"
				}
				if rank(latch) > rank(sub.Condition) {
					sub.Condition = latch
				}
				if rank(sub.Condition) > rank(h.Condition) {
					h.Condition = sub.Condition
				}
				if (sub.Condition == "ok" || sub.Condition == "paused") && (h.ValidUntil.IsZero() || sub.ValidUntil.Before(h.ValidUntil)) {
					h.ValidUntil = sub.ValidUntil
				}
				h.Subjects[p.Producer.ID] = append(h.Subjects[p.Producer.ID], sub)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
		}
		var bad int
		if e := tx.QueryRow("SELECT (SELECT count(*) FROM messages WHERE state IN ('ambiguous','blocked','retry_wait','fenced')) + (SELECT count(*) FROM command_replies WHERE state IN ('ambiguous','blocked'))").Scan(&bad); e != nil {
			return e
		}
		if bad > 0 && rank(h.Condition) < rank("warning") {
			h.Condition = "warning"
		}
		if len(h.Subjects) == 0 {
			allRetired, count := true, 0
			for _, subjects := range bindingSubjects {
				for _, sub := range subjects {
					count++
					allRetired = allRetired && sub.Condition == "retired"
				}
			}
			if h.Condition == "ok" && allRetired && (count > 0 || len(h.RetiredProducers) == len(s.config.Producers)) {
				h.Condition = "retired"
			} else {
				h.Condition = "unknown"
			}
		}
		s.runtimeHealth(&h)
		if len(s.config.LEDBindings) > 0 {
			h.Bindings = map[string]LEDBindingHealth{}
			for _, binding := range s.config.LEDBindings {
				selected := selectLEDBinding(binding, bindingSubjects[binding.Producer.ID], s.now())
				if retired[binding.Producer.ID] != nil {
					selected = LEDBindingHealth{Condition: "retired"}
				}
				if inherited := inheritedConditions[binding.ID]; inherited != "" {
					if rank(inherited) > rank(selected.Condition) {
						selected.Condition = inherited
					}
					selected.ValidUntil = time.Time{}
				}
				loopHealth := Health{Condition: selected.Condition, ValidUntil: selected.ValidUntil}
				s.runtimeHealth(&loopHealth)
				selected.Condition, selected.ValidUntil = loopHealth.Condition, loopHealth.ValidUntil
				h.Bindings[binding.ID] = selected
				if rank(selected.Condition) > rank(h.Condition) {
					h.Condition = selected.Condition
				}
				if (selected.Condition == "ok" || selected.Condition == "paused") && (h.ValidUntil.IsZero() || selected.ValidUntil.Before(h.ValidUntil)) {
					h.ValidUntil = selected.ValidUntil
				}
			}
		}
		for _, condition := range inheritedConditions {
			if rank(condition) > rank(h.Condition) {
				h.Condition = condition
			}
			h.ValidUntil = time.Time{}
		}
		s.ledHealth(&h)
		if s.verificationOnly {
			if rank(h.Condition) < rank("unknown") {
				h.Condition = "unknown"
			}
			h.ValidUntil = time.Time{}
		}
		return nil
	})
	return
}
