package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type reconciliationChallenge = protocol.RecoveryChallenge

// Floors are explicit even when individual records have been compacted. They
// never stand in for a receipt or prove that a pending message was not sent.
type producerRecoveryManifest = protocol.RecoveryManifest
type retainedRecoveryReceipt = protocol.RetainedReceipt
type producerRecoveryRecord = protocol.RecoveryRecord
type producerRecoverySummary = protocol.RecoverySummary

func (r *recoveryReconciler) Challenge(ctx context.Context, p protocol.Producer) (challenge reconciliationChallenge, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		boot, uptime, err := r.recoveryClock()
		if err != nil {
			return err
		}
		var body, completion string
		err = tx.QueryRowContext(ctx, "SELECT challenge,completion FROM reconciliation_producers WHERE producer=?", p.ID).Scan(&body, &completion)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			if completion != "" {
				return fmt.Errorf("producer already reconciled; inspect its summary")
			}
			if err = protocol.Decode([]byte(body), &challenge, 4096); err != nil {
				return err
			}
			if r.now().Before(challenge.IssuedAt) || (boot == challenge.BootID && uptime < challenge.Uptime) {
				return fmt.Errorf("reconciliation clock moved backwards")
			}
			if challenge.ExpiresAt.After(r.now()) && boot == challenge.BootID && uptime-challenge.Uptime < 600 {
				return nil
			}
		}
		nonce, err := safefs.NewID("n")
		if err != nil {
			return err
		}
		issued := r.now().UTC()
		challenge = reconciliationChallenge{Operation: r.journal.Operation, Identity: r.journal.After, Producer: p, Nonce: nonce, IssuedAt: issued, ExpiresAt: issued.Add(10 * time.Minute), BootID: boot, Uptime: uptime}
		data, _ := json.Marshal(challenge)
		_, err = tx.ExecContext(ctx, "INSERT INTO reconciliation_producers(producer,challenge) VALUES(?,?) ON CONFLICT(producer) DO UPDATE SET challenge=excluded.challenge", p.ID, string(data))
		return err
	})
	return
}

func (r *recoveryReconciler) recoveryClock() (string, float64, error) {
	if r.clock == nil {
		return "", 0, fmt.Errorf("reconciliation host clock unavailable")
	}
	boot, uptime, err := r.clock()
	if err != nil || !protocol.ValidToken(boot) || uptime < 0 || math.IsNaN(uptime) || math.IsInf(uptime, 0) {
		return "", 0, fmt.Errorf("reconciliation host clock unavailable")
	}
	return boot, uptime, nil
}

func (r *recoveryReconciler) checkChallenge(tx *sql.Tx, ch reconciliationChallenge) (manifest producerRecoveryManifest, completion string, err error) {
	if ch.Operation != r.journal.Operation || ch.Identity != r.journal.After || !safefs.ValidID(ch.Nonce, "n") {
		err = fmt.Errorf("reconciliation challenge identity mismatch")
		return
	}
	if _, err = r.config.enrollment(ch.Producer); err != nil {
		return
	}
	var body, header string
	err = tx.QueryRow("SELECT challenge,manifest,completion FROM reconciliation_producers WHERE producer=?", ch.Producer.ID).Scan(&body, &header, &completion)
	if err != nil {
		return
	}
	var saved reconciliationChallenge
	if err = protocol.Decode([]byte(body), &saved, 4096); err != nil {
		return
	}
	boot, uptime, clockErr := r.recoveryClock()
	if clockErr != nil {
		err = clockErr
		return
	}
	if saved != ch || r.now().Before(ch.IssuedAt) || !ch.ExpiresAt.After(r.now()) || boot != ch.BootID || uptime < ch.Uptime || uptime-ch.Uptime >= 600 {
		err = fmt.Errorf("stale or mismatched reconciliation challenge")
		return
	}
	if header != "" {
		err = protocol.Decode([]byte(header), &manifest, 4096)
	}
	return
}

