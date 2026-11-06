package backup

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type backupRecoveryAncestor struct {
	Version    int
	Journal    backupNotificationRecovery
	Activation backupRecoveryActivation
	Metadata   [][2]string
	Pages      []string
	LED        []backupLEDRecoveryRecord
}

func backupRecoveryActivationName(j backupNotificationRecovery) string {
	if j.Ancestor == "" {
		return recoveryActivationFile
	}
	return "notification-recovery-activation-" + j.Challenge.Operation + ".json"
}

func backupRecoveryJournalValid(j backupNotificationRecovery) bool {
	return j.Version == 1 && j.Ancestor == "" || j.Version == 2 && safefs.DigestPattern.MatchString(j.Ancestor)
}

func backupRecoveryHistoryFile(d *safefs.Directory, hash string, value any, create bool) error {
	name := "notification-recovery-history/" + hash + ".json"
	var raw json.RawMessage
	err := d.ReadJSON(name, &raw)
	if os.IsNotExist(err) && create {
		if err = d.Mkdir("notification-recovery-history"); err != nil {
			return err
		}
		return d.WriteJSON(name, value, false)
	}
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, encoded) {
		return fmt.Errorf("backup ancestor file changed")
	}
	return nil
}

func readBackupRecoveryAncestors(tx *sql.Tx, j backupNotificationRecovery, d *safefs.Directory) ([]backupRecoveryAncestor, map[string]notify.Message, error) {
	var ancestors []backupRecoveryAncestor
	messages := map[string]notify.Message{}
	cycles, err := readBackupMigrationCycles(tx, d)
	if err != nil {
		return nil, nil, err
	}
	for _, cycle := range cycles {
		exports, err := readMigrationExports(tx, d, cycle)
		if err != nil {
			return nil, nil, err
		}
		for id, m := range exports {
			if old, ok := messages[id]; ok && notify.Hash(old) != notify.Hash(m) {
				return nil, nil, fmt.Errorf("migration export histories disagree")
			}
			messages[id] = m
		}
	}
	hash := j.Ancestor
	expected := j.Binding
	for hash != "" {
		if len(ancestors) >= 32 || !safefs.DigestPattern.MatchString(hash) {
			return nil, nil, fmt.Errorf("backup recovery ancestor limit or hash invalid")
		}
		var raw string
		if err := tx.QueryRow("SELECT body FROM backup_recovery_ancestors WHERE hash=?", hash).Scan(&raw); err != nil {
			return nil, nil, err
		}
		var a backupRecoveryAncestor
		if err := notify.Decode([]byte(raw), &a, safefs.MaxRecordBytes); err != nil {
			return nil, nil, err
		}
		mapped, e := migrationBindingTo(tx, expected, a.Activation.After.Binding)
		if e != nil {
			return nil, nil, e
		}
		if a.Version != 1 || notify.Hash(a) != hash || !backupRecoveryJournalValid(a.Journal) || a.Activation.validate() != nil || a.Activation.Delivery.validate(a.Journal) != nil || a.Activation.After.Binding != mapped {
			return nil, nil, fmt.Errorf("backup ancestor authority changed")
		}
		var evidence [][2]string
		var savedJournal backupNotificationRecovery
		var savedActivation backupRecoveryActivation
		for _, v := range a.Metadata {
			if strings.HasPrefix(v[0], "notification_recovery") && !strings.HasPrefix(v[0], recoveryActivationKey) {
				evidence = append(evidence, v)
			}
			if v[0] == "notification_recovery" {
				if err := notify.Decode([]byte(v[1]), &savedJournal, safefs.MaxRecordBytes); err != nil {
					return nil, nil, err
				}
			}
			if v[0] == recoveryActivationKey+"_complete" {
				if err := notify.Decode([]byte(v[1]), &savedActivation, safefs.MaxRecordBytes); err != nil {
					return nil, nil, err
				}
			}
		}
		if notify.Hash(evidence) != a.Activation.EvidenceHash || notify.Hash(savedJournal) != notify.Hash(a.Journal) || savedActivation != a.Activation {
			return nil, nil, fmt.Errorf("backup ancestor metadata incomplete")
		}
		if d != nil {
			if err := backupRecoveryHistoryFile(d, hash, a, false); err != nil {
				return nil, nil, err
			}
			for _, suffix := range []string{"", ".complete"} {
				var file backupRecoveryActivation
				if err := d.ReadJSON(backupRecoveryActivationName(a.Journal)+suffix, &file); err != nil {
					return nil, nil, err
				}
				if file != a.Activation {
					return nil, nil, fmt.Errorf("backup ancestor activation lost")
				}
			}
		}
		var count uint64
		for _, pageHash := range a.Pages {
			if !safefs.DigestPattern.MatchString(pageHash) {
				return nil, nil, fmt.Errorf("invalid backup ancestor page hash")
			}
			if err := tx.QueryRow("SELECT body FROM backup_recovery_ancestor_pages WHERE hash=?", pageHash).Scan(&raw); err != nil {
				return nil, nil, err
			}
			var records []notify.RecoveryRecord
			if err := notify.Decode([]byte(raw), &records, safefs.MaxRecordBytes); err != nil {
				return nil, nil, err
			}
			if len(records) == 0 || len(records) > 32 || notify.Hash(records) != pageHash {
				return nil, nil, fmt.Errorf("backup ancestor page changed")
			}
			if d != nil {
				if err := backupRecoveryHistoryFile(d, pageHash, records, false); err != nil {
					return nil, nil, err
				}
			}
			for _, record := range records {
				count++
				if record.Message.Sequence != count || record.Message.Producer != j.Binding.Producer || record.Message.Validate() != nil {
					return nil, nil, fmt.Errorf("backup ancestor export changed")
				}
				if old, ok := messages[record.Message.ID]; ok && notify.Hash(old) != notify.Hash(record.Message) {
					return nil, nil, fmt.Errorf("backup ancestors disagree on message")
				}
				messages[record.Message.ID] = record.Message
			}
		}
		if count != a.Journal.Records {
			return nil, nil, fmt.Errorf("backup ancestor export incomplete")
		}
		ancestors = append(ancestors, a)
		expected = a.Journal.Binding
		hash = a.Journal.Ancestor
	}
	return ancestors, messages, nil
}

