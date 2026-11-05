package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const telegramHandoffLifetime = 10 * time.Minute
const telegramHandoffSkew = 5 * time.Second

type TelegramRecoveryCursor struct {
	Version                        int
	Drain                          TelegramDrainSummary
	RequestsHash                   string
	BeforeOffset, Offset           int64
	NotBefore                      time.Time
	HistoryComplete, IntakeEnabled bool
}

func (v TelegramRecoveryCursor) Validate() error {
	d := v.Drain
	if v.Version != 1 || !protocol.ValidToken(d.Operation) || !d.Identity.Valid() || !protocol.ValidToken(d.Account) || d.Pages == 0 || d.Pages > 4096 || d.NextOffset < 0 || d.ObservedAt.IsZero() || !protocol.ValidToken(d.BootID) || math.IsNaN(d.Uptime) || math.IsInf(d.Uptime, 0) || d.Uptime < 0 ||
		!safefs.DigestPattern.MatchString(d.HistoryHash) || !safefs.DigestPattern.MatchString(d.EvidenceDigest) || !safefs.DigestPattern.MatchString(v.RequestsHash) || d.ObservedEvents > 4096*16 || d.Mapped > d.ObservedEvents || d.Unmapped > d.ObservedEvents-d.Mapped || d.Rejected != d.ObservedEvents-d.Mapped-d.Unmapped || v.BeforeOffset < 0 || v.Offset != 0 || !v.NotBefore.Equal(d.ObservedAt.Add(telegramHandoffSkew)) || d.IntakeEnabled || d.HistoryComplete || v.IntakeEnabled || v.HistoryComplete {
		return fmt.Errorf("invalid Telegram recovery cursor")
	}
	return nil
}
func (v TelegramRecoveryCursor) fresh(now time.Time, boot string, up float64) error {
	if err := v.Validate(); err != nil {
		return err
	}
	elapsed := now.Sub(v.Drain.ObservedAt)
	mono := up - v.Drain.Uptime
	// Compare the absolute monotonic deadline. Subtracting fractional uptimes
	// can round a full ten minutes down and accept an already expired handoff.
	if boot != v.Drain.BootID || math.IsNaN(up) || math.IsInf(up, 0) || elapsed < 0 || elapsed >= telegramHandoffLifetime || mono < 0 || up >= v.Drain.Uptime+telegramHandoffLifetime.Seconds() || math.Abs(elapsed.Seconds()-mono) > telegramHandoffSkew.Seconds() {
		return fmt.Errorf("Telegram recovery observation expired or clock changed")
	}
	return nil
}

type TelegramRecoveryCursorSession interface {
	CommitTelegramRecoveryCursor(context.Context, string) (TelegramRecoveryCursor, error)
	ValidateTelegramRecoveryHandoff(context.Context, string) (TelegramRecoveryCursor, error)
	Close() error
}