func (r *recoveryReconciler) Begin(ctx context.Context, ch reconciliationChallenge, m producerRecoveryManifest) error {
	validSeq := func(v uint64) bool { return v == 0 || protocol.Sequence(v) }
	if !validSeq(m.HighestIssued) || !validSeq(m.HighestAcknowledged) || !validSeq(m.CompactedThrough) ||
		m.CompactedThrough > m.HighestAcknowledged || m.HighestAcknowledged > m.HighestIssued ||
		(m.CompactedThrough > 0 && !safefs.DigestPattern.MatchString(m.OutcomeCheckpoint)) || (m.CompactedThrough == 0 && m.OutcomeCheckpoint != "") ||
		m.ImportedReceipts.Identity != r.journal.Before || m.ImportedReceipts.Producer != ch.Producer || m.ImportedReceipts.Position < 0 {
		return fmt.Errorf("invalid producer recovery watermarks")
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		_, completed, err := r.checkChallenge(tx, ch)
		if err != nil {
			return err
		}
		if completed != "" {
			return fmt.Errorf("reconciliation already completed")
		}
		var existing string
		if err = tx.QueryRow("SELECT manifest FROM reconciliation_producers WHERE producer=?", ch.Producer.ID).Scan(&existing); err != nil {
			return err
		}
		body, _ := json.Marshal(m)
		if existing != "" {
			if existing != string(body) {
				return fmt.Errorf("reconciliation manifest changed")
			}
			return nil
		}
		var submitted uint64
		var acked int64
		if err = tx.QueryRow("SELECT submitted,acked FROM recovery_producers WHERE id=?", ch.Producer.ID).Scan(&submitted, &acked); err != nil {
			return err
		}
		if m.HighestIssued < submitted || m.ImportedReceipts.Position < acked {
			return fmt.Errorf("producer retained history contradicts restored watermarks")
		}
		_, err = tx.Exec("UPDATE reconciliation_producers SET manifest=? WHERE producer=?", string(body), ch.Producer.ID)
		return err
	})
}

