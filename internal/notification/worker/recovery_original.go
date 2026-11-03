package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

const originalManifestFile = "original-source.json"
const originalArchiveFile = "original.db"
const originalCopyFile = "original-copy.db"
const originalArchiveKey = "recovery-original-source"

// The file intent owns the temporary copy; the independent SQL acknowledgement
// owns the completed archive. Neither alone permits recovery readers to use it.
type recoveryOriginalManifest struct {
	Version       int
	Binding       recoveryBinding
	ConfigHash    string
	Schema        int
	DatabaseHash  string
	DatabaseBytes int64
	State         string
}

func (m recoveryOriginalManifest) hash() string {
	m.State = "copying"
	return protocol.Hash(m)
}

func (r *recoveryReconciler) originalManifest(ctx context.Context, q ledJournalReader) (m recoveryOriginalManifest, ack string, exists bool, err error) {
	err = q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", originalArchiveKey).Scan(&ack)
	if err != nil && err != sql.ErrNoRows {
		return
	}
	if err == nil && !safefs.DigestPattern.MatchString(ack) {
		return m, ack, false, fmt.Errorf("invalid original source acknowledgement")
	}
	if err == sql.ErrNoRows {
		ack = ""
	}
	f, fileErr := snapshotFile(r.stage, originalManifestFile, safefs.MaxRecordBytes)
	if fileErr == nil {
		f.Close()
		err = r.stage.ReadJSON(originalManifestFile, &m)
	} else {
		err = fileErr
	}
	if os.IsNotExist(err) {
		if ack != "" {
			return m, ack, false, fmt.Errorf("original source archive lost its intent")
		}
		for _, name := range []string{originalArchiveFile, originalCopyFile} {
			if e := absentOriginalFile(r.stage, name); e != nil {
				return m, ack, false, e
			}
			if e := originalSidecars(r.stage, name, true); e != nil {
				return m, ack, false, e
			}
		}
		return m, ack, false, nil
	}
	if err != nil {
		return
	}
	exists = true
	if m.Version != 1 || m.Binding != r.journal.binding() || m.ConfigHash != protocol.Hash(r.config) ||
		!supportedSnapshotSchema(m.Schema) || !safefs.DigestPattern.MatchString(m.DatabaseHash) ||
		m.DatabaseBytes <= 0 || m.DatabaseBytes > r.config.MaxDatabaseBytes ||
		(m.State != "copying" && m.State != "ready") || (ack != "" && ack != m.hash()) || (m.State == "ready" && ack == "") {
		err = fmt.Errorf("original source archive authority mismatch")
	}
	return
}

