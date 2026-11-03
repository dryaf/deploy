package backup

import (
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// Keep missing catalog rows as evidence. Deleting them would silently promote an
// older snapshot to latest and could turn lost protection back into green health.
func (a *backupArchive) reconcileBundleAvailability(state *backupState, listing ArchiveListing) error {
	if !listing.Complete {
		return fmt.Errorf("incomplete scan cannot establish bundle availability")
	}
	present := make(map[string]bool, len(listing.Entries))
	for _, receipt := range listing.Entries {
		present[receipt.BackupID] = true
	}
	tx, err := state.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query("SELECT id FROM bundles")
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if present[id] {
			_, err = tx.Exec("DELETE FROM meta WHERE key=?", "unavailable/"+id)
		} else {
			_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES(?, '"published bundle missing"') ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "unavailable/"+id)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *backupState) latestCatalogReceipt(instance ExpectedInstance) (SnapshotReceipt, error) {
	var receipt SnapshotReceipt
	var raw string
	err := s.db.QueryRow(`SELECT record FROM bundles WHERE json_extract(record,'$.header.attempt.source_id')=? AND json_extract(record,'$.header.attempt.service')=? AND json_extract(record,'$.header.generation')=? ORDER BY json_extract(record,'$.writer_epoch') DESC,json_extract(record,'$.header.attempt.capture_sequence') DESC LIMIT 1`, instance.SourceID, instance.Service, instance.Generation).Scan(&raw)
	if err == nil {
		err = safefs.DecodeJSON([]byte(raw), &receipt, safefs.MaxRecordBytes)
	}
	return receipt, err
}

// The independent health probe checks actual files, not just stale catalog rows.
// This is bounded by active instances rather than the size of retained history.
func (a *backupArchive) refreshLatestAvailability(state *backupState) error {
	var control ArchiveControl
	if err := state.get("control", &control); err != nil {
		return err
	}
	unavailable := false
	for _, instance := range control.Instances {
		if instance.Retired || control.RetiredSources[instance.SourceID] != "" {
			continue
		}
		receipt, err := state.latestCatalogReceipt(instance)
		if err == sql.ErrNoRows {
			unavailable = true
			continue // latestVerificationProblem reports the absent catalog entry.
		}
		if err != nil {
			return err
		}
		// Only inspect immutable metadata and file presence. The index lease
		// excludes publication/pruning without conflicting with verification.
		index, err := a.dir.Lock(".locks/index.lock", false, false)
		if err != nil {
			return err // Contention is not evidence of a missing bundle.
		}
		problem := ""
		dir, err := a.openBundleDirectory(receipt)
		if err == nil {
			file, e := dir.Open("database.db", unix.O_RDONLY|unix.O_NONBLOCK, 0)
			err = e
			if err == nil {
				info, e := file.Stat()
				file.Close()
				if e != nil || !info.Mode().IsRegular() || info.Size() != receipt.Header.Size {
					err = fmt.Errorf("missing or wrong-size database")
				}
			}
			dir.Close()
		}
		index.Close()
		if err != nil {
			problem = "published bundle unavailable"
			unavailable = true
		}
		if err = state.put("unavailable/"+receipt.BackupID, problem); err != nil {
			return err
		}
	}
	if unavailable {
		return fmt.Errorf("latest snapshot bundle unavailable")
	}
	return nil
}
