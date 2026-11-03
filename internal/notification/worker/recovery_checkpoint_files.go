package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
	"modernc.org/sqlite"
)

func privateCheckpointDirectory(d *safefs.Directory) error {
	i, err := d.File().Stat()
	if err != nil {
		return err
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || i.Mode().Perm() != 0700 {
		return fmt.Errorf("unsafe checkpoint directory")
	}
	return nil
}

func (r *recoveryReconciler) openCheckpointDirectory(hash string) (*safefs.Directory, error) {
	if err := r.stage.Mkdir("checkpoints"); err != nil {
		return nil, err
	}
	parent, err := r.stage.Sub("checkpoints")
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if err = privateCheckpointDirectory(parent); err != nil {
		return nil, err
	}
	names, err := parent.File().Readdirnames(129)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(names) > 128 {
		return nil, fmt.Errorf("recovery checkpoint count exceeds budget")
	}
	for _, name := range names {
		if !safefs.DigestPattern.MatchString(name) {
			return nil, fmt.Errorf("unrecognized recovery checkpoint entry")
		}
	}
	d, err := parent.Sub(hash)
	if os.IsNotExist(err) {
		if len(names) == 128 {
			return nil, fmt.Errorf("recovery checkpoint budget exhausted")
		}
		if err = parent.Capacity(r.config.MaxDatabaseBytes + r.config.MinFreeBytes); err != nil {
			return nil, err
		}
		if err = parent.Mkdir(hash); err != nil {
			return nil, err
		}
		d, err = parent.Sub(hash)
	}
	if err != nil {
		return nil, err
	}
	if err = privateCheckpointDirectory(d); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func readCheckpointJSON[T any](d *safefs.Directory, name string, value *T) error {
	f, err := snapshotFile(d, name, safefs.MaxRecordBytes)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, safefs.MaxRecordBytes+1))
	if err != nil {
		return err
	}
	// Decode into a fresh value. Reusing a populated struct would let removed
	// JSON fields inherit previously observed authority on a later read.
	var decoded T
	if err = protocol.Decode(raw, &decoded, safefs.MaxRecordBytes); err != nil {
		return err
	}
	*value = decoded
	return nil
}

func emptyCheckpointDirectory(d *safefs.Directory) error {
	scan, err := d.Sub(".")
	if err != nil {
		return err
	}
	defer scan.Close()
	names, err := scan.File().Readdirnames(129)
	if err != nil && err != io.EOF {
		return err
	}
	if len(names) > 128 {
		return fmt.Errorf("checkpoint temporary budget exceeded")
	}
	for _, name := range names {
		if !safefs.IsJSONTemporary(name) {
			return fmt.Errorf("checkpoint artifact has no owning intent")
		}
	}
	return nil
}

// Only the fixed disposable copy named by a validated intent is replaceable.
// With journal_mode=OFF no SQLite sidecar is legitimate for this temporary.
func resetCheckpointTemporary(d *safefs.Directory, limit int64) error {
	return resetRecoveryCopy(d, checkpointTemporary, limit)
}

func resetRecoveryCopy(d *safefs.Directory, name string, limit int64) error {
	if err := originalSidecars(d, name, true); err != nil {
		return err
	}
	f, err := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	i, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || i.Mode().Perm() != 0600 || i.Size() > limit {
		return fmt.Errorf("unsafe checkpoint temporary")
	}
	if err = unix.Unlinkat(int(d.File().Fd()), name, 0); err != nil {
		return err
	}
	return d.File().Sync()
}

