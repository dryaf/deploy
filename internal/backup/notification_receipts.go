package backup

import (
	"database/sql"
	"encoding/json"
	"fmt"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

func installNotificationReceiptHistory(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS notification_receipt_history(
identity TEXT NOT NULL,position INTEGER NOT NULL,event TEXT NOT NULL,seq INTEGER NOT NULL,state TEXT NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,
PRIMARY KEY(identity,position));
CREATE INDEX IF NOT EXISTS notification_receipt_event ON notification_receipt_history(identity,event,position);`)
	return err
}

// Receipt evidence commits with its domain effects and cursor, before any ack.
// Rows predating this ledger are not retroactively certified from acceptance text.
func retainNotificationReceipt(tx *sql.Tx, c notify.Cursor, r notify.Receipt, replay bool) error {
	evidence := notify.RetainedReceipt{Identity: c.Identity, Producer: c.Producer, Receipt: r}
	body, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	if len(body) > 16384 {
		return fmt.Errorf("retained receipt exceeds bound")
	}
	identity := notify.Hash(c.Identity)
	var hash string
	err = tx.QueryRow("SELECT hash FROM notification_receipt_history WHERE identity=? AND position=?", identity, r.Position).Scan(&hash)
	if err == nil {
		if hash != notify.Hash(evidence) {
			return fmt.Errorf("receipt replay conflicts with retained evidence")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	if replay {
		return nil
	}
	_, err = tx.Exec("INSERT INTO notification_receipt_history VALUES(?,?,?,?,?,?,?)", identity, r.Position, r.EventID, r.Sequence, r.State, notify.Hash(evidence), string(body))
	return err
}
