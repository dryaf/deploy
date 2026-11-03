package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// RecoveryMessageReview is a reviewable request basis, not delivery authority.
// Reusing its RequestID makes a lost CLI response retryable with the same choice.
type RecoveryMessageReview struct {
	Version                  int
	RequestID, Operation     string
	Identity                 protocol.Identity
	Message                  protocol.Message
	MessageHash, SummaryHash string
	DecisionState            string // none, intent, recorded
	Decision                 *RecoveryDispositionRecord
	DeliveryEnabled          bool
	ApplicationState         string                      `json:"ApplicationState,omitempty"`
	Application              *RecoveryAppliedDisposition `json:"Application,omitempty"`
	RetryBindingState        string                      `json:"RetryBindingState,omitempty"`
	RetryBinding             *RecoveryRetryBinding       `json:"RetryBinding,omitempty"`
}

func (v RecoveryMessageReview) Validate() error {
	if v.Version != 1 || v.DeliveryEnabled || !safefs.ValidID(v.RequestID, "n") ||
		!safefs.ValidID(v.Operation, "n") || !v.Identity.Valid() ||
		!safefs.DigestPattern.MatchString(v.SummaryHash) || protocol.Hash(v.Message) != v.MessageHash {
		return fmt.Errorf("invalid recovery review")
	}
	if err := v.Message.Validate(); err != nil {
		return err
	}
	switch v.DecisionState {
	case "none":
		if v.Decision != nil {
			return fmt.Errorf("unexpected recovery decision")
		}
	case "intent", "recorded":
		if v.Decision == nil {
			return fmt.Errorf("missing recovery decision")
		}
		r, d := v.Decision, v.Decision.Decision
		if r.Version != 1 || r.RecordedAt.IsZero() || r.DeliveryEnabled ||
			d.ID != v.RequestID || d.Operation != v.Operation || d.Identity != v.Identity ||
			d.Producer != v.Message.Producer || d.MessageID != v.Message.ID ||
			d.MessageHash != v.MessageHash || d.SummaryHash != v.SummaryHash {
			return fmt.Errorf("review decision differs from message")
		}
		if err := d.Validate(); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown recovery decision state")
	}
	switch v.ApplicationState {
	case "", "none":
		if v.Application != nil {
			return fmt.Errorf("unexpected recovery application")
		}
	case "intent", "applied":
		if v.Application == nil || v.Decision == nil || v.DecisionState != "recorded" {
			return fmt.Errorf("application has no recorded decision")
		}
		a := v.Application
		if err := a.Application.Validate(); err != nil {
			return err
		}
		if protocol.Hash(a.Application.Decision) != protocol.Hash(v.Decision) ||
			(v.ApplicationState == "intent" && a.ReceiptPosition != 0) || (v.ApplicationState == "applied" && a.ReceiptPosition <= 0) {
			return fmt.Errorf("review application differs from decision")
		}
	default:
		return fmt.Errorf("unknown recovery application state")
	}
	switch v.RetryBindingState {
	case "", "none":
		if v.RetryBinding != nil {
			return fmt.Errorf("unexpected retry binding")
		}
	case "intent", "bound":
		if v.RetryBinding == nil || v.Application == nil || v.ApplicationState != "applied" {
			return fmt.Errorf("retry binding has no applied decision")
		}
		if err := v.RetryBinding.Validate(); err != nil {
			return err
		}
		if protocol.Hash(v.RetryBinding.Applied) != protocol.Hash(v.Application) || v.RetryBinding.OriginalGeneration != v.Message.Generation {
			return fmt.Errorf("review retry binding changed")
		}
	default:
		return fmt.Errorf("unknown retry binding state")
	}
	return nil
}

func (r *recoveryReconciler) dispositionEvidence(tx *sql.Tx, p protocol.Producer, id string) (record RecoveryDispositionRecord, state string, err error) {
	saved, dbErr := readDisposition(tx, p, id)
	var intent RecoveryDispositionRecord
	fileErr := r.stage.ReadJSON(dispositionFile(p, id), &intent)
	if dbErr == sql.ErrNoRows && os.IsNotExist(fileErr) {
		return record, "none", nil
	}
	if dbErr != nil && dbErr != sql.ErrNoRows {
		return record, "", dbErr
	}
	if fileErr != nil {
		return record, "", fileErr
	}
	if intent.Version != 1 || intent.RecordedAt.IsZero() || intent.DeliveryEnabled ||
		intent.Decision.Producer != p || intent.Decision.MessageID != id {
		return record, "", fmt.Errorf("recovery disposition intent changed")
	}
	if err = r.validateDisposition(tx, intent.Decision); err != nil {
		return
	}
	if dbErr == sql.ErrNoRows {
		return intent, "intent", nil
	}
	if protocol.Hash(saved) != protocol.Hash(intent) {
		return record, "", fmt.Errorf("recovery disposition journal changed")
	}
	return saved, "recorded", nil
}

func (r *recoveryReconciler) ReviewRecoveryMessage(ctx context.Context, p protocol.Producer, id string) (v RecoveryMessageReview, err error) {
	if _, err = r.config.enrollment(p); err != nil {
		return
	}
	if !protocol.ValidToken(id) {
		return v, fmt.Errorf("invalid message identity")
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var raw string
		if err := tx.QueryRow("SELECT body FROM messages WHERE producer=? AND id=?", p.ID, id).Scan(&raw); err != nil {
			return err
		}
		if err := protocol.Decode([]byte(raw), &v.Message, protocol.MaxRequest); err != nil {
			return err
		}
		if err := tx.QueryRow("SELECT completion FROM reconciliation_producers WHERE producer=?", p.ID).Scan(&raw); err != nil {
			return err
		}
		var summary protocol.RecoverySummary
		if err := protocol.Decode([]byte(raw), &summary, protocol.MaxRequest+8192); err != nil {
			return err
		}
		v.Version, v.Operation, v.Identity = 1, r.journal.Operation, r.journal.After
		v.MessageHash, v.SummaryHash = protocol.Hash(v.Message), protocol.Hash(summary)
		var err error
		record, state, err := r.dispositionEvidence(tx, p, id)
		if err != nil {
			return err
		}
		v.DecisionState = state
		if state == "none" {
			v.RequestID, err = safefs.NewID("n")
			if err != nil {
				return err
			}
		} else {
			v.RequestID, v.Decision = record.Decision.ID, &record
		}
		// Share the same evidence checks as admission. No choice is recorded by
		// this synthetic validation request.
		d := RecoveryDisposition{ID: v.RequestID, Operation: v.Operation, Identity: v.Identity, Producer: p,
			MessageID: id, MessageHash: v.MessageHash, SummaryHash: v.SummaryHash,
			Action: "cancel", Actor: "review", Reason: "Review evidence only"}
		if state != "none" {
			d = record.Decision
		}
		if err = r.validateDisposition(tx, d); err != nil {
			return err
		}
		application, phase, err := r.applicationEvidence(tx, p, id)
		if err != nil {
			return err
		}
		if phase != "none" {
			v.ApplicationState, v.Application = phase, &application
		}
		binding, phase, err := r.retryBindingEvidence(tx, p, id)
		if err != nil {
			return err
		}
		if phase != "none" {
			v.RetryBindingState, v.RetryBinding = phase, &binding
		}
		return v.Validate()
	})
	return
}
