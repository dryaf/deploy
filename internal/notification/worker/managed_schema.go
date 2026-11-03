package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// ManagedSchemaMigration supports the explicitly supported 9 -> 10 transition.
// It holds the worker lease, retains a verified pre-migration database, and never
// starts a runtime or changes producer ownership. The installer owns service fencing.
type ManagedSchemaMigration struct {
	Operation   string            `json:"operation"`
	Identity    protocol.Identity `json:"identity"`
	ConfigHash  string            `json:"config_hash"`
	From, To    int
	BeforeHash  string `json:"before_hash"`
	BeforeBytes int64  `json:"before_bytes"`
}

func MigrateManagedSchema(ctx context.Context, path string, c Config, identity protocol.Identity, operation string, from int) error {
	return migrateManagedSchema(ctx, path, c, identity, operation, from, nil)
}

func migrateManagedSchema(ctx context.Context, path string, c Config, identity protocol.Identity, operation string, from int, fault func(string) error) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !identity.Valid() || !safefs.ValidID(operation, "n") || from != 9 || currentSchema != 10 {
		return fmt.Errorf("unsupported managed schema transition")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := privateDirectory(path)
	if err != nil {
		return err
	}
	defer d.Close()
	lease, err := d.Lock("worker.lock", true, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	var installed protocol.Identity
	if err = d.ReadJSON("identity.json", &installed); err != nil {
		return err
	}
	if installed != identity {
		return fmt.Errorf("schema migration worker identity changed")
	}
	for _, name := range []string{"recovery.json", "led-migration.json"} {
		f, e := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if e == nil {
			f.Close()
			return fmt.Errorf("resolve %s before schema migration", name)
		}
		if !os.IsNotExist(e) {
			return e
		}
	}
	s := &Store{dir: d, config: c, identity: identity, now: time.Now}
	if err = s.RequireInactiveProducers(); err != nil {
		return err
	}
	// The DB must exist before SQLite is opened; never recreate missing state.
	f, err := snapshotFile(d, "state.db", c.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	f.Close()
	// SQLite opens its own sidecars. Validate existing names before allowing
	// that access; empty WAL/journal files are valid, symlinks and hardlinks are not.
	for _, name := range []string{"state.db-wal", "state.db-shm", "state.db-journal"} {
		side, e := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		info, e := side.Stat()
		side.Close()
		if e != nil {
			return e
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0600 || info.Size() > c.MaxDatabaseBytes {
			return fmt.Errorf("unsafe schema migration SQLite sidecar")
		}
	}
	db, err := snapshotDB(d, "state.db", false)
	if err != nil {
		return err
	}
	defer db.Close()
	s.db = db
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != from && version != currentSchema {
		return fmt.Errorf("schema migration source version changed")
	}
	m := SnapshotManifest{Identity: identity, Schema: version}
	if err = verifySnapshotDB(ctx, db, m, c); err != nil {
		return err
	}
	// Checkpoint under the exclusive worker lease before copying raw database bytes.
	var busy, log, checkpointed int
	if err = db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &checkpointed); err != nil {
		return err
	}
	if busy != 0 || log > 0 {
		return fmt.Errorf("schema migration WAL checkpoint incomplete")
	}
	if err = d.Capacity(2*c.MaxDatabaseBytes + c.MinFreeBytes); err != nil {
		return err
	}
	// Reject a foreign pending operation before creating any new history.
	var pending ManagedSchemaMigration
	if e := d.ReadJSON("schema-migration.json", &pending); e == nil {
		if pending.Operation != operation || pending.Identity != identity || pending.ConfigHash != protocol.Hash(c) || pending.From != from || pending.To != currentSchema {
			return fmt.Errorf("another schema migration is pending")
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if version == currentSchema {
		existing, e := d.Sub("schema-migrations/" + operation)
		if e != nil {
			return fmt.Errorf("upgraded schema requires its original migration history: %w", e)
		}
		existing.Close()
	}
	if err = d.Mkdir("schema-migrations/" + operation); err != nil {
		return err
	}
	stage, err := d.Sub("schema-migrations/" + operation)
	if err != nil {
		return err
	}
	defer stage.Close()
	var intent ManagedSchemaMigration
	err = stage.ReadJSON("intent.json", &intent)
	if os.IsNotExist(err) {
		if version != from {
			return fmt.Errorf("upgraded schema lacks migration intent")
		}
		hash, size, e := schemaDatabaseDigest(d, "state.db", c.MaxDatabaseBytes)
		if e != nil {
			return e
		}
		intent = ManagedSchemaMigration{operation, identity, protocol.Hash(c), from, currentSchema, hash, size}
		if err = stage.WriteJSON("intent.json", intent, false); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if intent.Operation != operation || intent.Identity != identity || intent.ConfigHash != protocol.Hash(c) || intent.From != from || intent.To != currentSchema || !safefs.DigestPattern.MatchString(intent.BeforeHash) || intent.BeforeBytes <= 0 || intent.BeforeBytes > c.MaxDatabaseBytes {
		return fmt.Errorf("schema migration intent changed")
	}
	var gate ManagedSchemaMigration
	gateErr := d.ReadJSON("schema-migration.json", &gate)
	if gateErr == nil && gate != intent {
		return fmt.Errorf("another schema migration is pending")
	}
	if gateErr != nil && !os.IsNotExist(gateErr) {
		return gateErr
	}
	// A completed retry is evidence inspection, not a fresh backup or a new upgrade.
	var complete ManagedSchemaMigration
	doneErr := stage.ReadJSON("complete.json", &complete)
	if doneErr != nil && !os.IsNotExist(doneErr) {
		return doneErr
	}
	if doneErr == nil && complete != intent {
		return fmt.Errorf("schema migration completion changed")
	}
	var marker string
	markerErr := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", "schema-migration:"+operation).Scan(&marker)
	if markerErr != nil && markerErr != sql.ErrNoRows {
		return markerErr
	}
	migrated := markerErr == nil
	if migrated && (version != currentSchema || marker != protocol.Hash(intent)) {
		return fmt.Errorf("schema migration database acknowledgement changed")
	}
	if !migrated && (version != from || doneErr == nil) {
		return fmt.Errorf("schema migration lost its database acknowledgement")
	}
	if os.IsNotExist(gateErr) && doneErr != nil {
		if err = d.WriteJSON("schema-migration.json", intent, false); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(fault, "schema_intent"); err != nil {
		return err
	}
	backupHash, backupSize, backupErr := schemaDatabaseDigest(stage, "before.db", c.MaxDatabaseBytes)
	if os.IsNotExist(backupErr) && !migrated && doneErr != nil {
		hash, size, e := schemaDatabaseDigest(d, "state.db", c.MaxDatabaseBytes)
		if e != nil {
			return e
		}
		if hash != intent.BeforeHash || size != intent.BeforeBytes {
			return fmt.Errorf("schema migration source changed before backup")
		}
		// Orphan partial copies from crashes are bounded and never used as backups.
		names, e := stage.File().Readdirnames(10)
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
		if len(names) >= 10 {
			return fmt.Errorf("schema migration temporary-file budget exhausted")
		}
		temp, e := safefs.NewID("n")
		if e != nil {
			return e
		}
		temp += ".db"
		snap := SnapshotManifest{DatabaseHash: intent.BeforeHash, DatabaseBytes: intent.BeforeBytes}
		if e = copySnapshotFile(d, "state.db", stage, temp, snap); e != nil {
			return e
		}
		if e = recoveryBoundary(fault, "schema_copy"); e != nil {
			return e
		}
		if e = stage.Rename(temp, stage, "before.db"); e != nil {
			return e
		}
		backupHash, backupSize, backupErr = schemaDatabaseDigest(stage, "before.db", c.MaxDatabaseBytes)
	}
	if backupErr != nil {
		return backupErr
	}
	if backupHash != intent.BeforeHash || backupSize != intent.BeforeBytes {
		return fmt.Errorf("retained schema backup changed")
	}
	if err = recoveryBoundary(fault, "schema_backup"); err != nil {
		return err
	}
	if !migrated {
		hash, size, e := schemaDatabaseDigest(d, "state.db", c.MaxDatabaseBytes)
		if e != nil {
			return e
		}
		if hash != intent.BeforeHash || size != intent.BeforeBytes {
			return fmt.Errorf("schema migration source changed")
		}
		tx, e := db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		if e = s.migrateTransaction(tx); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?)", "schema-migration:"+operation, protocol.Hash(intent)); e != nil {
			return e
		}
		if e = recoveryBoundary(fault, "schema_transaction"); e != nil {
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	if err = recoveryBoundary(fault, "schema_committed"); err != nil {
		return err
	}
	if doneErr != nil {
		if err = stage.WriteJSON("complete.json", intent, false); err != nil {
			return err
		}
	}
	if err = recoveryBoundary(fault, "schema_complete"); err != nil {
		return err
	}
	// Remove only our exact gate, after both SQL and independent completion exist.
	err = d.ReadJSON("schema-migration.json", &gate)
	if err == nil {
		if gate != intent {
			return fmt.Errorf("schema migration gate changed")
		}
		if err = unix.Unlinkat(int(d.File().Fd()), "schema-migration.json", 0); err != nil {
			return err
		}
		if err = d.File().Sync(); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return recoveryBoundary(fault, "schema_released")
}
func schemaDatabaseDigest(d *safefs.Directory, name string, limit int64) (string, int64, error) {
	f, err := snapshotFile(d, name, limit)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	hash, err := safefs.DigestFile(f)
	return hash, info.Size(), err
}