// Called under the notifier lease and the same transaction that captures and
// fences the next epoch. Files are durable before SQL can refer to them; a failed
// transaction leaves only unreferenced immutable evidence, never a new owner.
func retainBackupRecoveryAncestor(tx *sql.Tx, s *backupState, j backupNotificationRecovery, fault func(string) error) (string, error) {
	activation, err := readRecoveryActivation(tx, true)
	if err != nil {
		return "", err
	}
	d, err := safefs.Open(s.directory)
	if err != nil {
		return "", err
	}
	defer d.Close()
	if _, _, err = readBackupRecoveryAncestors(tx, j, d); err != nil {
		return "", err
	}
	for _, suffix := range []string{"", ".complete"} {
		var file backupRecoveryActivation
		if err = d.ReadJSON(backupRecoveryActivationName(j)+suffix, &file); err != nil {
			return "", err
		}
		if file != activation {
			return "", fmt.Errorf("backup recovery lost independent activation")
		}
	}
	if f, e := d.Open(backupRecoveryActivationName(j)+".revoked", os.O_RDONLY, 0); e == nil {
		f.Close()
		return "", fmt.Errorf("backup recovery activation revoked")
	} else if !os.IsNotExist(e) {
		return "", e
	}
	a := backupRecoveryAncestor{Version: 1, Journal: j, Activation: activation}
	rows, err := tx.Query("SELECT key,value FROM meta WHERE key LIKE 'notification_recovery%' OR key LIKE 'notification_led_recovery%' ORDER BY key")
	if err != nil {
		return "", err
	}
	for rows.Next() {
		var v [2]string
		if err = rows.Scan(&v[0], &v[1]); err != nil {
			break
		}
		a.Metadata = append(a.Metadata, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return "", err
	}
	var summary notify.RecoverySummary
	if err = backupNotificationMeta(tx, "notification_recovery_summary", &summary); err != nil {
		return "", err
	}
	a.LED, err = readBackupLEDRecoveryHistory(tx, j, summary)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec("CREATE TABLE IF NOT EXISTS backup_recovery_ancestors(hash TEXT PRIMARY KEY,body TEXT NOT NULL); CREATE TABLE IF NOT EXISTS backup_recovery_ancestor_pages(hash TEXT PRIMARY KEY,body TEXT NOT NULL)"); err != nil {
		return "", err
	}
	rows, err = tx.Query("SELECT seq,hash,body FROM backup_notification_recovery_records ORDER BY seq")
	if err != nil {
		return "", err
	}
	var pages [][]notify.RecoveryRecord
	var page []notify.RecoveryRecord
	var count uint64
	for rows.Next() {
		var seq uint64
		var hash, raw string
		var v notify.RecoveryRecord
		if err = rows.Scan(&seq, &hash, &raw); err != nil {
			break
		}
		if err = notify.Decode([]byte(raw), &v, notify.MaxRequest); err != nil {
			break
		}
		count++
		if seq != count || notify.Hash(v) != hash {
			err = fmt.Errorf("backup ancestor export damaged")
			break
		}
		page = append(page, v)
		if len(page) == 32 {
			pages = append(pages, page)
			page = nil
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return "", err
	}
	if len(page) > 0 {
		pages = append(pages, page)
	}
	if count != j.Records {
		return "", fmt.Errorf("backup ancestor export lost")
	}
	for _, p := range pages {
		hash := notify.Hash(p)
		if err = backupRecoveryHistoryFile(d, hash, p, true); err != nil {
			return "", err
		}
		raw, _ := json.Marshal(p)
		if _, err = tx.Exec("INSERT INTO backup_recovery_ancestor_pages VALUES(?,?) ON CONFLICT(hash) DO NOTHING", hash, string(raw)); err != nil {
			return "", err
		}
		a.Pages = append(a.Pages, hash)
	}
	hash := notify.Hash(a)
	if err = backupRecoveryHistoryFile(d, hash, a, true); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(a)
	if _, err = tx.Exec("INSERT INTO backup_recovery_ancestors VALUES(?,?)", hash, string(raw)); err != nil {
		return "", err
	}
	if err = backupRecoveryBoundary(fault, "backup_next_ancestor"); err != nil {
		return "", err
	}
	if _, err = tx.Exec("DELETE FROM meta WHERE key LIKE 'notification_recovery%' OR key LIKE 'notification_led_recovery%'; DROP TABLE backup_notification_recovery_records; DROP TABLE IF EXISTS backup_led_recovery_reports"); err != nil {
		return "", err
	}
	return hash, nil
}

func requireBackupRecoveryHistory(s *backupState) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var j backupNotificationRecovery
	if err = backupNotificationMeta(tx, "notification_recovery", &j); err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		return err
	}
	d, err := safefs.Open(s.directory)
	if err != nil {
		return err
	}
	defer d.Close()
	_, _, err = readBackupRecoveryAncestors(tx, j, d)
	return err
}

func backupOriginalRecoveryBinding(s *backupState, b backupNotificationBinding) (backupNotificationBinding, error) {
	var j backupNotificationRecovery
	if err := s.get("notification_recovery", &j); err == nil {
		if b != j.Binding {
			return b, fmt.Errorf("backup recovery binding changed")
		}
	} else if err != sql.ErrNoRows {
		return b, err
	}
	return backupHistoricalBinding(s, b)
}
