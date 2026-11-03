package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func OpenSignalHandoffSession(ctx context.Context, path string, c Config) (interaction.SignalHandoffSession, error) {
	session, err := OpenSignalRecoverySession(ctx, path, c)
	if err != nil {
		return nil, err
	}
	r := session.(*recoveryReconciler)
	if err = r.initializeSignalHandoff(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func signalHandoffSchema(tx *sql.Tx) (bool, error) {
	var version string
	err := tx.QueryRow("SELECT value FROM metadata WHERE key='signal_handoff_schema'").Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	var n int
	if e := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='reconciliation_signal_handoffs'").Scan(&n); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && n == 0 {
		return false, nil
	}
	if err != nil || version != "1" || n != 1 {
		return false, fmt.Errorf("Signal handoff schema missing or changed")
	}
	return true, nil
}
func (r *recoveryReconciler) initializeSignalHandoff(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		exists, err := signalHandoffSchema(tx)
		if err != nil || exists {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE reconciliation_signal_handoffs(account TEXT PRIMARY KEY,hash TEXT NOT NULL,body TEXT NOT NULL);
INSERT INTO metadata VALUES('signal_handoff_schema','1');`)
		return err
	})
}
func signalHandoffFile(account string) string {
	return "signal-handoff-" + protocol.Hash(account) + ".json"
}
func (r *recoveryReconciler) PrepareSignalHandoff(ctx context.Context, account string) (proof interaction.SignalRecoveryHandoff, err error) {
	if err = r.signalRecoveryEnrollment(account); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error { var e error; proof, e = r.prepareSignalHandoff(tx, account); return e })
	return
}
func (r *recoveryReconciler) prepareSignalHandoff(tx *sql.Tx, account string) (proof interaction.SignalRecoveryHandoff, err error) {
	return r.prepareSignalHandoffAt(tx, account, nil)
}

// The cursor installer validates the actual cursor separately and retains its original preimage.
func (r *recoveryReconciler) prepareSignalHandoffAt(tx *sql.Tx, account string, before *interaction.SpoolCursor) (proof interaction.SignalRecoveryHandoff, err error) {
	exists, err := signalHandoffSchema(tx)
	if err != nil {
		return
	}
	if !exists {
		return proof, protocol.ErrRecoveryIncomplete
	}
	s, err := readSignalRecovery(tx, account)
	if err != nil {
		return
	}
	if err = r.validateSignalSummary(s); err != nil {
		return
	}
	if err = verifySignalHandoffRecords(tx, s); err != nil {
		return
	}
	if ready, e := requestRecoveryAdmissionReady(tx); e != nil {
		return proof, e
	} else if !ready {
		return proof, protocol.ErrRecoveryIncomplete
	}
	var raw string
	if err = tx.QueryRow("SELECT body FROM reconciliation_request_seal WHERE singleton=1").Scan(&raw); err != nil {
		return
	}
	var requests RecoveryRequestSummary
	if err = protocol.Decode([]byte(raw), &requests, 8192); err != nil {
		return
	}
	proof = interaction.SignalRecoveryHandoff{Version: 1, Summary: *s.Summary, RequestsHash: protocol.Hash(requests), Before: s.Manifest.Relay, Target: s.Manifest.Relay}
	if s.Manifest.Restore.Operation != "" {
		proof.Target.Identity = s.Manifest.Restore.After
	}
	proof.Target.Position = max(s.Manifest.Highest, s.GatewayHighest)
	err = tx.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", account).Scan(&raw)
	if err == nil {
		if err = protocol.Decode([]byte(raw), &proof.Before, 4096); err != nil {
			return
		}
	} else if err != sql.ErrNoRows {
		return
	}
	if before != nil {
		proof.Before = *before
	}
	err = proof.Validate()
	return
}
func verifySignalHandoffRecords(tx *sql.Tx, s signalRecoverySession) error {
	rows, err := tx.Query("SELECT position,key,hash,body,request_key,command_id,state FROM reconciliation_signal_events WHERE account=? ORDER BY position", s.Manifest.Relay.Account)
	if err != nil {
		return err
	}
	defer rows.Close()
	var hashes []string
	var mapped, unmapped, rejected uint64
	for rows.Next() {
		var position uint64
		var key, hash, raw, request, id, state string
		if err = rows.Scan(&position, &key, &hash, &raw, &request, &id, &state); err != nil {
			return err
		}
		var event interaction.SpoolEvent
		if err = protocol.Decode([]byte(raw), &event, 16384); err != nil {
			return err
		}
		if err = interaction.ValidateSignalRecoveryEvent(event, s.Manifest.Relay); err != nil {
			return err
		}
		if position != s.Manifest.PositionAt(uint64(len(hashes))) || position > s.Manifest.Highest || event.Position != position || key != event.Key || hash != protocol.Hash(event) {
			return fmt.Errorf("Signal handoff retained event changed")
		}
		if event.Inbound == nil {
			if request != "" || id != "" || state != "rejected" {
				return fmt.Errorf("Signal rejection mapping changed")
			}
		} else {
			if request != event.Inbound.RequestKey() {
				return fmt.Errorf("Signal handoff request identity changed")
			}
			if id != "" {
				m, e := readRetainedRequest(tx, *event.Inbound)
				if e != nil {
					return e
				}
				if m == nil || m.ID != id {
					return fmt.Errorf("Signal handoff original mapping missing")
				}
				expected := "mapped"
				if m.Query != nil && m.Query.Kind == "rejected" {
					expected = "rejected"
				}
				if state != expected {
					return fmt.Errorf("Signal handoff mapping disposition changed")
				}
			} else if state != "unmapped" {
				return fmt.Errorf("Signal handoff unmapped disposition changed")
			}
		}
		var inboxHash, inboxState string
		if err = tx.QueryRow("SELECT hash,state FROM provider_inbox WHERE account=? AND id=?", s.Manifest.Relay.Account, position).Scan(&inboxHash, &inboxState); err != nil {
			return err
		}
		if inboxHash != event.Hash || inboxState != "recovery_quarantined" {
			return fmt.Errorf("Signal handoff inbox evidence changed")
		}
		switch state {
		case "mapped":
			if id == "" {
				return fmt.Errorf("Signal mapped record lacks identity")
			}
			mapped++
		case "unmapped":
			if id != "" {
				return fmt.Errorf("Signal unmapped record acquired identity")
			}
			unmapped++
		case "rejected":
			rejected++
		default:
			return fmt.Errorf("Signal handoff disposition invalid")
		}
		hashes = append(hashes, hash)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if uint64(len(hashes)) != s.Manifest.EventCount() || interaction.RecoveryEvidenceDigest(hashes) != s.Manifest.Digest || mapped != s.Summary.Mapped || unmapped != s.Summary.Unmapped || rejected != s.Summary.Rejected {
		return fmt.Errorf("Signal handoff evidence differs from completion")
	}
	return nil
}
func (r *recoveryReconciler) RecordSignalHandoff(ctx context.Context, proof interaction.SignalRecoveryHandoff) error {
	if err := proof.Validate(); err != nil {
		return err
	}
	account := proof.Target.Account
	if err := r.signalRecoveryEnrollment(account); err != nil {
		return err
	}
	err := r.transactionWithValidation(ctx, func(tx *sql.Tx) error {
		current, err := r.prepareSignalHandoff(tx, account)
		if err != nil {
			return err
		}
		if protocol.Hash(current) != protocol.Hash(proof) {
			return fmt.Errorf("Signal handoff preparation changed")
		}
		var hash, raw string
		savedErr := tx.QueryRow("SELECT hash,body FROM reconciliation_signal_handoffs WHERE account=?", account).Scan(&hash, &raw)
		if savedErr != nil && savedErr != sql.ErrNoRows {
			return savedErr
		}
		var intent interaction.SignalRecoveryHandoff
		fileErr := r.stage.ReadJSON(signalHandoffFile(account), &intent)
		if savedErr == nil {
			if fileErr != nil {
				return fmt.Errorf("Signal handoff intent missing: %w", fileErr)
			}
			var saved interaction.SignalRecoveryHandoff
			if err = protocol.Decode([]byte(raw), &saved, 16384); err != nil {
				return err
			}
			if hash != protocol.Hash(proof) || protocol.Hash(saved) != hash || protocol.Hash(intent) != hash {
				return fmt.Errorf("Signal handoff agreement changed")
			}
			return nil
		}
		if err = r.capacity(tx, 32768); err != nil {
			return err
		}
		if os.IsNotExist(fileErr) {
			if err = r.stage.WriteJSON(signalHandoffFile(account), proof, false); err != nil {
				return err
			}
		} else if fileErr != nil {
			return fileErr
		} else if protocol.Hash(intent) != protocol.Hash(proof) {
			return fmt.Errorf("Signal handoff intent changed")
		}
		if err = recoveryBoundary(r.fault, "signal_handoff_intent"); err != nil {
			return err
		}
		body, err := json.Marshal(proof)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO reconciliation_signal_handoffs VALUES(?,?,?)", account, protocol.Hash(proof), string(body)); err != nil {
			return err
		}
		return recoveryBoundary(r.fault, "signal_handoff_transaction")
	}, func() error {
		var intent interaction.SignalRecoveryHandoff
		if err := r.stage.ReadJSON(signalHandoffFile(account), &intent); err != nil {
			return err
		}
		if protocol.Hash(intent) != protocol.Hash(proof) {
			return fmt.Errorf("Signal handoff intent changed before commit")
		}
		return nil
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "signal_handoff_commit")
	}
	return err
}