// SQLite's online backup copies a pinned read transaction, including WAL pages
// and implicit rowids. VACUUM INTO can renumber those rowids and would not retain
// the exact reviewed projection. The disposable destination deliberately has no
// journal; an interrupted copy is discarded only under its matching intent.
func (r *recoveryReconciler) copyCheckpoint(ctx context.Context, d *safefs.Directory, projection string) (err error) {
	if err = originalSidecars(d, checkpointDatabase, true); err != nil {
		return err
	}
	if err = resetCheckpointTemporary(d, r.config.MaxDatabaseBytes); err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: fmt.Sprintf("/proc/self/fd/%d/reconciled.db", r.stage.File().Fd())}
	u.RawQuery = url.Values{"mode": {"ro"}, "_pragma": {"query_only(ON)", "busy_timeout(1500)"}}.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var pages, size int64
	if err = tx.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		return err
	}
	if err = tx.QueryRowContext(ctx, "PRAGMA page_size").Scan(&size); err != nil {
		return err
	}
	if size < 512 || size > 65536 || pages <= 0 || pages > r.config.MaxDatabaseBytes/size {
		return fmt.Errorf("checkpoint database exceeds byte budget")
	}
	if err = d.Capacity(pages*size + r.config.MinFreeBytes); err != nil {
		return err
	}
	f, err := d.Open(checkpointTemporary, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	u.Path = fmt.Sprintf("/proc/self/fd/%d/%s", d.File().Fd(), checkpointTemporary)
	u.RawQuery = url.Values{"mode": {"rw"}, "_pragma": {"journal_mode(OFF)", "synchronous(OFF)"}}.Encode()
	err = conn.Raw(func(raw any) (err error) {
		b, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("SQLite driver does not support recovery checkpoints")
		}
		backup, err := b.NewBackup(u.String())
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, backup.Finish()) }()
		for {
			if err = ctx.Err(); err != nil {
				return err
			}
			more, e := backup.Step(128)
			if e != nil {
				return e
			}
			if int64(backup.PageCount()) != pages {
				return fmt.Errorf("checkpoint source page count changed")
			}
			if err = recoveryBoundary(r.fault, "checkpoint_copy_step"); err != nil {
				return err
			}
			if !more {
				return nil
			}
		}
	})
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = d.File().Sync(); err != nil {
		return err
	}
	return verifyCheckpointProjection(ctx, d, checkpointTemporary, projection, r.config.MaxDatabaseBytes)
}

func verifyCheckpointProjection(ctx context.Context, d *safefs.Directory, name, want string, limit int64) (err error) {
	if err = originalSidecars(d, name, true); err != nil {
		return err
	}
	f, err := snapshotFile(d, name, limit)
	if err != nil {
		return err
	}
	f.Close()
	db, err := immutableOriginalDB(d, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var integrity string
	if err = tx.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("checkpoint integrity check failed")
	}
	hash, err := recoveryProjectionHash(ctx, tx, limit)
	if err != nil {
		return err
	}
	if hash != want {
		return fmt.Errorf("checkpoint differs from reviewed projection")
	}
	return tx.Commit()
}

func (r *recoveryReconciler) inspectCheckpoint(ctx context.Context, d *safefs.Directory, name string, review RecoveryStatus) (out RecoveryCheckpoint, err error) {
	if name == checkpointDatabase {
		if err = absentOriginalFile(d, checkpointTemporary); err != nil {
			return
		}
	}
	if err = originalSidecars(d, checkpointTemporary, true); err != nil {
		return
	}
	if err = verifyCheckpointProjection(ctx, d, name, review.ProjectionHash, r.config.MaxDatabaseBytes); err != nil {
		return
	}
	f, err := snapshotFile(d, name, r.config.MaxDatabaseBytes)
	if err != nil {
		return out, err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return out, err
	}
	hash, err := safefs.DigestFile(f)
	if err != nil {
		return out, err
	}
	out = RecoveryCheckpoint{Version: 1, Operation: r.journal.Operation, ReviewHash: review.ReviewHash,
		ProjectionHash: review.ProjectionHash, DatabaseHash: hash, DatabaseBytes: i.Size()}
	out.CheckpointHash = protocol.Hash(out)
	return out, nil
}

func (r *recoveryReconciler) checkCheckpointDirectory(d *safefs.Directory, hash string) error {
	parent, err := r.stage.Sub("checkpoints")
	if err != nil {
		return err
	}
	defer parent.Close()
	if err = privateCheckpointDirectory(parent); err != nil {
		return err
	}
	current, err := parent.Sub(hash)
	if err != nil {
		return err
	}
	defer current.Close()
	if err = privateCheckpointDirectory(current); err != nil {
		return err
	}
	before, err := d.File().Stat()
	if err != nil {
		return err
	}
	after, err := current.File().Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return fmt.Errorf("checkpoint directory replaced")
	}
	return nil
}
