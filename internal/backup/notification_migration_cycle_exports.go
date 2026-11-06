package backup

import (
	"database/sql"
	"fmt"
	"path/filepath"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func cycleExportPage(hash string) string {
	return filepath.Join("notification-migration-pages", hash+".json")
}

func retainMigrationExports(tx *sql.Tx, d *safefs.Directory, record *backupMigrationCycle) error {
	rows, err := tx.Query("SELECT seq,body FROM notification_exports ORDER BY seq LIMIT 10001")
	if err != nil {
		return err
	}
	var pages [][]notify.Message
	var page []notify.Message
	for rows.Next() {
		var seq uint64
		var raw string
		var m notify.Message
		if err = rows.Scan(&seq, &raw); err != nil {
			break
		}
		if err = notify.Decode([]byte(raw), &m, notify.MaxRequest); err != nil {
			break
		}
		record.Exports++
		if record.Exports > 10000 || seq != record.Exports || m.Sequence != seq || m.Producer != record.Before.Binding.Producer || m.Validate() != nil {
			err = fmt.Errorf("migration export inventory changed or exceeds bound")
			break
		}
		page = append(page, m)
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
		return err
	}
	if len(page) > 0 {
		pages = append(pages, page)
	}
	if _, err = tx.Exec("CREATE TABLE IF NOT EXISTS backup_notification_migration_pages(hash TEXT PRIMARY KEY,body TEXT NOT NULL)"); err != nil {
		return err
	}
	if err = d.Mkdir("notification-migration-pages"); err != nil {
		return err
	}
	for _, p := range pages {
		hash := notify.Hash(p)
		if err = frozenRemigrationRecord(d, cycleExportPage(hash), p); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO backup_notification_migration_pages VALUES(?,?) ON CONFLICT(hash) DO NOTHING", hash, mustCutoverJSON(p)); err != nil {
			return err
		}
		record.Pages = append(record.Pages, hash)
	}
	_, err = readMigrationExports(tx, d, *record)
	return err
}

func readMigrationExports(tx *sql.Tx, d *safefs.Directory, record backupMigrationCycle) (map[string]notify.Message, error) {
	if len(record.Pages) > 313 || record.Exports > 10000 {
		return nil, fmt.Errorf("migration export inventory exceeds bound")
	}
	result := map[string]notify.Message{}
	var count uint64
	for _, hash := range record.Pages {
		if !safefs.DigestPattern.MatchString(hash) {
			return nil, fmt.Errorf("invalid migration export page")
		}
		var raw string
		if err := tx.QueryRow("SELECT body FROM backup_notification_migration_pages WHERE hash=?", hash).Scan(&raw); err != nil {
			return nil, err
		}
		var page []notify.Message
		if err := notify.Decode([]byte(raw), &page, safefs.MaxRecordBytes); err != nil {
			return nil, err
		}
		if len(page) == 0 || len(page) > 32 || notify.Hash(page) != hash {
			return nil, fmt.Errorf("migration export page changed")
		}
		if d != nil {
			var retained []notify.Message
			if err := d.ReadJSON(cycleExportPage(hash), &retained); err != nil {
				return nil, err
			}
			if notify.Hash(retained) != hash {
				return nil, fmt.Errorf("migration export page lost independent evidence")
			}
		}
		for _, m := range page {
			count++
			if m.Sequence != count || m.Producer != record.Before.Binding.Producer || m.Validate() != nil {
				return nil, fmt.Errorf("migration export lineage changed")
			}
			if _, exists := result[m.ID]; exists {
				return nil, fmt.Errorf("duplicate migration export")
			}
			result[m.ID] = m
		}
	}
	if count != record.Exports {
		return nil, fmt.Errorf("migration export inventory incomplete")
	}
	return result, nil
}
