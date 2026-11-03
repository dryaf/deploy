package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type RecoveryLEDChallenge = protocol.RecoveryLEDChallenge
type RecoveryLEDReport = protocol.RecoveryLEDReport
type RecoveryLEDEvidence = protocol.RecoveryLEDEvidence
type RecoveryLEDEvidenceSession = protocol.RecoveryLEDEvidenceSession

type recoveryLEDChallengeRecord struct {
	Challenge RecoveryLEDChallenge
	Domains   recoveryLEDDomains
	Previous  []RecoveryLEDReport
}

func OpenRecoveryLEDEvidenceSession(ctx context.Context, path string, c Config) (RecoveryLEDEvidenceSession, error) {
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return nil, err
	}
	if err = r.ensureLEDEvidence(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func ledEvidenceFile(inventory string, p protocol.Producer, suffix string) string {
	return "led-evidence-" + protocol.Hash(struct {
		Inventory string
		Producer  protocol.Producer
	}{inventory, p}) + "-" + suffix + ".json"
}
func ledEvidenceSchema(ctx context.Context, q ledJournalReader) (bool, error) {
	var version string
	var tables int
	err := q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-evidence-version'").Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if e := q.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='reconciliation_led_evidence'").Scan(&tables); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && tables == 0 {
		return false, nil
	}
	if err != nil || version != "1" || tables != 1 {
		return false, fmt.Errorf("LED evidence schema missing or changed")
	}
	return true, nil
}
func (r *recoveryReconciler) ensureLEDEvidence(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		if err := r.checkLEDEvidenceHistory(ctx, tx); err != nil {
			return err
		}
		exists, err := ledEvidenceSchema(ctx, tx)
		if err != nil || exists {
			return err
		}
		if err = r.capacity(tx, 4096); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `CREATE TABLE reconciliation_led_evidence(inventory TEXT NOT NULL,producer TEXT NOT NULL,challenge TEXT NOT NULL,challenge_hash TEXT NOT NULL,reply TEXT NOT NULL DEFAULT '',reply_hash TEXT NOT NULL DEFAULT '',PRIMARY KEY(inventory,producer)); INSERT INTO metadata VALUES('led-evidence-version','1');`)
		return err
	})
}

type recoveryLEDEvidenceSlot struct {
	Intent             *recoveryLEDChallengeRecord
	ChallengeCommitted bool
	Reply              *RecoveryLEDEvidence
	ReplyCommitted     bool
}

// Missing SQL can only resume the exact independently durable file intent. A
// missing/mismatched file never gets reconstructed from an SQL row.
func (r *recoveryReconciler) readLEDEvidenceSlot(ctx context.Context, q ledJournalReader, v recoveryLEDInventory, p protocol.Producer, exists bool) (slot recoveryLEDEvidenceSlot, err error) {
	var intent recoveryLEDChallengeRecord
	fileErr := r.stage.ReadJSON(ledEvidenceFile(v.ID, p, "challenge"), &intent)
	var raw, hash, reply, replyHash string
	dbErr := sql.ErrNoRows
	if exists {
		dbErr = q.QueryRowContext(ctx, "SELECT challenge,challenge_hash,reply,reply_hash FROM reconciliation_led_evidence WHERE inventory=? AND producer=?", v.ID, p.ID).Scan(&raw, &hash, &reply, &replyHash)
	}
	if dbErr != nil && dbErr != sql.ErrNoRows {
		return slot, dbErr
	}
	var result RecoveryLEDEvidence
	resultErr := r.stage.ReadJSON(ledEvidenceFile(v.ID, p, "report"), &result)
	if os.IsNotExist(fileErr) && dbErr == sql.ErrNoRows && os.IsNotExist(resultErr) {
		return slot, nil
	}
	if !exists || fileErr != nil {
		return slot, fmt.Errorf("LED challenge lost its schema or file")
	}
	if err = r.validateLEDChallenge(intent, v, p); err != nil {
		return
	}
	slot.Intent = &intent
	if dbErr == nil {
		var saved recoveryLEDChallengeRecord
		if err = protocol.Decode([]byte(raw), &saved, 2<<20); err != nil {
			return
		}
		if protocol.Hash(saved) != hash || hash != protocol.Hash(intent) {
			return slot, fmt.Errorf("LED challenge journal mismatch")
		}
		slot.ChallengeCommitted = true
	}
	if os.IsNotExist(resultErr) {
		if reply != "" || replyHash != "" {
			return slot, fmt.Errorf("LED report lost its file")
		}
		return slot, nil
	}
	if resultErr != nil {
		return slot, resultErr
	}
	if !slot.ChallengeCommitted {
		return slot, fmt.Errorf("LED report lacks committed challenge")
	}
	if result.Challenge != intent.Challenge || result.Hash != protocol.Hash(result.Report) || result.ReceivedAt.Before(intent.Challenge.IssuedAt) || !result.ReceivedAt.Before(intent.Challenge.ExpiresAt) {
		return slot, fmt.Errorf("LED report identity mismatch")
	}
	if err = r.validateLEDReport(intent, result.Report, result.ReceivedAt); err != nil {
		return
	}
	slot.Reply = &result
	if reply != "" {
		var saved RecoveryLEDEvidence
		if err = protocol.Decode([]byte(reply), &saved, 2*protocol.MaxRequest); err != nil {
			return
		}
		if protocol.Hash(saved) != replyHash || replyHash != protocol.Hash(result) {
			return slot, fmt.Errorf("LED report journal mismatch")
		}
		slot.ReplyCommitted = true
	} else if replyHash != "" {
		return slot, fmt.Errorf("LED report hash without report")
	}
	return slot, nil
}

