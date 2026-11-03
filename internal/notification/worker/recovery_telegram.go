package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type TelegramRecoverySummary struct {
	Operation                           string
	Identity                            protocol.Identity
	Source                              SnapshotManifest
	Account                             string
	RestoredOffset, RetainedOffset      int64
	Records, Mapped, Unmapped, Rejected uint64
	EvidenceDigest                      string
	// A verified artifact proves retained evidence, not complete provider history.
	HistoryComplete, IntakeEnabled bool
}
type telegramRecoverySession struct {
	Manifest       telegramRecoveryManifest
	RestoredOffset int64
	Summary        *TelegramRecoverySummary
}

// ReconcileTelegramHistory is an offline evidence import, not a restore or
// provider-polling API. The worker remains stopped and quarantined throughout.
// The caller supplies a separately retained, verified gateway snapshot; no bot
// credentials, new command authorization or operational offset changes are used.
func ReconcileTelegramHistory(ctx context.Context, statePath, historyPath, account string, c Config) (TelegramRecoverySummary, error) {
	return reconcileTelegramHistory(ctx, statePath, historyPath, account, c, nil)
}
func reconcileTelegramHistory(ctx context.Context, statePath, historyPath, account string, c Config, fault func(string) error) (summary TelegramRecoverySummary, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	r, err := openRecoveryReconciler(ctx, statePath, c, nil)
	if err != nil {
		return summary, err
	}
	defer r.Close()
	known := false
	if c.Interaction != nil {
		for _, d := range c.Interaction.Destinations {
			if d.Provider == "telegram" && d.Account == account {
				known = true
			}
		}
	}
	if !known {
		return summary, fmt.Errorf("Telegram recovery account not enrolled")
	}
	m, records, err := readTelegramRecovery(ctx, historyPath, account, c)
	if err != nil {
		return summary, err
	}
	if m.Snapshot.Identity != r.journal.Before {
		return summary, fmt.Errorf("Telegram history belongs to another gateway epoch")
	}
	if err = r.initializeTelegramRecovery(ctx); err != nil {
		return summary, err
	}
	if err = r.beginTelegramRecovery(ctx, m, records); err != nil {
		return summary, err
	}
	for first := 0; first < len(records); {
		last, bytes := first, 2
		for last < len(records) && last-first < 128 {
			raw, _ := json.Marshal(records[last])
			if bytes+len(raw)+1+4096 > protocol.MaxRequest {
				break
			}
			bytes += len(raw) + 1
			last++
		}
		if last == first {
			return summary, fmt.Errorf("Telegram recovery record exceeds page bound")
		}
		if err = r.importTelegramRecovery(ctx, m, uint64(first), records[first:last], fault); err != nil {
			return summary, err
		}
		first = last
	}
	return r.finishTelegramRecovery(ctx, m, fault)
}
func (r *recoveryReconciler) initializeTelegramRecovery(ctx context.Context) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		var version string
		err := tx.QueryRow("SELECT value FROM metadata WHERE key='telegram_reconciliation_schema'").Scan(&version)
		if err == nil {
			if version != "1" {
				return fmt.Errorf("unknown Telegram recovery schema")
			}
			for _, table := range []string{"reconciliation_telegram_sessions", "reconciliation_telegram_records"} {
				var n int
				if err = tx.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
					return err
				}
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE reconciliation_telegram_sessions(account TEXT PRIMARY KEY,body TEXT NOT NULL);
CREATE TABLE reconciliation_telegram_records(account TEXT NOT NULL,ordinal INTEGER NOT NULL,id INTEGER NOT NULL,hash TEXT NOT NULL,body TEXT NOT NULL,PRIMARY KEY(account,ordinal),UNIQUE(account,id));
INSERT INTO metadata VALUES('telegram_reconciliation_schema','1');`)
		return err
	})
}
func writeTelegramSession(tx *sql.Tx, s telegramRecoverySession) error {
	body, _ := json.Marshal(s)
	_, err := tx.Exec("INSERT INTO reconciliation_telegram_sessions VALUES(?,?) ON CONFLICT(account) DO UPDATE SET body=excluded.body", s.Manifest.Account, string(body))
	return err
}
func (r *recoveryReconciler) beginTelegramRecovery(ctx context.Context, m telegramRecoveryManifest, records []telegramRecoveryRecord) error {
	return r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := readTelegramSession(tx, m.Account)
		if err == nil {
			if protocol.Hash(s.Manifest) != protocol.Hash(m) {
				return fmt.Errorf("Telegram recovery evidence source changed")
			}
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		var orphan int
		if err = tx.QueryRow("SELECT count(*) FROM reconciliation_telegram_records WHERE account=?", m.Account).Scan(&orphan); err != nil {
			return err
		}
		if orphan != 0 {
			return fmt.Errorf("Telegram recovery lost its manifest")
		}
		s.Manifest = m
		err = tx.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", m.Account).Scan(&s.RestoredOffset)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if s.RestoredOffset < 0 || m.Offset < s.RestoredOffset {
			return fmt.Errorf("Telegram history is behind restored cursor")
		}
		byID := make(map[int64]telegramRecoveryRecord, len(records))
		for _, v := range records {
			byID[v.ID] = v
		}
		rows, err := tx.Query("SELECT id,hash,body FROM provider_inbox WHERE account=?", m.Account)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var hash, body string
			if err = rows.Scan(&id, &hash, &body); err != nil {
				return err
			}
			v, ok := byID[id]
			if !ok || v.Hash != hash {
				return fmt.Errorf("Telegram history lost or changed restored event")
			}
			if body != "" {
				var in interaction.Inbound
				if err = protocol.Decode([]byte(body), &in, 8192); err != nil {
					return err
				}
				if v.Inbound == nil || protocol.Hash(in) != protocol.Hash(v.Inbound) {
					return fmt.Errorf("Telegram history changed restored request")
				}
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
		return writeTelegramSession(tx, s)
	})
}
func (r *recoveryReconciler) importTelegramRecovery(ctx context.Context, m telegramRecoveryManifest, after uint64, records []telegramRecoveryRecord, fault func(string) error) error {
	raw, _ := json.Marshal(records)
	if len(records) == 0 || len(records) > 128 || len(raw)+4096 > protocol.MaxRequest || after > m.Records || uint64(len(records)) > m.Records-after {
		return fmt.Errorf("invalid Telegram recovery page")
	}
	err := r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := readTelegramSession(tx, m.Account)
		if err != nil {
			return err
		}
		if protocol.Hash(s.Manifest) != protocol.Hash(m) {
			return fmt.Errorf("Telegram recovery manifest changed")
		}
		reserved := false
		for i, v := range records {
			ordinal := after + uint64(i) + 1
			var hash string
			err = tx.QueryRow("SELECT hash FROM reconciliation_telegram_records WHERE account=? AND ordinal=?", m.Account, ordinal).Scan(&hash)
			if err == nil {
				if hash != protocol.Hash(v) {
					return fmt.Errorf("Telegram recovery replay changed")
				}
				continue
			}
			if err != sql.ErrNoRows {
				return err
			}
			if s.Summary != nil {
				return fmt.Errorf("completed Telegram recovery lost evidence")
			}
			if !reserved {
				if err = r.capacity(tx, len(raw)); err != nil {
					return err
				}
				reserved = true
			}
			if err = v.validate(m.Account, m.Offset); err != nil {
				return err
			}
			if v.Mapping != nil {
				if v.Mapping.Command != nil {
					if _, err = r.config.enrollment(v.Mapping.Command.Producer); err != nil {
						return err
					}
				}
				if err = importRetainedRequest(tx, *v.Mapping); err != nil {
					return err
				}
			}
			var oldHash, body string
			err = tx.QueryRow("SELECT hash,body FROM provider_inbox WHERE account=? AND id=?", m.Account, v.ID).Scan(&oldHash, &body)
			if err == nil {
				if oldHash != v.Hash {
					return fmt.Errorf("Telegram restored event changed")
				}
			} else if err == sql.ErrNoRows {
				if v.Inbound != nil {
					data, _ := json.Marshal(v.Inbound)
					body = string(data)
				}
				if _, err = tx.Exec("INSERT INTO provider_inbox(account,id,hash,body,state) VALUES(?,?,?,?,'recovery_quarantined')", m.Account, v.ID, v.Hash, body); err != nil {
					return err
				}
			} else {
				return err
			}
			if _, err = tx.Exec("UPDATE provider_inbox SET state='recovery_quarantined' WHERE account=? AND id=?", m.Account, v.ID); err != nil {
				return err
			}
			data, _ := json.Marshal(v)
			if _, err = tx.Exec("INSERT INTO reconciliation_telegram_records VALUES(?,?,?,?,?)", m.Account, ordinal, v.ID, protocol.Hash(v), string(data)); err != nil {
				return err
			}
		}
		return recoveryBoundary(fault, "telegram_import_transaction")
	})
	if err != nil {
		return err
	}
	return recoveryBoundary(fault, "telegram_import_commit")
}
func (r *recoveryReconciler) finishTelegramRecovery(ctx context.Context, m telegramRecoveryManifest, fault func(string) error) (summary TelegramRecoverySummary, err error) {
	err = r.transaction(ctx, func(tx *sql.Tx) error {
		s, err := readTelegramSession(tx, m.Account)
		if err != nil {
			return err
		}
		if protocol.Hash(s.Manifest) != protocol.Hash(m) {
			return fmt.Errorf("Telegram recovery manifest changed")
		}
		summary = TelegramRecoverySummary{Operation: r.journal.Operation, Identity: r.journal.After, Source: m.Snapshot, Account: m.Account, RestoredOffset: s.RestoredOffset, RetainedOffset: m.Offset, Records: m.Records, EvidenceDigest: m.Digest}
		rows, err := tx.Query("SELECT ordinal,id,hash,body FROM reconciliation_telegram_records WHERE account=? ORDER BY ordinal", m.Account)
		if err != nil {
			return err
		}
		defer rows.Close()
		var hashes []string
		var previous int64 = -1
		for rows.Next() {
			var ordinal uint64
			var id int64
			var hash, body string
			var v telegramRecoveryRecord
			if err = rows.Scan(&ordinal, &id, &hash, &body); err != nil {
				return err
			}
			if err = protocol.Decode([]byte(body), &v, protocol.MaxRequest); err != nil {
				return err
			}
			if ordinal != uint64(len(hashes))+1 || id != v.ID || id <= previous || hash != protocol.Hash(v) {
				return fmt.Errorf("Telegram recovery history damaged")
			}
			if err = v.validate(m.Account, m.Offset); err != nil {
				return err
			}
			previous = id
			hashes = append(hashes, hash)
			switch v.disposition() {
			case "mapped":
				summary.Mapped++
			case "unmapped":
				summary.Unmapped++
			case "rejected":
				summary.Rejected++
			}
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if uint64(len(hashes)) != m.Records || interaction.RecoveryEvidenceDigest(hashes) != m.Digest || previous+1 != m.Offset {
			return fmt.Errorf("Telegram recovery history incomplete")
		}
		if s.Summary != nil {
			if protocol.Hash(*s.Summary) != protocol.Hash(summary) {
				return fmt.Errorf("Telegram recovery completion changed")
			}
			return nil
		}
		s.Summary = &summary
		if err = writeTelegramSession(tx, s); err != nil {
			return err
		}
		return recoveryBoundary(fault, "telegram_finish_transaction")
	})
	if err == nil {
		err = recoveryBoundary(fault, "telegram_finish_commit")
	}
	return
}
