package backup

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

// The shared-delivery ledger is additive to the backup schema. Installation of
// this ledger does not select shared mode or transfer authority from the legacy
// notifier. The managed handoff must hold the notifier/ownership leases.
func installNotificationProducer(tx *sql.Tx, identity notify.Identity, p notify.Producer) error {
	if !identity.Valid() || !p.Valid() {
		return fmt.Errorf("invalid notification enrollment")
	}
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS notification_producer(singleton INTEGER PRIMARY KEY CHECK(singleton=1),version INTEGER NOT NULL,identity TEXT NOT NULL,producer TEXT NOT NULL,submitted INTEGER NOT NULL,cursor INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS notification_exports(id TEXT PRIMARY KEY,seq INTEGER UNIQUE NOT NULL,body TEXT NOT NULL,state TEXT NOT NULL,accepted INTEGER NOT NULL DEFAULT 0,eligible INTEGER NOT NULL DEFAULT 0,acceptance TEXT NOT NULL DEFAULT 'null');
CREATE TABLE IF NOT EXISTS notification_obligations(id TEXT PRIMARY KEY,kind TEXT NOT NULL,disposition TEXT NOT NULL,representative TEXT NOT NULL DEFAULT '',covered_by TEXT NOT NULL DEFAULT '',reason TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS notification_members(event TEXT NOT NULL,obligation TEXT NOT NULL,PRIMARY KEY(event,obligation));
CREATE TABLE IF NOT EXISTS notification_successors(event TEXT NOT NULL,predecessor TEXT NOT NULL,PRIMARY KEY(event,predecessor));`)
	if err != nil {
		return err
	}
	if err = installNotificationReceiptHistory(tx); err != nil {
		return err
	}
	i, _ := json.Marshal(identity)
	producer, _ := json.Marshal(p)
	var oldI, oldP string
	var version int
	err = tx.QueryRow("SELECT version,identity,producer FROM notification_producer WHERE singleton=1").Scan(&version, &oldI, &oldP)
	if err == nil {
		if version != 1 || oldI != string(i) || oldP != string(producer) {
			return fmt.Errorf("notification enrollment reconciliation required")
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	_, err = tx.Exec("INSERT INTO notification_producer VALUES(1,1,?,?,0,0)", string(i), string(producer))
	return err
}

type notificationObligation struct{ ID, Kind string }

// freezeNotificationExport must share the transaction that freezes the evaluated
// snapshot and domain disposition. A retry cannot silently change membership.
func freezeNotificationExport(tx *sql.Tx, m notify.Message, members []notificationObligation) (notify.Message, error) {
	if err := requireNotificationRecoveryInactive(tx); err != nil {
		return m, err
	}
	if len(members) == 0 || len(members) > 128 {
		return m, fmt.Errorf("invalid coverage membership")
	}
	members = slices.Clone(members)
	slices.SortFunc(members, func(a, b notificationObligation) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	for i, v := range members {
		if !notify.ValidToken(v.ID) || (i > 0 && v.ID == members[i-1].ID) {
			return m, fmt.Errorf("duplicate or invalid obligation")
		}
		switch v.Kind {
		case "alert", "daily", "catchup":
		default:
			return m, fmt.Errorf("invalid obligation kind")
		}
		if m.ReportType == "backlog" && v.Kind != "alert" {
			return m, fmt.Errorf("backlog cannot cover daily/catchup obligations")
		}
		if v.Kind == "catchup" && (m.Kind != "catchup" || v.ID != m.OutageID) {
			return m, fmt.Errorf("catchup coverage must name its outage")
		}
		if v.Kind == "daily" && m.Kind != "catchup" && (m.Kind != "report" || m.ReportType != "daily" || v.ID != m.ReportKey) {
			return m, fmt.Errorf("daily coverage requires an authorized report")
		}
	}
	var old string
	err := tx.QueryRow("SELECT body FROM notification_exports WHERE id=?", m.ID).Scan(&old)
	if err == nil {
		var existing notify.Message
		if err = notify.Decode([]byte(old), &existing, notify.MaxRequest); err != nil {
			return m, err
		}
		m.Sequence = existing.Sequence
		if notify.Hash(m) != notify.Hash(existing) {
			return m, fmt.Errorf("frozen export changed")
		}
		rows, err := tx.Query("SELECT o.id,o.kind FROM notification_members m JOIN notification_obligations o ON o.id=m.obligation WHERE m.event=? ORDER BY o.id", m.ID)
		if err != nil {
			return m, err
		}
		var prior []notificationObligation
		for rows.Next() {
			var v notificationObligation
			if err = rows.Scan(&v.ID, &v.Kind); err != nil {
				rows.Close()
				return m, err
			}
			prior = append(prior, v)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return m, err
		}
		if !slices.Equal(prior, members) {
			return m, fmt.Errorf("frozen membership changed")
		}
		return m, nil
	}
	if err != sql.ErrNoRows {
		return m, err
	}
	var submitted uint64
	var producerJSON string
	if err = tx.QueryRow("SELECT submitted,producer FROM notification_producer WHERE singleton=1").Scan(&submitted, &producerJSON); err != nil {
		return m, err
	}
	var p notify.Producer
	if err = notify.Decode([]byte(producerJSON), &p, 4096); err != nil {
		return m, err
	}
	if p != m.Producer {
		return m, fmt.Errorf("producer identity mismatch")
	}
	m.Sequence = submitted + 1
	if err = m.Validate(); err != nil {
		return m, err
	}
	b, _ := json.Marshal(m)
	if _, err = tx.Exec("INSERT INTO notification_exports(id,seq,body,state) VALUES(?,?,?,'frozen')", m.ID, m.Sequence, string(b)); err != nil {
		return m, err
	}
	for _, v := range members {
		if _, err = tx.Exec("INSERT INTO notification_obligations(id,kind,disposition) VALUES(?,?,'open') ON CONFLICT(id) DO NOTHING", v.ID, v.Kind); err != nil {
			return m, err
		}
		var kind, disposition, prior string
		if err = tx.QueryRow("SELECT kind,disposition,representative FROM notification_obligations WHERE id=?", v.ID).Scan(&kind, &disposition, &prior); err != nil {
			return m, err
		}
		if kind != v.Kind || disposition != "open" {
			return m, fmt.Errorf("obligation is no longer open")
		}
		if prior != "" {
			var state string
			if err = tx.QueryRow("SELECT state FROM notification_exports WHERE id=?", prior).Scan(&state); err != nil {
				return m, err
			}
			if state != "expired" && state != "superseded" && state != "cancelled" && state != "provider_accepted" {
				return m, fmt.Errorf("prior representative must be settled before replacement")
			}
			if _, err = tx.Exec("INSERT INTO notification_successors VALUES(?,?) ON CONFLICT DO NOTHING", m.ID, prior); err != nil {
				return m, err
			}
		}
		if _, err = tx.Exec("INSERT INTO notification_members VALUES(?,?)", m.ID, v.ID); err != nil {
			return m, err
		}
		if _, err = tx.Exec("UPDATE notification_obligations SET representative=? WHERE id=?", m.ID, v.ID); err != nil {
			return m, err
		}
	}
	_, err = tx.Exec("UPDATE notification_producer SET submitted=? WHERE singleton=1", m.Sequence)
	return m, err
}

// Receipt pages and their cursor commit together. Terminal events never masquerade
// as delivery; open obligations survive expiry even before a successor exists.
func importNotificationReceipts(tx *sql.Tx, c notify.Cursor, receipts []notify.Receipt) error {
	if err := requireNotificationRecoveryInactive(tx); err != nil {
		return err
	}
	if err := installNotificationReceiptHistory(tx); err != nil {
		return err
	}
	var identityJSON, producerJSON string
	var previous int64
	if err := tx.QueryRow("SELECT identity,producer,cursor FROM notification_producer WHERE singleton=1").Scan(&identityJSON, &producerJSON, &previous); err != nil {
		return err
	}
	var identity notify.Identity
	var p notify.Producer
	if err := notify.Decode([]byte(identityJSON), &identity, 4096); err != nil {
		return err
	}
	if err := notify.Decode([]byte(producerJSON), &p, 4096); err != nil {
		return err
	}
	if c.Identity != identity || c.Producer != p || c.Position < previous {
		return fmt.Errorf("receipt identity/cursor mismatch")
	}
	position := previous
	for _, r := range receipts {
		if r.Position <= 0 {
			return fmt.Errorf("invalid receipt position")
		}
		if r.Position <= previous {
			if err := retainNotificationReceipt(tx, c, r, true); err != nil {
				return err
			}
			continue
		}
		if r.Position <= position || r.Position > c.Position {
			return fmt.Errorf("unordered receipt page")
		}
		position = r.Position
		var body, prior, priorEvidence string
		var accepted int
		if err := tx.QueryRow("SELECT body,state,accepted,acceptance FROM notification_exports WHERE id=?", r.EventID).Scan(&body, &prior, &accepted, &priorEvidence); err != nil {
			return err
		}
		var m notify.Message
		if err := notify.Decode([]byte(body), &m, notify.MaxRequest); err != nil {
			return err
		}
		if m.Sequence != r.Sequence {
			return fmt.Errorf("receipt event mismatch")
		}
		switch r.State {
		case "queued", "waiting_status", "sending", "retry_wait", "fenced", "blocked", "ambiguous", "expired", "superseded", "cancelled", "provider_accepted":
		default:
			return fmt.Errorf("unknown receipt outcome")
		}
		if err := retainNotificationReceipt(tx, c, r, false); err != nil {
			return err
		}
		if accepted != 0 && r.State != "provider_accepted" {
			continue
		}
		if accepted != 0 && r.State == "provider_accepted" {
			evidence, _ := json.Marshal(r.Acceptance)
			if string(evidence) != priorEvidence {
				return fmt.Errorf("conflicting acceptance evidence")
			}
			continue
		}
		if (prior == "expired" || prior == "superseded" || prior == "cancelled") && r.State != "provider_accepted" {
			continue
		}
		if r.State == "provider_accepted" {
			if r.Acceptance == nil || r.Acceptance.ObservedAt.IsZero() {
				return fmt.Errorf("acceptance evidence missing")
			}
			evidence, _ := json.Marshal(r.Acceptance)
			if _, err := tx.Exec("UPDATE notification_exports SET state=?,accepted=1,eligible=?,acceptance=? WHERE id=?", r.State, r.Acceptance.Eligible, string(evidence), r.EventID); err != nil {
				return err
			}
			if r.Acceptance.Eligible {
				if _, err := tx.Exec("UPDATE notification_obligations SET disposition='satisfied',covered_by=? WHERE disposition='open' AND id IN (SELECT obligation FROM notification_members WHERE event=?)", r.EventID, r.EventID); err != nil {
					return err
				}
			}
		} else {
			if _, err := tx.Exec("UPDATE notification_exports SET state=? WHERE id=?", r.State, r.EventID); err != nil {
				return err
			}
		}
	}
	if position != c.Position {
		return fmt.Errorf("receipt page skipped its cursor")
	}
	_, err := tx.Exec("UPDATE notification_producer SET cursor=? WHERE singleton=1", c.Position)
	return err
}

func notificationCatchupReady(tx *sql.Tx, outage string) (bool, error) {
	var pending int
	err := tx.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open' AND NOT(kind='catchup' AND id=?)", outage).Scan(&pending)
	return pending == 0, err
}

func waiveNotificationObligation(tx *sql.Tx, id, reason string) error {
	if reason == "" || len(reason) > 256 {
		return fmt.Errorf("bounded audit reason required")
	}
	var kind string
	if err := tx.QueryRow("SELECT kind FROM notification_obligations WHERE id=?", id).Scan(&kind); err != nil {
		return err
	}
	if kind == "catchup" {
		return fmt.Errorf("cannot waive an active outage's recovery obligation")
	}
	_, err := tx.Exec("UPDATE notification_obligations SET disposition='waived',reason=? WHERE id=? AND disposition='open'", reason, id)
	return err
}
