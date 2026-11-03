package backup

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// Called with the archive writer lease. A dead process cannot retain a bundle
// lease, while a live reader/exporter prevents cleanup regardless of its age.
func (a *backupArchive) reconcileWork(state *backupState) error {
	if err := a.syncVerificationStatuses(state); err != nil {
		return err
	}
	if err := a.commitControl(state); err != nil {
		return err
	}
	for _, kind := range []string{".incoming", ".trash"} {
		parent, err := a.dir.Sub(kind)
		if err != nil {
			return err
		}
		entries, err := parent.File().ReadDir(1001)
		if err != nil && err != io.EOF {
			parent.Close()
			return err
		}
		if len(entries) > 1000 {
			parent.Close()
			return fmt.Errorf("reconciliation work limit exceeded")
		}
		for _, entry := range entries {
			if !entry.IsDir() || !safefs.ValidID(entry.Name(), "b") {
				parent.Close()
				return fmt.Errorf("foreign entry in archive work directory")
			}
			if err = a.reconcileWorkEntry(state, parent, kind, entry.Name()); err != nil {
				parent.Close()
				return err
			}
		}
		parent.Close()
	}
	return a.reconcilePublished(state)
}

func (a *backupArchive) reconcileWorkEntry(state *backupState, parent *safefs.Directory, kind, id string) error {
	lease, err := a.dir.Lock(".locks/"+id+".lock", true, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	controlLease, err := a.dir.Lock(".locks/control.lock", true, false)
	if err != nil {
		return err
	}
	defer controlLease.Close()
	if err = a.commitControlLocked(state); err != nil {
		return err
	}
	if kind == ".incoming" {
		var status, raw string
		if err = state.db.QueryRow("SELECT status,record FROM attempts WHERE id=?", id).Scan(&status, &raw); err != nil {
			return fmt.Errorf("incoming bundle has no durable ownership evidence")
		}
		if status != "receiving" && status != "failed" {
			return fmt.Errorf("ambiguous incoming publication requires explicit investigation")
		}
		var attempt protocol.SnapshotAttempt
		if err = protocol.StrictBackupJSON([]byte(raw), &attempt); err != nil || attempt.Validate() != nil {
			return fmt.Errorf("invalid incoming attempt evidence")
		}
	} else {
		var intent archiveDeletion
		if err = a.dir.ReadJSON("control/deletions/"+id+".json", &intent); err != nil {
			return err
		}
		if intent.Schema != 1 || intent.ArchiveID != a.identity.ID || intent.Receipt.BackupID != id || validateReceipt(intent.Receipt, a.identity.ID) != nil {
			return fmt.Errorf("invalid trash deletion evidence")
		}
		var control ArchiveControl
		if err = state.get("control", &control); err != nil {
			return err
		}
		if control.Pins[id] != "" || control.Revision < intent.ControlRevision {
			return fmt.Errorf("trash conflicts with current protection evidence")
		}
		if file, e := a.dir.Open(receiptPath(intent.Receipt)+"/receipt.json", unix.O_RDONLY, 0); e == nil {
			file.Close()
			return fmt.Errorf("trash ID also appears in published archive")
		} else if !os.IsNotExist(e) {
			return e
		}
	}
	dir, err := parent.Sub(id)
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.File().ReadDir(5)
	if err != nil && err != io.EOF {
		return err
	}
	if len(entries) > 4 {
		return fmt.Errorf("foreign work files prevent cleanup")
	}
	for _, entry := range entries {
		name := entry.Name()
		temporary := safefs.IsJSONTemporary(name)
		if name != "database.db" && name != "receipt.json" && !(kind == ".incoming" && temporary) {
			return fmt.Errorf("unknown file prevents managed work cleanup")
		}
		f, e := dir.Open(name, unix.O_RDONLY, 0)
		if e != nil {
			return e
		}
		f.Close()
	}
	// All ownership and type checks complete before the first unlink. A retry can
	// finish any prefix after a crash; durable intent precedes the trash rename.
	for _, entry := range entries {
		if err = unix.Unlinkat(int(dir.File().Fd()), entry.Name(), 0); err != nil {
			return err
		}
	}
	if err = dir.File().Sync(); err != nil {
		return err
	}
	if err = unix.Unlinkat(int(parent.File().Fd()), id, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	if err = parent.File().Sync(); err != nil {
		return err
	}
	if kind == ".incoming" {
		_, err = state.db.Exec("UPDATE attempts SET status='abandoned' WHERE id=?", id)
	} else {
		_, err = state.db.Exec("DELETE FROM bundles WHERE id=?", id)
	}
	if err == nil {
		err = state.put("last_reconciliation", time.Now().UTC())
	}
	return err
}
