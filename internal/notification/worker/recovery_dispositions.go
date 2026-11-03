package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// RecoveryDisposition is a local operator decision about an ambiguous delivery,
// never a chat command, command-execution grant or provider-send permission.
// The eventual authenticated lifecycle caller supplies Actor and explicit consent.
// Request IDs are scoped to the exact producer/message, as is idempotent replay.
type RecoveryDisposition struct {
	ID, Operation                       string
	Identity                            protocol.Identity
	Producer                            protocol.Producer
	MessageID, MessageHash, SummaryHash string
	Action, Actor, Reason               string
	AcceptDuplicateRisk                 bool
}
type RecoveryDispositionRecord struct {
	Version         int
	Decision        RecoveryDisposition
	RecordedAt      time.Time
	DeliveryEnabled bool
}
type RecoveryDispositionSession interface {
	ReviewRecoveryMessage(context.Context, protocol.Producer, string) (RecoveryMessageReview, error)
	DecideRecoveryMessage(context.Context, RecoveryDisposition) (RecoveryDispositionRecord, error)
	RecoveryMessageDisposition(context.Context, protocol.Producer, string) (RecoveryDispositionRecord, error)
	Close() error
}

func OpenRecoveryDispositionSession(ctx context.Context, path string, c Config) (RecoveryDispositionSession, error) {
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return nil, err
	}
	if err = r.initializeDispositions(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func (r *recoveryReconciler) initializeDispositions(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		var version string
		err := tx.QueryRow("SELECT value FROM metadata WHERE key='recovery_dispositions_schema'").Scan(&version)
		if err == nil {
			if version != "1" {
				return fmt.Errorf("unknown recovery disposition schema")
			}
			var n int
			return tx.QueryRow("SELECT count(*) FROM reconciliation_dispositions").Scan(&n)
		}
		if err != sql.ErrNoRows {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE reconciliation_dispositions(
producer TEXT NOT NULL,message TEXT NOT NULL,id TEXT NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,
PRIMARY KEY(producer,message));
INSERT INTO metadata VALUES('recovery_dispositions_schema','1');`)
		return err
	})
}
func dispositionFile(p protocol.Producer, id string) string {
	return "disposition-" + protocol.Hash(struct {
		Producer protocol.Producer
		Message  string
	}{p, id}) + ".json"
}
func (d RecoveryDisposition) Validate() error {
	if !safefs.ValidID(d.ID, "n") || !safefs.ValidID(d.Operation, "n") || !d.Identity.Valid() || !d.Producer.Valid() ||
		!protocol.ValidToken(d.MessageID) || !safefs.DigestPattern.MatchString(d.MessageHash) ||
		!safefs.DigestPattern.MatchString(d.SummaryHash) || !protocol.ValidToken(d.Actor) ||
		len(d.Reason) > 1024 || !utf8.ValidString(d.Reason) || strings.TrimSpace(d.Reason) == "" || strings.ContainsRune(d.Reason, 0) ||
		(d.Action != "retry" && d.Action != "cancel") || d.AcceptDuplicateRisk != (d.Action == "retry") {
		return fmt.Errorf("invalid recovery disposition; retry requires explicit duplicate-delivery consent")
	}
	return nil
}
func (r *recoveryReconciler) validateDisposition(tx *sql.Tx, d RecoveryDisposition) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if d.Operation != r.journal.Operation || d.Identity != r.journal.After {
		return fmt.Errorf("recovery disposition epoch mismatch")
	}
	if _, err := r.config.enrollment(d.Producer); err != nil {
		return err
	}
	var raw, challenge, manifest string
	if err := tx.QueryRow("SELECT completion,challenge,manifest FROM reconciliation_producers WHERE producer=?", d.Producer.ID).Scan(&raw, &challenge, &manifest); err != nil {
		return err
	}
	if raw == "" {
		return fmt.Errorf("complete producer reconciliation before deciding delivery")
	}
	var summary protocol.RecoverySummary
	if err := protocol.Decode([]byte(raw), &summary, protocol.MaxRequest+8192); err != nil {
		return err
	}
	var ch protocol.RecoveryChallenge
	var m protocol.RecoveryManifest
	if err := protocol.Decode([]byte(challenge), &ch, 4096); err != nil {
		return err
	}
	if err := protocol.Decode([]byte(manifest), &m, 4096); err != nil {
		return err
	}
	if protocol.Hash(summary) != d.SummaryHash || protocol.Hash(summary.Challenge) != protocol.Hash(ch) ||
		summary.Manifest != m || ch.Operation != d.Operation || ch.Identity != d.Identity ||
		ch.Producer != d.Producer || summary.DeliveryEnabled {
		return fmt.Errorf("recovery disposition summary changed")
	}
	var producer, hash, body, state, generation, attempt string
	var sequence uint64
	if err := tx.QueryRow("SELECT producer,hash,body,state,generation,attempt,seq FROM messages WHERE producer=? AND id=?", d.Producer.ID, d.MessageID).Scan(&producer, &hash, &body, &state, &generation, &attempt, &sequence); err != nil {
		return err
	}
	var msg protocol.Message
	if err := protocol.Decode([]byte(body), &msg, protocol.MaxRequest); err != nil {
		return err
	}
	if err := msg.Validate(); err != nil {
		return err
	}
	if producer != d.Producer.ID || msg.Producer != d.Producer || msg.ID != d.MessageID ||
		hash != d.MessageHash || protocol.Hash(msg) != hash || sequence != msg.Sequence || attempt != "" {
		return fmt.Errorf("disposition requires the exact ambiguous message")
	}
	var evidenceHash string
	if err := tx.QueryRow("SELECT body,evidence_hash FROM reconciliation_records WHERE producer=? AND id=?", d.Producer.ID, d.MessageID).Scan(&body, &evidenceHash); err != nil {
		return err
	}
	var evidence protocol.RecoveryRecord
	if err := protocol.Decode([]byte(body), &evidence, protocol.MaxRequest+8192); err != nil {
		return err
	}
	if protocol.Hash(evidence) != evidenceHash || protocol.Hash(evidence.Message) != d.MessageHash || evidence.Receipt != nil {
		return fmt.Errorf("delivery disposition conflicts with retained payload or terminal evidence")
	}
	if err := r.validateDispositionGeneration(tx, d, msg.Generation, generation); err != nil {
		return err
	}
	return r.validateDispositionState(tx, d, state)
}
func readDisposition(tx *sql.Tx, p protocol.Producer, id string) (record RecoveryDispositionRecord, err error) {
	var raw, hash, requestID string
	if err = tx.QueryRow("SELECT id,hash,body FROM reconciliation_dispositions WHERE producer=? AND message=?", p.ID, id).Scan(&requestID, &hash, &raw); err != nil {
		return
	}
	if err = protocol.Decode([]byte(raw), &record, 8192); err != nil {
		return
	}
	if record.Version != 1 || record.RecordedAt.IsZero() || record.DeliveryEnabled ||
		record.Decision.Producer != p || record.Decision.MessageID != id || record.Decision.ID != requestID || protocol.Hash(record) != hash {
		return record, fmt.Errorf("recovery disposition record changed")
	}
	err = record.Decision.Validate()
	return
}

func (r *recoveryReconciler) DecideRecoveryMessage(ctx context.Context, d RecoveryDisposition) (record RecoveryDispositionRecord, err error) {
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		if err := r.validateDisposition(tx, d); err != nil {
			return err
		}
		saved, dbErr := readDisposition(tx, d.Producer, d.MessageID)
		if dbErr != nil && dbErr != sql.ErrNoRows {
			return dbErr
		}
		name := dispositionFile(d.Producer, d.MessageID)
		var intent RecoveryDispositionRecord
		fileErr := r.stage.ReadJSON(name, &intent)
		if fileErr != nil && !os.IsNotExist(fileErr) {
			return fileErr
		}
		if dbErr == nil {
			if fileErr != nil || protocol.Hash(saved) != protocol.Hash(intent) || saved.Decision != d {
				return fmt.Errorf("recovery disposition lost its journal or changed")
			}
			record = saved
			return nil
		}
		if fileErr == nil {
			if intent.Version != 1 || intent.RecordedAt.IsZero() || intent.DeliveryEnabled || intent.Decision != d {
				return fmt.Errorf("pending recovery disposition differs")
			}
		} else {
			intent = RecoveryDispositionRecord{Version: 1, Decision: d, RecordedAt: r.now().UTC()}
			if intent.RecordedAt.IsZero() {
				return fmt.Errorf("recovery decision time unavailable")
			}
		}
		raw, err := json.Marshal(intent)
		if err != nil {
			return err
		}
		if err = r.capacity(tx, len(raw)); err != nil {
			return err
		}
		if os.IsNotExist(fileErr) {
			if err = r.stage.WriteJSON(name, intent, false); err != nil {
				return err
			}
		}
		if err = recoveryBoundary(r.fault, "disposition_intent"); err != nil {
			return err
		}
		// Keep the original ambiguous state, receipt stream and monitoring
		// evidence unchanged. Activation must consume this decision explicitly.
		if _, err = tx.Exec("INSERT INTO reconciliation_dispositions VALUES(?,?,?,?,?)", d.Producer.ID, d.MessageID, d.ID, protocol.Hash(intent), string(raw)); err != nil {
			return err
		}
		record = intent
		return recoveryBoundary(r.fault, "disposition_transaction")
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "disposition_commit")
	}
	return
}
func (r *recoveryReconciler) RecoveryMessageDisposition(ctx context.Context, p protocol.Producer, id string) (record RecoveryDispositionRecord, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	if !protocol.ValidToken(id) {
		return record, fmt.Errorf("invalid message identity")
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var state string
		var err error
		record, state, err = r.dispositionEvidence(tx, p, id)
		if err != nil {
			return err
		}
		if state != "recorded" {
			return protocol.ErrRecoveryIncomplete
		}
		return nil
	})
	return
}