func (r *recoveryReconciler) checkLEDEvidenceHistory(ctx context.Context, q ledJournalReader) error {
	exists, err := ledEvidenceSchema(ctx, q)
	if err != nil {
		return err
	}
	set, err := r.readLEDInventorySet(ctx, q)
	if err != nil {
		return err
	}
	count := 0
	prior := map[string][]RecoveryLEDReport{}
	for _, v := range set.Records {
		for _, e := range r.config.Producers {
			slot, err := r.readLEDEvidenceSlot(ctx, q, v, e.Producer, exists)
			if err != nil {
				return err
			}
			if slot.Intent != nil && protocol.Hash(slot.Intent.Previous) != protocol.Hash(prior[e.Producer.ID]) {
				return fmt.Errorf("LED evidence lost earlier producer reports")
			}
			if slot.Reply != nil {
				prior[e.Producer.ID] = append(prior[e.Producer.ID], slot.Reply.Report)
			}
			if slot.ChallengeCommitted {
				count++
			}
		}
	}
	if exists {
		var rows int
		if err = q.QueryRowContext(ctx, "SELECT count(*) FROM reconciliation_led_evidence").Scan(&rows); err != nil {
			return err
		}
		if rows != count {
			return fmt.Errorf("LED evidence has unknown inventory or producer")
		}
	}
	return nil
}

func (r *recoveryReconciler) validateLEDChallenge(record recoveryLEDChallengeRecord, v recoveryLEDInventory, p protocol.Producer) error {
	c := record.Challenge
	if len(record.Previous) >= maxRecoveryLEDInventories || c.PreviousHash != protocol.Hash(record.Previous) {
		return fmt.Errorf("invalid previous LED evidence")
	}
	if c.Version != 1 || c.Operation != r.journal.Operation || c.Before != r.journal.Before || c.After != r.journal.After || c.Producer != p || c.InventoryID != v.ID || c.InventoryHash != protocol.Hash(v) || c.SourcesHash != protocol.Hash(v.Sources) || c.DomainHash != protocol.Hash(record.Domains) || c.ConfigHash != protocol.Hash(r.config) || !safefs.ValidID(c.Nonce, "n") || c.IssuedAt.Before(v.ObservedAt) || !c.IssuedAt.Before(v.ValidUntil) || !c.ExpiresAt.Equal(c.IssuedAt.Add(10*time.Minute)) || c.BootID != v.BootID || c.Uptime < v.Uptime || c.Uptime-v.Uptime >= 30 {
		return fmt.Errorf("invalid LED recovery challenge")
	}
	if _, err := r.config.enrollment(p); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > 2<<20 {
		return fmt.Errorf("LED challenge exceeds budget")
	}
	return nil
}
func (r *recoveryReconciler) currentLEDChallenge(c RecoveryLEDChallenge) error {
	boot, uptime, err := r.recoveryClock()
	if err != nil {
		return err
	}
	if boot != c.BootID || uptime < c.Uptime || uptime-c.Uptime >= 600 || r.now().Before(c.IssuedAt) || !r.now().Before(c.ExpiresAt) {
		return fmt.Errorf("LED challenge expired or clock changed; capture a new inventory")
	}
	return nil
}
func (r *recoveryReconciler) ledEvidenceHead(ctx context.Context, tx *sql.Tx, id string) (recoveryLEDInventory, error) {
	set, err := r.readLEDInventorySet(ctx, tx)
	if err != nil {
		return recoveryLEDInventory{}, err
	}
	if set.Pending || set.Head == nil || set.Head.ID != id {
		return recoveryLEDInventory{}, fmt.Errorf("LED evidence requires the current sealed inventory")
	}
	sources, err := r.collectLEDSources(ctx, tx)
	if err != nil {
		return recoveryLEDInventory{}, err
	}
	if protocol.Hash(sources) != protocol.Hash(set.Head.Sources) {
		return recoveryLEDInventory{}, fmt.Errorf("LED source changed since inventory")
	}
	return *set.Head, nil
}