func (r *recoveryReconciler) Import(ctx context.Context, ch reconciliationChallenge, records []producerRecoveryRecord) error {
	data, err := json.Marshal(records)
	if err != nil || len(records) == 0 || len(records) > 128 || len(data)+4096 > protocol.MaxRequest {
		return fmt.Errorf("reconciliation page exceeds bounds")
	}
	return r.transaction(ctx, func(tx *sql.Tx) error {
		m, completed, err := r.checkChallenge(tx, ch)
		if err != nil {
			return err
		}
		if completed != "" {
			return fmt.Errorf("reconciliation already completed")
		}
		var header string
		if err = tx.QueryRow("SELECT manifest FROM reconciliation_producers WHERE producer=?", ch.Producer.ID).Scan(&header); err != nil {
			return err
		}
		if header == "" {
			return fmt.Errorf("begin reconciliation before importing evidence")
		}
		if err = r.capacity(tx, len(data)); err != nil {
			return err
		}
		for _, record := range records {
			if err = r.importRecord(tx, ch, m, record); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *recoveryReconciler) importRecord(tx *sql.Tx, ch reconciliationChallenge, manifest producerRecoveryManifest, record producerRecoveryRecord) error {
	m := record.Message
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Producer != ch.Producer || m.Sequence <= manifest.CompactedThrough || m.Sequence > manifest.HighestIssued || m.ObservedAt.After(r.now().Add(5*time.Second)) {
		return fmt.Errorf("recovery record outside producer manifest")
	}
	enrollment, _ := r.config.enrollment(ch.Producer)
	route, ok := r.config.Routes[m.Route]
	if !ok || !slices.Contains(enrollment.Routes, m.Route) {
		return fmt.Errorf("recovery route not enrolled")
	}
	if m.Context != nil {
		if !enrollment.InteractiveContext {
			return fmt.Errorf("recovery context not enrolled")
		}
		for _, target := range m.Context.Targets {
			if !slices.Contains(enrollment.ContextKinds, target.Kind) {
				return fmt.Errorf("recovery context kind not enrolled")
			}
		}
	}
	evidenceHash := protocol.Hash(record)
	var previous string
	err := tx.QueryRow("SELECT evidence_hash FROM reconciliation_records WHERE producer=? AND seq=?", m.Producer.ID, m.Sequence).Scan(&previous)
	if err == nil {
		if previous != evidenceHash {
			return fmt.Errorf("conflicting recovery evidence replay")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	var oldHash, state string
	var seq uint64
	err = tx.QueryRow("SELECT hash,seq,state FROM messages WHERE producer=? AND id=?", m.Producer.ID, m.ID).Scan(&oldHash, &seq, &state)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && (oldHash != protocol.Hash(m) || seq != m.Sequence) {
		return fmt.Errorf("recovery payload conflicts with restored event")
	}
	if err == sql.ErrNoRows {
		body, _ := json.Marshal(m)
		if _, err = tx.Exec("INSERT INTO messages(producer,id,seq,hash,body,generation,route_revision,state) VALUES(?,?,?,?,?,?,?,'ambiguous')", m.Producer.ID, m.ID, m.Sequence, protocol.Hash(m), string(body), m.Generation, route.Revision); err != nil {
			return err
		}
		state = "ambiguous"
	}
	if record.Receipt != nil {
		if err = r.importRetainedReceipt(tx, ch, manifest, m, *record.Receipt, state); err != nil {
			return err
		}
	}
	body, _ := json.Marshal(record)
	_, err = tx.Exec("INSERT INTO reconciliation_records VALUES(?,?,?,?,?,?)", m.Producer.ID, m.Sequence, m.ID, protocol.Hash(m), evidenceHash, string(body))
	return err
}

func (r *recoveryReconciler) importRetainedReceipt(tx *sql.Tx, ch reconciliationChallenge, manifest producerRecoveryManifest, m protocol.Message, e retainedRecoveryReceipt, state string) error {
	v := e.Receipt
	if e.Identity != r.journal.Before || e.Producer != ch.Producer || v.EventID != m.ID || v.Sequence != m.Sequence ||
		v.Position <= 0 || v.Position > manifest.ImportedReceipts.Position || !terminal(v.State) || v.At.IsZero() || v.At.After(r.now().Add(5*time.Second)) {
		return fmt.Errorf("invalid retained receipt provenance")
	}
	if v.State == "provider_accepted" {
		route := r.config.Routes[m.Route]
		if !safefs.ValidID(v.AttemptID, "n") || v.Provider == "" || (v.Provider != route.Primary && v.Provider != route.Fallback) || v.Acceptance == nil ||
			v.Acceptance.ObservedAt.IsZero() || v.Acceptance.ObservedAt.After(r.now().Add(5*time.Second)) {
			return fmt.Errorf("invalid retained acceptance")
		}
		a := v.Acceptance
		if a.Eligible {
			if protocol.Guarded(m.Kind) {
				if !protocol.Sequence(a.StatusSequence) || a.BasisRevision != m.BasisRevision ||
					!a.ValidUntil.After(a.ObservedAt) || a.ValidUntil.After(m.ExpiresAt) {
					return fmt.Errorf("invalid retained acceptance eligibility")
				}
			} else if a.StatusSequence != 0 || a.BasisRevision != 0 || !a.ValidUntil.Equal(m.ExpiresAt) ||
				(!m.ExpiresAt.IsZero() && !m.ExpiresAt.After(a.ObservedAt)) {
				// Unguarded alerts/test messages never borrow a status lease. Their
				// real receipts legitimately have zero status/basis counters.
				return fmt.Errorf("invalid retained alert eligibility")
			}
		}
	} else if v.Acceptance != nil {
		return fmt.Errorf("non-acceptance carries eligibility")
	}
	if terminal(state) && v.State != state {
		if v.State != "provider_accepted" || state == "provider_accepted" || v.Acceptance.Eligible {
			return fmt.Errorf("retained outcome conflicts with restored terminal state")
		}
	}
	// Stored receipt bodies omit the issued position. Compare that normalized
	// body against old gateway evidence before retaining the full producer proof.
	position := v.Position
	v.Position = 0
	var producerID, body string
	err := tx.QueryRow("SELECT producer,body FROM recovery_receipts WHERE position=?", position).Scan(&producerID, &body)
	if err == nil {
		var known protocol.Receipt
		if err = protocol.Decode([]byte(body), &known, 8192); err != nil {
			return err
		}
		known.Position = 0
		if producerID != m.Producer.ID || protocol.Hash(known) != protocol.Hash(v) {
			return fmt.Errorf("retained receipt contradicts gateway history")
		}
	} else if err != sql.ErrNoRows {
		return err
	}
	var hash string
	err = tx.QueryRow("SELECT hash FROM reconciliation_receipts WHERE position=?", position).Scan(&hash)
	if err == nil && hash != protocol.Hash(e) {
		return fmt.Errorf("receipt position claimed by conflicting evidence")
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	encoded, _ := json.Marshal(e)
	if _, err = tx.Exec("INSERT INTO reconciliation_receipts VALUES(?,?,?,?) ON CONFLICT(position) DO NOTHING", position, m.Producer.ID, protocol.Hash(e), string(encoded)); err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE messages SET state=?,attempt='' WHERE producer=? AND id=?", v.State, m.Producer.ID, m.ID)
	return err
}

func (r *recoveryReconciler) Finish(ctx context.Context, ch reconciliationChallenge, snapshot protocol.Snapshot) (summary producerRecoverySummary, err error) {
	if err = snapshot.Validate(); err != nil {
		return
	}
	e, err := r.config.enrollment(ch.Producer)
	if err != nil {
		return summary, err
	}
	now := r.now()
	if snapshot.Producer != ch.Producer || snapshot.EvaluatedAt.Before(ch.IssuedAt) || snapshot.EvaluatedAt.After(now.Add(5*time.Second)) ||
		now.Sub(snapshot.EvaluatedAt) > time.Duration(e.MaxSilenceSeconds)*time.Second {
		return summary, fmt.Errorf("reconciliation needs a fresh challenged snapshot")
	}
	if !e.PolicyHealth && len(snapshot.Support) > 0 {
		return summary, fmt.Errorf("policy support not enrolled")
	}
	for _, subject := range snapshot.Subjects {
		if (!e.PolicyHealth && subject.FaultRevision != 0) || subject.ValidUntil.After(snapshot.EvaluatedAt.Add(time.Duration(e.MaxEvidenceSeconds)*time.Second)) ||
			(subject.Condition == "ok" && !subject.ValidUntil.After(now)) {
			return summary, fmt.Errorf("invalid reconciliation evidence lease")
		}
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		manifest, completed, err := r.checkChallenge(tx, ch)
		if err != nil {
			return err
		}
		if completed != "" {
			if err = protocol.Decode([]byte(completed), &summary, protocol.MaxRequest+8192); err != nil {
				return err
			}
			if protocol.Hash(summary.Snapshot) != protocol.Hash(snapshot) {
				return fmt.Errorf("completed reconciliation snapshot changed")
			}
			return nil
		}
		var header string
		if err = tx.QueryRow("SELECT manifest FROM reconciliation_producers WHERE producer=?", ch.Producer.ID).Scan(&header); err != nil {
			return err
		}
		if header == "" {
			return fmt.Errorf("missing recovery manifest")
		}
		var count uint64
		if err = tx.QueryRow("SELECT count(*) FROM reconciliation_records WHERE producer=?", ch.Producer.ID).Scan(&count); err != nil {
			return err
		}
		if count != manifest.HighestIssued-manifest.CompactedThrough {
			return fmt.Errorf("recovery evidence has submission gaps")
		}
		var seq, basis uint64
		var hash string
		err = tx.QueryRow("SELECT seq,basis,hash FROM status WHERE producer=?", ch.Producer.ID).Scan(&seq, &basis, &hash)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil && (snapshot.Sequence < seq || snapshot.BasisRevision < basis || (snapshot.Sequence == seq && protocol.Hash(snapshot) != hash)) {
			return fmt.Errorf("reconciliation status regressed")
		}
		var acceptedSeq, acceptedBasis uint64
		if err = tx.QueryRow("SELECT COALESCE(max(json_extract(body,'$.Receipt.acceptance.status_sequence')),0),COALESCE(max(json_extract(body,'$.Receipt.acceptance.basis_revision')),0) FROM reconciliation_receipts WHERE producer=?", ch.Producer.ID).Scan(&acceptedSeq, &acceptedBasis); err != nil {
			return err
		}
		if snapshot.Sequence < acceptedSeq || snapshot.BasisRevision < acceptedBasis {
			return fmt.Errorf("reconciliation status is behind retained acceptance evidence")
		}
		summary = producerRecoverySummary{Challenge: ch, Manifest: manifest, Snapshot: snapshot}
		var oldFloor uint64
		if err = tx.QueryRow("SELECT submitted FROM recovery_producers WHERE id=?", ch.Producer.ID).Scan(&oldFloor); err != nil {
			return err
		}
		if manifest.CompactedThrough > oldFloor {
			summary.UnknownFirst, summary.UnknownLast = oldFloor+1, manifest.CompactedThrough
			if _, err = tx.Exec("INSERT INTO reconciliation_unknown VALUES(?,?,?,?)", ch.Producer.ID, summary.UnknownFirst, summary.UnknownLast, manifest.OutcomeCheckpoint); err != nil {
				return err
			}
		}
		if err = tx.QueryRow("SELECT count(*) FROM messages WHERE producer=? AND state='ambiguous'", ch.Producer.ID).Scan(&summary.Ambiguous); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE producers SET submitted=max(submitted,?) WHERE id=?", manifest.HighestIssued, ch.Producer.ID); err != nil {
			return err
		}
		body, _ := json.Marshal(summary)
		if err = r.capacity(tx, len(body)); err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE reconciliation_producers SET completion=? WHERE producer=?", string(body), ch.Producer.ID)
		return err
	})
	return
}

func (r *recoveryReconciler) Summary(ctx context.Context, p protocol.Producer) (summary producerRecoverySummary, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var body string
		if err := tx.QueryRow("SELECT completion FROM reconciliation_producers WHERE producer=?", p.ID).Scan(&body); err != nil {
			if err == sql.ErrNoRows {
				return protocol.ErrRecoveryIncomplete
			}
			return err
		}
		if body == "" {
			return protocol.ErrRecoveryIncomplete
		}
		return protocol.Decode([]byte(body), &summary, protocol.MaxRequest+8192)
	})
	return
}