func (r *recoveryReconciler) preserveOriginal(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := r.checkAuthority(); err != nil {
		return "", err
	}
	m, ack, exists, err := r.originalManifest(ctx, r.db)
	if err != nil {
		return "", err
	}
	if !exists {
		if err = r.stage.Capacity(r.config.MaxDatabaseBytes + r.config.MinFreeBytes); err != nil {
			return "", err
		}
		if m, err = r.checkpointOriginal(ctx); err != nil {
			return "", err
		}
		if err = r.checkOriginalFiles(m, false); err != nil {
			return "", err
		}
		if err = r.stage.WriteJSON(originalManifestFile, m, false); err != nil {
			return "", err
		}
	}
	if err = recoveryBoundary(r.fault, "original_intent"); err != nil {
		return "", err
	}
	if err = r.checkOriginalFiles(m, false); err != nil {
		return "", err
	}
	if m.State == "ready" {
		if err = absentOriginalTemporary(r.stage); err != nil {
			return "", err
		}
		return m.hash(), r.checkOriginalReady(ctx, m)
	}
	f, e := r.stage.Open(originalArchiveFile, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if e == nil {
		f.Close()
		if err = absentOriginalTemporary(r.stage); err != nil {
			return "", err
		}
	} else if os.IsNotExist(e) && ack == "" {
		// Only the fixed temporary named by this durable intent can be discarded.
		// A completed archive is never overwritten or silently regenerated.
		f, e = r.stage.Open(originalCopyFile, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if e == nil {
			info, statErr := f.Stat()
			f.Close()
			if statErr != nil {
				return "", statErr
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0600 || info.Size() > m.DatabaseBytes {
				return "", fmt.Errorf("unsafe original source temporary")
			}
			if err = originalSidecars(r.stage, originalCopyFile, true); err != nil {
				return "", err
			}
			if err = unix.Unlinkat(int(r.stage.File().Fd()), originalCopyFile, 0); err != nil {
				return "", err
			}
			if err = r.stage.File().Sync(); err != nil {
				return "", err
			}
		} else if !os.IsNotExist(e) {
			return "", e
		}
		if err = originalSidecars(r.stage, originalCopyFile, true); err != nil {
			return "", err
		}
		if err = r.stage.Capacity(m.DatabaseBytes + r.config.MinFreeBytes); err != nil {
			return "", err
		}
		if err = copySnapshotFile(r.dir, "state.db", r.stage, originalCopyFile, SnapshotManifest{DatabaseHash: m.DatabaseHash, DatabaseBytes: m.DatabaseBytes}); err != nil {
			return "", err
		}
		if err = recoveryBoundary(r.fault, "original_copy"); err != nil {
			return "", err
		}
		if err = r.checkOriginalFiles(m, false); err != nil {
			return "", err
		}
		if err = r.stage.Rename(originalCopyFile, r.stage, originalArchiveFile); err != nil {
			return "", err
		}
	} else {
		return "", fmt.Errorf("original source archive unavailable: %w", e)
	}
	if err = r.checkOriginalFiles(m, true); err != nil {
		return "", err
	}
	if err = recoveryBoundary(r.fault, "original_archive"); err != nil {
		return "", err
	}
	db, err := immutableOriginalDB(r.stage, originalArchiveFile)
	if err != nil {
		return "", err
	}
	err = verifySnapshotDB(ctx, db, SnapshotManifest{Identity: r.journal.Before, Schema: m.Schema}, r.config)
	closeErr := db.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err = r.transactionWithValidation(ctx, func(tx *sql.Tx) error {
		current, currentAck, _, e := r.originalManifest(ctx, tx)
		if e != nil {
			return e
		}
		if current != m || currentAck != ack {
			return fmt.Errorf("original source archive intent changed")
		}
		if e = r.checkOriginalFiles(m, true); e != nil {
			return e
		}
		if ack != "" {
			return nil
		}
		if e = r.capacity(tx, len(m.hash())); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO metadata VALUES(?,?)", originalArchiveKey, m.hash())
		return e
	}, func() error {
		var current recoveryOriginalManifest
		if err := r.stage.ReadJSON(originalManifestFile, &current); err != nil {
			return err
		}
		if current != m {
			return fmt.Errorf("original source intent changed before acknowledgement")
		}
		return r.checkOriginalFiles(m, true)
	}); err != nil {
		return "", err
	}
	if err = recoveryBoundary(r.fault, "original_acknowledged"); err != nil {
		return "", err
	}
	current, currentAck, exists, err := r.originalManifest(ctx, r.db)
	if err != nil {
		return "", err
	}
	if !exists || current != m || currentAck != m.hash() {
		return "", fmt.Errorf("original source authority changed before completion")
	}
	if err = r.checkOriginalFiles(m, true); err != nil {
		return "", err
	}
	m.State = "ready"
	if err = r.stage.WriteJSON(originalManifestFile, m, true); err != nil {
		return "", err
	}
	if err = recoveryBoundary(r.fault, "original_ready"); err != nil {
		return "", err
	}
	return m.hash(), r.checkOriginalReady(ctx, m)
}

func (r *recoveryReconciler) checkOriginalReady(ctx context.Context, want recoveryOriginalManifest) error {
	current, ack, exists, err := r.originalManifest(ctx, r.db)
	if err != nil {
		return err
	}
	if !exists || current != want || current.State != "ready" || ack != want.hash() {
		return fmt.Errorf("original source completion changed")
	}
	if err = absentOriginalTemporary(r.stage); err != nil {
		return err
	}
	return r.checkOriginalFiles(current, true)
}

// Recovery readers use the sealed archive once present. An unfinished archive
// cannot fall back to mutable state.db. Legacy low-level preparations are still
// readable until preservation is explicitly performed by recovery-prepare.
func (r *recoveryReconciler) originalDB(ctx context.Context, q ledJournalReader) (*sql.DB, error) {
	m, _, exists, err := r.originalManifest(ctx, q)
	if err != nil {
		return nil, err
	}
	var db *sql.DB
	if exists {
		if m.State != "ready" {
			return nil, fmt.Errorf("original source preservation requires resume")
		}
		if err = absentOriginalTemporary(r.stage); err != nil {
			return nil, err
		}
		if err = r.checkOriginalFiles(m, true); err != nil {
			return nil, err
		}
		db, err = immutableOriginalDB(r.stage, originalArchiveFile)
	} else {
		f, e := snapshotFile(r.dir, "state.db", r.config.MaxDatabaseBytes)
		if e != nil {
			return nil, e
		}
		f.Close()
		db, err = snapshotDB(r.dir, "state.db", true)
	}
	if err != nil {
		return nil, err
	}
	var check, raw string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err == nil && check != "ok" {
		err = fmt.Errorf("original source integrity unavailable")
	}
	if err == nil {
		err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&raw)
	}
	var id protocol.Identity
	if err == nil {
		err = protocol.Decode([]byte(raw), &id, 4096)
	}
	if err == nil && id != r.journal.Before {
		err = fmt.Errorf("original source epoch mismatch")
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}