func (r *recoveryReconciler) ChallengeLED(ctx context.Context, id string, p protocol.Producer) (out RecoveryLEDChallenge, err error) {
	if !safefs.ValidID(id, "n") {
		return out, fmt.Errorf("invalid LED inventory")
	}
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	if err = r.ensureLEDEvidence(ctx); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		v, err := r.ledEvidenceHead(ctx, tx, id)
		if err != nil {
			return err
		}
		slot, err := r.readLEDEvidenceSlot(ctx, tx, v, p, true)
		if err != nil {
			return err
		}
		if slot.ChallengeCommitted {
			out = slot.Intent.Challenge
			return nil
		}
		domains, err := r.collectLEDDomains(ctx, tx, p)
		if err != nil {
			return err
		}
		previous, err := r.earlierLEDReports(ctx, tx, v, p)
		if err != nil {
			return err
		}
		intent := recoveryLEDChallengeRecord{Domains: domains, Previous: previous}
		if slot.Intent != nil {
			intent = *slot.Intent
			if protocol.Hash(domains) != intent.Challenge.DomainHash || protocol.Hash(previous) != intent.Challenge.PreviousHash {
				return fmt.Errorf("LED domain changed during challenge persistence")
			}
		} else {
			if !r.ledInventoryView(v, v.Sources, nil).ObservationCurrent {
				return fmt.Errorf("capture a fresh LED inventory before challenging a producer")
			}
			boot, uptime, err := r.recoveryClock()
			if err != nil {
				return err
			}
			nonce, err := safefs.NewID("n")
			if err != nil {
				return err
			}
			now := r.now().UTC()
			intent.Challenge = RecoveryLEDChallenge{Version: 1, Operation: r.journal.Operation, Before: r.journal.Before, After: r.journal.After, Producer: p, InventoryID: v.ID, InventoryHash: protocol.Hash(v), SourcesHash: protocol.Hash(v.Sources), DomainHash: protocol.Hash(domains), ConfigHash: protocol.Hash(r.config), Nonce: nonce, IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute), BootID: boot, Uptime: uptime}
			intent.Challenge.PreviousHash = protocol.Hash(previous)
		}
		if err = r.validateLEDChallenge(intent, v, p); err != nil {
			return err
		}
		raw, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		if err = r.capacity(tx, len(raw)+4096); err != nil {
			return err
		}
		if slot.Intent == nil {
			if err = r.stage.WriteJSON(ledEvidenceFile(id, p, "challenge"), intent, false); err != nil {
				return err
			}
		}
		if err = recoveryBoundary(r.fault, "led_challenge_intent"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO reconciliation_led_evidence(inventory,producer,challenge,challenge_hash) VALUES(?,?,?,?)", id, p.ID, string(raw), protocol.Hash(intent)); err != nil {
			return err
		}
		out = intent.Challenge
		return recoveryBoundary(r.fault, "led_challenge_transaction")
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "led_challenge_committed")
	}
	return
}

