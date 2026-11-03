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

func OpenSignalCursorSession(ctx context.Context, path string, c Config) (interaction.SignalCursorSession, error) {
	session, err := OpenSignalHandoffSession(ctx, path, c)
	if err != nil {
		return nil, err
	}
	r := session.(*recoveryReconciler)
	if err = r.initializeSignalCursor(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}
func signalCursorSchema(tx *sql.Tx) (bool, error) {
	var version string
	err := tx.QueryRow("SELECT value FROM metadata WHERE key='signal_cursor_recovery_schema'").Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return false, err
	}
	var n int
	if e := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='reconciliation_signal_cursors'").Scan(&n); e != nil {
		return false, e
	}
	if err == sql.ErrNoRows && n == 0 {
		return false, nil
	}
	if err != nil || version != "1" || n != 1 {
		return false, fmt.Errorf("Signal cursor recovery schema changed")
	}
	return true, nil
}
func (r *recoveryReconciler) initializeSignalCursor(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		exists, err := signalCursorSchema(tx)
		if err != nil || exists {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE reconciliation_signal_cursors(account TEXT PRIMARY KEY,hash TEXT NOT NULL,body TEXT NOT NULL);
INSERT INTO metadata VALUES('signal_cursor_recovery_schema','1');`)
		return err
	})
}
func signalCursorFile(account string) string {
	return "signal-cursor-" + protocol.Hash(account) + ".json"
}
func readSignalOperationalCursor(tx *sql.Tx, p interaction.SignalRecoveryHandoff) (cursor interaction.SpoolCursor, err error) {
	cursor = p.Summary.Manifest.Relay
	var raw string
	err = tx.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", p.Target.Account).Scan(&raw)
	if err == sql.ErrNoRows {
		return cursor, nil
	}
	if err == nil {
		err = protocol.Decode([]byte(raw), &cursor, 4096)
	}
	return
}
func (r *recoveryReconciler) signalCursorHandoff(tx *sql.Tx, account string) (p interaction.SignalRecoveryHandoff, err error) {
	exists, err := signalCursorSchema(tx)
	if err != nil {
		return
	}
	if !exists {
		return p, protocol.ErrRecoveryIncomplete
	}
	var hash, raw string
	if err = tx.QueryRow("SELECT hash,body FROM reconciliation_signal_handoffs WHERE account=?", account).Scan(&hash, &raw); err != nil {
		return
	}
	if err = protocol.Decode([]byte(raw), &p, 16384); err != nil {
		return
	}
	if err = p.Validate(); err != nil {
		return
	}
	var file interaction.SignalRecoveryHandoff
	if err = r.stage.ReadJSON(signalHandoffFile(account), &file); err != nil {
		return
	}
	if p.Target.Account != account || hash != protocol.Hash(p) || hash != protocol.Hash(file) {
		return p, fmt.Errorf("Signal cursor handoff agreement changed")
	}
	current, err := r.prepareSignalHandoffAt(tx, account, &p.Before)
	if err != nil {
		return p, err
	}
	if protocol.Hash(current) != hash {
		return p, fmt.Errorf("Signal cursor reconciliation changed")
	}
	return
}
func (r *recoveryReconciler) readInstalledSignalCursor(tx *sql.Tx, account string) (p *interaction.SignalRecoveryCursor, err error) {
	var hash, raw string
	err = tx.QueryRow("SELECT hash,body FROM reconciliation_signal_cursors WHERE account=?", account).Scan(&hash, &raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return
	}
	p = &interaction.SignalRecoveryCursor{}
	if err = protocol.Decode([]byte(raw), p, 16384); err != nil {
		return
	}
	if err = p.Validate(); err != nil {
		return
	}
	var file interaction.SignalRecoveryCursor
	if err = r.stage.ReadJSON(signalCursorFile(account), &file); err != nil {
		return
	}
	if p.Handoff.Target.Account != account || hash != protocol.Hash(*p) || hash != protocol.Hash(file) {
		return p, fmt.Errorf("Signal cursor commit changed")
	}
	return
}
func (r *recoveryReconciler) SignalCursorHandoff(ctx context.Context, account string) (p interaction.SignalRecoveryHandoff, err error) {
	if err = r.signalRecoveryEnrollment(account); err != nil {
		return
	}
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		var e error
		p, e = r.signalCursorHandoff(tx, account)
		if e != nil {
			return e
		}
		installed, e := r.readInstalledSignalCursor(tx, account)
		if e != nil {
			return e
		}
		expected := p.Before
		if installed != nil {
			if protocol.Hash(installed.Handoff) != protocol.Hash(p) {
				return fmt.Errorf("Signal cursor installed another handoff")
			}
			expected = p.Target
		}
		actual, e := readSignalOperationalCursor(tx, p)
		if e != nil {
			return e
		}
		if actual != expected {
			return fmt.Errorf("Signal cursor changed outside installation")
		}
		return nil
	})
	return
}
func (r *recoveryReconciler) InstallSignalCursor(ctx context.Context, proof interaction.SignalRecoveryCursor) error {
	if err := proof.Validate(); err != nil {
		return err
	}
	account := proof.Handoff.Target.Account
	if err := r.signalRecoveryEnrollment(account); err != nil {
		return err
	}
	err := r.transactionWithValidation(ctx, func(tx *sql.Tx) error {
		handoff, err := r.signalCursorHandoff(tx, account)
		if err != nil {
			return err
		}
		if protocol.Hash(handoff) != protocol.Hash(proof.Handoff) {
			return fmt.Errorf("Signal cursor source has another handoff")
		}
		installed, err := r.readInstalledSignalCursor(tx, account)
		if err != nil {
			return err
		}
		actual, err := readSignalOperationalCursor(tx, handoff)
		if err != nil {
			return err
		}
		if installed != nil {
			if protocol.Hash(*installed) != protocol.Hash(proof) || actual != handoff.Target {
				return fmt.Errorf("Signal cursor installation regressed")
			}
			return nil
		}
		if actual != handoff.Before {
			return fmt.Errorf("Signal cursor preimage changed")
		}
		if err = r.capacity(tx, 32768); err != nil {
			return err
		}
		var intent interaction.SignalRecoveryCursor
		err = r.stage.ReadJSON(signalCursorFile(account), &intent)
		if os.IsNotExist(err) {
			if err = r.stage.WriteJSON(signalCursorFile(account), proof, false); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if protocol.Hash(intent) != protocol.Hash(proof) {
			return fmt.Errorf("Signal cursor intent changed")
		}
		if err = recoveryBoundary(r.fault, "signal_cursor_intent"); err != nil {
			return err
		}
		cursor, _ := json.Marshal(handoff.Target)
		if _, err = tx.Exec("INSERT INTO signal_imports VALUES(?,?) ON CONFLICT(account) DO UPDATE SET cursor=excluded.cursor", account, string(cursor)); err != nil {
			return err
		}
		raw, _ := json.Marshal(proof)
		if _, err = tx.Exec("INSERT INTO reconciliation_signal_cursors VALUES(?,?,?)", account, protocol.Hash(proof), string(raw)); err != nil {
			return err
		}
		return recoveryBoundary(r.fault, "signal_cursor_transaction")
	}, func() error {
		var intent interaction.SignalRecoveryCursor
		if err := r.stage.ReadJSON(signalCursorFile(account), &intent); err != nil {
			return err
		}
		if protocol.Hash(intent) != protocol.Hash(proof) {
			return fmt.Errorf("Signal cursor intent changed before commit")
		}
		return nil
	})
	if err == nil {
		err = recoveryBoundary(r.fault, "signal_cursor_commit")
	}
	return err
}