func OpenTelegramRecoveryCursorSession(ctx context.Context, path string, c Config) (TelegramRecoveryCursorSession, error) {
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	if err != nil {
		return nil, err
	}
	if err = r.initializeTelegramCursorRecovery(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func telegramCursorFile(account string) string {
	return "telegram-cursor-" + protocol.Hash(account) + ".json"
}
func telegramCursorSchema(tx *sql.Tx) (bool, error) {
	var version string
	err := tx.QueryRow("SELECT value FROM metadata WHERE key='telegram_cursor_recovery_schema'").Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	var n int
	if e := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='reconciliation_telegram_cursors'").Scan(&n); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && n == 0 {
		return false, nil
	}
	if err != nil || version != "1" || n != 1 {
		return false, fmt.Errorf("Telegram cursor recovery schema missing or changed")
	}
	return true, nil
}
func (r *recoveryReconciler) initializeTelegramCursorRecovery(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		exists, err := telegramCursorSchema(tx)
		if err != nil || exists {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE reconciliation_telegram_cursors(account TEXT PRIMARY KEY,hash TEXT NOT NULL,body TEXT NOT NULL);
INSERT INTO metadata VALUES('telegram_cursor_recovery_schema','1');`)
		return err
	})
}

// Commit changes only the offline candidate. Zero deliberately avoids inheriting
// a positive acknowledgement offset across an interruption or native-ID reset.
// The empty-drain observation is not proof of complete provider history.
func (r *recoveryReconciler) CommitTelegramRecoveryCursor(ctx context.Context, account string) (proof TelegramRecoveryCursor, err error) {
	state, err := r.openTelegramDrain(ctx, account)
	if err != nil {
		return proof, err
	}
	if state.Summary == nil {
		return proof, protocol.ErrRecoveryIncomplete
	}
	applying := false
	err = r.transactionWithValidation(ctx, func(tx *sql.Tx) error {
		exists, err := telegramCursorSchema(tx)
		if err != nil {
			return err
		}
		if !exists {
			return protocol.ErrRecoveryIncomplete
		}
		proof, err = r.prepareTelegramCursor(tx, state)
		if err != nil {
			return err
		}
		var hash, body string
		savedErr := tx.QueryRow("SELECT hash,body FROM reconciliation_telegram_cursors WHERE account=?", account).Scan(&hash, &body)
		if savedErr != nil && savedErr != sql.ErrNoRows {
			return savedErr
		}
		var offset int64
		offsetErr := tx.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", account).Scan(&offset)
		if offsetErr != nil && offsetErr != sql.ErrNoRows {
			return offsetErr
		}
		var intent TelegramRecoveryCursor
		fileErr := r.stage.ReadJSON(telegramCursorFile(account), &intent)
		if savedErr == nil {
			if fileErr != nil {
				return fmt.Errorf("Telegram cursor commit lost intent: %w", fileErr)
			}
			var saved TelegramRecoveryCursor
			if err = protocol.Decode([]byte(body), &saved, 16384); err != nil {
				return err
			}
			if hash != protocol.Hash(proof) || protocol.Hash(saved) != hash || protocol.Hash(intent) != hash || offsetErr != nil || offset != proof.Offset {
				return fmt.Errorf("Telegram cursor commit regressed")
			}
			return nil
		}
		applying = true
		if offset != proof.BeforeOffset {
			return fmt.Errorf("Telegram cursor changed before commit")
		}
		boot, up, err := r.recoveryClock()
		if err != nil {
			return err
		}
		if err = proof.fresh(r.now(), boot, up); err != nil {
			return err
		}
		if err = r.capacity(tx, 32768); err != nil {
			return err
		}
		if os.IsNotExist(fileErr) {
			if err = r.stage.WriteJSON(telegramCursorFile(account), proof, false); err != nil {
				return err
			}
		} else if fileErr != nil {
			return fileErr
		} else if protocol.Hash(intent) != protocol.Hash(proof) {
			return fmt.Errorf("Telegram cursor intent changed")
		}
		if err = recoveryBoundary(r.fault, "telegram_cursor_intent"); err != nil {
			return err
		}
		if err = r.checkAuthority(); err != nil {
			return err
		}
		raw, err := json.Marshal(proof)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO telegram_cursors VALUES(?,?) ON CONFLICT(account) DO UPDATE SET offset=excluded.offset", account, proof.Offset); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO reconciliation_telegram_cursors VALUES(?,?,?)", account, protocol.Hash(proof), string(raw)); err != nil {
			return err
		}
		if err = recoveryBoundary(r.fault, "telegram_cursor_transaction"); err != nil {
			return err
		}
		return nil
	}, func() error {
		if !applying {
			return nil
		}
		boot, up, err := r.recoveryClock()
		if err != nil {
			return err
		}
		return proof.fresh(r.now(), boot, up)
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "telegram_cursor_commit")
	}
	return
}
func (r *recoveryReconciler) prepareTelegramCursor(tx *sql.Tx, want telegramDrainState) (proof TelegramRecoveryCursor, err error) {
	state, err := readTelegramDrain(tx, want.Account)
	if err != nil {
		return
	}
	verified, _, err := r.verifyTelegramDrain(tx, state, state.Pages)
	if err != nil {
		return
	}
	var pin telegramDrainState
	if err = r.stage.ReadJSON(telegramDrainFile(want.Account), &pin); err != nil {
		return
	}
	if state.Summary == nil || protocol.Hash(state) != protocol.Hash(want) || protocol.Hash(verified) != protocol.Hash(state) || protocol.Hash(pin) != protocol.Hash(state) || state.Binding != r.journal.binding() {
		return proof, fmt.Errorf("Telegram drain checkpoint changed")
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
	history, err := readTelegramSession(tx, want.Account)
	if err != nil {
		return
	}
	if history.Summary == nil || protocol.Hash(history.Summary) != state.HistoryHash {
		return proof, fmt.Errorf("Telegram history summary changed")
	}
	proof = TelegramRecoveryCursor{Version: 1, Drain: *state.Summary, RequestsHash: protocol.Hash(requests), BeforeOffset: history.RestoredOffset, NotBefore: state.Summary.ObservedAt.Add(telegramHandoffSkew)}
	return proof, proof.Validate()
}

// A lifecycle caller must check again immediately before activation while all
// other receivers remain stopped. This checks evidence and clocks, not shutdown.
func (r *recoveryReconciler) ValidateTelegramRecoveryHandoff(ctx context.Context, account string) (proof TelegramRecoveryCursor, err error) {
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var raw string
		return tx.QueryRow("SELECT body FROM reconciliation_telegram_cursors WHERE account=?", account).Scan(&raw)
	})
	if err != nil {
		return
	}
	proof, err = r.CommitTelegramRecoveryCursor(ctx, account)
	if err != nil {
		return
	}
	err = r.transactionWithValidation(ctx, func(*sql.Tx) error { return nil }, func() error {
		boot, up, err := r.recoveryClock()
		if err != nil {
			return err
		}
		if err = proof.fresh(r.now(), boot, up); err != nil {
			return err
		}
		if !r.now().After(proof.NotBefore) {
			return fmt.Errorf("Telegram recovery cutoff has not passed")
		}
		return nil
	})
	return
}