func (r *recoveryReconciler) SubmitLED(ctx context.Context, ch RecoveryLEDChallenge, report RecoveryLEDReport) (out RecoveryLEDEvidence, err error) {
	if _, err = r.config.enrollment(ch.Producer); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		exists, err := ledEvidenceSchema(ctx, tx)
		if err != nil {
			return err
		}
		if !exists {
			return protocol.ErrRecoveryIncomplete
		}
		v, err := r.ledEvidenceHead(ctx, tx, ch.InventoryID)
		if err != nil {
			return err
		}
		slot, err := r.readLEDEvidenceSlot(ctx, tx, v, ch.Producer, true)
		if err != nil {
			return err
		}
		if !slot.ChallengeCommitted || slot.Intent.Challenge != ch {
			return fmt.Errorf("LED report challenge mismatch")
		}
		if slot.Reply != nil {
			if protocol.Hash(slot.Reply.Report) != protocol.Hash(report) {
				return fmt.Errorf("conflicting LED report replay")
			}
			out = *slot.Reply
			if slot.ReplyCommitted {
				return nil
			}
		} else {
			if err = r.currentLEDChallenge(ch); err != nil {
				return err
			}
			if err = r.validateLEDReport(*slot.Intent, report, r.now()); err != nil {
				return err
			}
			domains, err := r.collectLEDDomains(ctx, tx, ch.Producer)
			if err != nil {
				return err
			}
			if protocol.Hash(domains) != ch.DomainHash {
				return fmt.Errorf("LED domain changed since challenge")
			}
			out = RecoveryLEDEvidence{Challenge: ch, Report: report, ReceivedAt: r.now().UTC(), Hash: protocol.Hash(report)}
			// Source reads can outlive short evidence leases. Persist only an
			// observation valid at its final recorded admission time.
			if err = r.currentLEDChallenge(ch); err != nil {
				return err
			}
			if err = r.validateLEDReport(*slot.Intent, report, out.ReceivedAt); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if err = r.capacity(tx, len(raw)+4096); err != nil {
			return err
		}
		if slot.Reply == nil {
			if err = r.stage.WriteJSON(ledEvidenceFile(v.ID, ch.Producer, "report"), out, false); err != nil {
				return err
			}
		}
		if err = recoveryBoundary(r.fault, "led_report_intent"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE reconciliation_led_evidence SET reply=?,reply_hash=? WHERE inventory=? AND producer=?", string(raw), protocol.Hash(out), v.ID, ch.Producer.ID); err != nil {
			return err
		}
		return recoveryBoundary(r.fault, "led_report_transaction")
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "led_report_committed")
	}
	return
}

func (r *recoveryReconciler) LEDEvidence(ctx context.Context, id string, p protocol.Producer) (out RecoveryLEDEvidence, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		exists, err := ledEvidenceSchema(ctx, tx)
		if err != nil {
			return err
		}
		set, err := r.readLEDInventorySet(ctx, tx)
		if err != nil {
			return err
		}
		for _, v := range set.Records {
			if v.ID == id {
				slot, err := r.readLEDEvidenceSlot(ctx, tx, v, p, exists)
				if err != nil {
					return err
				}
				if !slot.ReplyCommitted {
					return protocol.ErrRecoveryIncomplete
				}
				out = *slot.Reply
				return nil
			}
		}
		return protocol.ErrRecoveryIncomplete
	})
	return
}

func (r *recoveryReconciler) earlierLEDReports(ctx context.Context, q ledJournalReader, v recoveryLEDInventory, p protocol.Producer) (reports []RecoveryLEDReport, err error) {
	set, err := r.readLEDInventorySet(ctx, q)
	if err != nil {
		return nil, err
	}
	for _, old := range set.Records {
		if old.Sequence >= v.Sequence {
			break
		}
		slot, err := r.readLEDEvidenceSlot(ctx, q, old, p, true)
		if err != nil {
			return nil, err
		}
		if slot.Reply != nil {
			reports = append(reports, slot.Reply.Report)
		}
	}
	return reports, nil
}

func ledEvidenceDomains(intent recoveryLEDChallengeRecord) []recoveryLEDDomain {
	domains := []recoveryLEDDomain{intent.Domains.Snapshot, intent.Domains.Latest}
	reports := append([]RecoveryLEDReport{}, intent.Previous...)
	if intent.Domains.Reconciled != nil {
		reports = append(reports, RecoveryLEDReport{Snapshot: *intent.Domains.Reconciled})
	}
	for _, report := range reports {
		status := report.Snapshot
		d := recoveryLEDDomain{Status: &status, Support: status.Support}
		for _, sub := range status.Subjects {
			latch := ""
			if sub.Condition == "critical" || sub.Condition == "warning" {
				latch = sub.Condition
				if sub.FaultRevision > 0 {
					d.Faults = append(d.Faults, recoveryLEDPolicyFault{sub.Key, sub.FaultRevision, sub.Condition, true})
				}
			}
			d.Subjects = append(d.Subjects, recoveryLEDSubject{sub, latch})
		}
		for _, a := range status.Support {
			d.Dependencies = append(d.Dependencies, recoveryLEDDependency{a.Subject, a.Decision, a.Revision})
		}
		domains = append(domains, d)
	}
	return domains
}
