package worker

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func absentOriginalFile(d *safefs.Directory, name string) error {
	f, err := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err == nil {
		f.Close()
		return fmt.Errorf("unexpected original source artifact: %s", name)
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func absentOriginalTemporary(d *safefs.Directory) error {
	if err := absentOriginalFile(d, originalCopyFile); err != nil {
		return err
	}
	return originalSidecars(d, originalCopyFile, true)
}

// An archived file is a standalone database. For the still-canonical source,
// empty WAL/SHM files are harmless, but any uncheckpointed WAL or journal is drift.
func originalSidecars(d *safefs.Directory, name string, archived bool) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		f, err := d.Open(name+suffix, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		info, err := f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0600 {
			return fmt.Errorf("unsafe original source SQLite sidecar")
		}
		if archived || (suffix != "-shm" && info.Size() != 0) {
			return fmt.Errorf("unaccounted original source SQLite sidecar")
		}
	}
	return nil
}

func originalFileMatches(d *safefs.Directory, name string, m recoveryOriginalManifest) error {
	f, err := snapshotFile(d, name, m.DatabaseBytes)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() != m.DatabaseBytes {
		return fmt.Errorf("original source database length changed")
	}
	hash, err := safefs.DigestFile(f)
	if err != nil {
		return err
	}
	if hash != m.DatabaseHash {
		return fmt.Errorf("original source database checksum changed")
	}
	return nil
}

func immutableOriginalDB(d *safefs.Directory, name string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: fmt.Sprintf("/proc/self/fd/%d/%s", d.File().Fd(), name)}
	u.RawQuery = url.Values{"mode": {"ro"}, "immutable": {"1"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

// checkpointOriginal includes committed WAL contents before sealing the byte
// digest. No domain row, identity, generation or receipt is changed.
func (r *recoveryReconciler) checkpointOriginal(ctx context.Context) (m recoveryOriginalManifest, err error) {
	f, err := snapshotFile(r.dir, "state.db", r.config.MaxDatabaseBytes)
	if err != nil {
		return m, err
	}
	f.Close()
	// Validate sidecar types before SQLite can follow any of them. WAL contents
	// are allowed here only, before the immutable intent has been published.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		f, e := r.dir.Open("state.db"+suffix, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return m, e
		}
		info, e := f.Stat()
		f.Close()
		if e != nil {
			return m, e
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0600 {
			return m, fmt.Errorf("unsafe original source SQLite sidecar")
		}
	}
	db, err := snapshotDB(r.dir, "state.db", false)
	if err != nil {
		return m, err
	}
	defer db.Close()
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&m.Schema); err != nil {
		return
	}
	if err = verifySnapshotDB(ctx, db, SnapshotManifest{Identity: r.journal.Before, Schema: m.Schema}, r.config); err != nil {
		return
	}
	if err = r.checkAuthority(); err != nil {
		return
	}
	var pages, size int64
	if err = db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return
	}
	if err = db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return
	}
	if size <= 0 || pages <= 0 || pages > r.config.MaxDatabaseBytes/size {
		return m, fmt.Errorf("original source exceeds database budget")
	}
	var busy, log, checkpointed int
	if err = db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed); err != nil {
		return
	}
	if busy != 0 || log != checkpointed {
		return m, fmt.Errorf("original source WAL checkpoint incomplete")
	}
	if err = db.Close(); err != nil {
		return
	}
	if err = originalSidecars(r.dir, "state.db", false); err != nil {
		return
	}
	f, err = snapshotFile(r.dir, "state.db", r.config.MaxDatabaseBytes)
	if err != nil {
		return
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil {
		return
	}
	m.DatabaseBytes = info.Size()
	m.DatabaseHash, err = safefs.DigestFile(f)
	if err != nil {
		return
	}
	m.Version, m.Binding, m.ConfigHash, m.State = 1, r.journal.binding(), protocol.Hash(r.config), "copying"
	err = r.dir.File().Sync()
	return
}

func (r *recoveryReconciler) checkOriginalFiles(m recoveryOriginalManifest, archive bool) error {
	if err := r.checkAuthority(); err != nil {
		return err
	}
	if err := originalSidecars(r.dir, "state.db", false); err != nil {
		return err
	}
	if r.publication != nil {
		if err := r.checkPublicationSource(m); err != nil {
			return err
		}
	} else if err := originalFileMatches(r.dir, "state.db", m); err != nil {
		return err
	}
	if archive {
		if err := originalSidecars(r.stage, originalArchiveFile, true); err != nil {
			return err
		}
		if err := originalFileMatches(r.stage, originalArchiveFile, m); err != nil {
			return err
		}
	}
	return nil
}
