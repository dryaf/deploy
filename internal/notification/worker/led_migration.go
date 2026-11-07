package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func validLEDSocket(socket string) bool {
	return filepath.IsAbs(socket) && filepath.Clean(socket) == socket
}

// ValidateLEDBindingChange checks the effective mapping before an installer
// publishes lifecycle intent or stops a service. Representation-only edits cannot
// produce a mapping transition and must not strand a prepared installation.
func ValidateLEDBindingChange(before, after Config) error {
	if err := before.Validate(); err != nil {
		return err
	}
	if err := after.Validate(); err != nil {
		return err
	}
	if ledBaseConfigHash(before) != ledBaseConfigHash(after) {
		return fmt.Errorf("LED migration requires unchanged enrollment/runtime")
	}
	old, next := desiredLEDLayout(before, "", 0), desiredLEDLayout(after, "", 0)
	if protocol.Hash(old.Bindings) == protocol.Hash(next.Bindings) {
		return fmt.Errorf("no effective LED binding changes")
	}
	return nil
}

// MigrateLEDBindings operates on a stopped local worker under its existing lease.
// It only changes LED bindings; UID, socket and other runtime changes require
// their own lifecycle protocol. Fault resolution is deliberately not inferred.
func MigrateLEDBindings(ctx context.Context, path string, before, after Config, socket string, api LEDAPI) (LEDMigrationResult, error) {
	return migrateLEDBindings(ctx, path, before, after, socket, api, nil)
}

func migrateLEDBindings(ctx context.Context, path string, before, after Config, socket string, api LEDAPI, fault func(string) error) (result LEDMigrationResult, err error) {
	return migrateLEDBindingsPinned(ctx, path, before, after, socket, api, fault, nil)
}

type managedLEDAuthority struct {
	Identity  protocol.Identity
	Operation string
}

func MigrateManagedLEDBindings(ctx context.Context, path string, before, after Config, socket string, api LEDAPI, identity protocol.Identity, operation string) (LEDMigrationResult, error) {
	if !identity.Valid() || !safefs.ValidID(operation, "n") {
		return LEDMigrationResult{}, fmt.Errorf("invalid managed LED migration authority")
	}
	return migrateLEDBindingsPinned(ctx, path, before, after, socket, api, nil, &managedLEDAuthority{identity, operation})
}

func (a *managedLEDAuthority) check(s *Store) error {
	if a == nil {
		return nil
	}
	if s.identity != a.Identity {
		return fmt.Errorf("managed LED migration identity changed")
	}
	return s.RequireInactiveProducers()
}

func migrateLEDBindingsPinned(ctx context.Context, path string, before, after Config, socket string, api LEDAPI, fault func(string) error, authority *managedLEDAuthority) (result LEDMigrationResult, err error) {
	if err = ValidateLEDBindingChange(before, after); err != nil {
		return
	}
	if !validLEDSocket(socket) || api == nil {
		return result, fmt.Errorf("LED migration requires unchanged enrollment/runtime and an explicit socket")
	}
	d, err := privateDirectory(path)
	if err != nil {
		return result, err
	}
	var intent ledMappingIntent
	e := d.ReadJSON("led-migration.json", &intent)
	if e == nil {
		return resumeLEDMapping(ctx, d, before, after, socket, api, fault, intent, authority)
	}
	d.Close()
	if !os.IsNotExist(e) {
		return result, e
	}
	open := func(c Config) (*Store, error) {
		if authority != nil {
			return OpenManagedVerification(path, c, authority.Identity)
		}
		return Open(path, c, false)
	}
	s, err := open(before)
	if err != nil {
		original := err
		completed, e := open(after)
		if e != nil {
			return result, original
		}
		defer completed.Close()
		if e = authority.check(completed); e != nil {
			return result, e
		}
		var id string
		if e = completed.db.QueryRowContext(ctx, "SELECT id FROM led_migrations ORDER BY position DESC LIMIT 1").Scan(&id); e != nil {
			return result, original
		}
		m, e := readLEDMapping(ctx, completed.db, id)
		if e != nil || !m.Completed || m.Intent.BeforeConfigHash != protocol.Hash(before) || m.Intent.AfterConfigHash != protocol.Hash(after) || m.Intent.Socket != socket || (authority != nil && m.Intent.ID != authority.Operation) {
			return result, original
		}
		return LEDMigrationResult{m.Intent.ID, protocol.Hash(m.Intent.Before), protocol.Hash(m.Intent.After), len(m.Intent.Steps)}, nil
	}
	defer s.Close()
	if err = authority.check(s); err != nil {
		return result, err
	}
	operation := ""
	if authority != nil {
		operation = authority.Operation
	}
	m, err := prepareLEDMappingFor(ctx, s, after, socket, operation)
	if err != nil {
		return result, err
	}
	if err = preflightLEDMapping(ctx, api, m.Intent, true); err != nil {
		return result, err
	}
	if err = s.dir.WriteJSON("led-migration.json", m.Intent, false); err != nil {
		return result, err
	}
	if err = recoveryBoundary(fault, "mapping_intent"); err != nil {
		return result, err
	}
	if err = s.transaction(ctx, func(tx *sql.Tx) error { return writeLEDMapping(ctx, tx, m) }); err != nil {
		return result, err
	}
	if err = recoveryBoundary(fault, "mapping_prepared"); err != nil {
		return result, err
	}
	return finishLEDMapping(ctx, s, after, api, fault, m)
}

func prepareLEDMapping(ctx context.Context, s *Store, after Config, socket string) (m ledMappingRecord, err error) {
	return prepareLEDMappingFor(ctx, s, after, socket, "")
}

func prepareLEDMappingFor(ctx context.Context, s *Store, after Config, socket, operation string) (m ledMappingRecord, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		var raw string
		if e := tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); e != nil {
			return e
		}
		if e := protocol.Decode([]byte(raw), &m.Intent.Before, protocol.MaxRequest); e != nil {
			return e
		}
		id, e := safefs.NewID("n")
		if e != nil {
			return e
		}
		if operation != "" {
			id = operation
		}
		m.Intent.After = desiredLEDLayout(after, s.identity.Worker, os.Getuid())
		m.Intent.After.Version, m.Intent.After.Revision, m.Intent.After.Legacy = 2, id, m.Intent.Before.Legacy
		if protocol.Hash(m.Intent.Before.Bindings) == protocol.Hash(m.Intent.After.Bindings) {
			return fmt.Errorf("no LED binding changes")
		}
		var count int
		if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM led_migrations").Scan(&count); e != nil {
			return e
		}
		if count >= maxLEDMigrations {
			return fmt.Errorf("LED mapping history budget exhausted; explicit archive compaction is required")
		}
		m.Intent.Version, m.Intent.Position, m.Intent.ID, m.Intent.Identity, m.Intent.Socket = 1, count+1, id, s.identity, socket
		m.Intent.BaseConfigHash, m.Intent.BeforeConfigHash, m.Intent.AfterConfigHash = ledBaseConfigHash(s.config), protocol.Hash(s.config), protocol.Hash(after)
		if e = pinLEDMappingOutput(ctx, s, tx, &m.Intent); e != nil {
			return e
		}
		m.Intent.Steps = ledMappingIntentSteps(m.Intent)
		if m.Intent.HistoryHash, e = ledHistoryHash(ctx, tx, ""); e != nil {
			return e
		}
		m.UpdatedAt = time.Now().UTC()
		if e = validateLEDMapping(m, s.config, s.identity); e != nil {
			return e
		}
		return checkLEDMappingPrerequisites(ctx, s, tx, m.Intent)
	})
	return
}

func checkLEDMappingPrerequisites(ctx context.Context, s *Store, tx *sql.Tx, v ledMappingIntent) error {
	var expected = ledMappingIntent{Version: 1}
	if err := pinLEDMappingOutput(ctx, s, tx, &expected); err != nil {
		return err
	}
	if expected.LEDOutputOperation != v.LEDOutputOperation || expected.LEDOutputHash != v.LEDOutputHash || expected.LEDOutputDisabled != v.LEDOutputDisabled {
		return fmt.Errorf("LED mapping output disposition changed")
	}
	for _, id := range changedLEDBindings(v.Before, v.After) {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM led_states WHERE binding=?", id).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("LED binding %s still has fault or cleanup records; resolve or retire it before migration", id)
		}
	}
	var bytes int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT sum(length(body)+length(hash)+length(binding)+128) FROM led_operations),0)+COALESCE((SELECT sum(length(body)+length(hash)+length(binding)+128) FROM led_operation_heads),0)").Scan(&bytes); err != nil {
		return err
	}
	// Reserve for the archive, two mapping pins and repeated durable step outcomes.
	return s.capacity(tx, "", bytes+2*protocol.MaxRequest+64*1024, false)
}

func resumeLEDMapping(ctx context.Context, d *safefs.Directory, before, after Config, socket string, api LEDAPI, fault func(string) error, v ledMappingIntent, authority *managedLEDAuthority) (result LEDMigrationResult, err error) {
	defer d.Close()
	lease, err := d.Lock("worker.lock", true, false)
	if err != nil {
		return result, err
	}
	defer lease.Close()
	if authority != nil && (v.ID != authority.Operation || v.Identity != authority.Identity) {
		return result, fmt.Errorf("managed LED migration intent changed")
	}
	if v.BaseConfigHash != ledBaseConfigHash(before) || v.BeforeConfigHash != protocol.Hash(before) || v.AfterConfigHash != protocol.Hash(after) || v.Socket != socket {
		return result, fmt.Errorf("resume requires the original old/new configurations and LED socket")
	}
	for _, name := range []string{"recovery.json", "schema-migration.json"} {
		if gate, e := d.Open(name, os.O_RDONLY|unix.O_NONBLOCK, 0); e == nil {
			gate.Close()
			return result, fmt.Errorf("resolve %s before LED migration", name)
		} else if !os.IsNotExist(e) {
			return result, e
		}
	}
	var identity protocol.Identity
	if err = d.ReadJSON("identity.json", &identity); err != nil {
		return
	}
	initial := ledMappingRecord{Intent: v, UpdatedAt: time.Now().UTC()}
	if err = validateLEDMapping(initial, before, identity); err != nil {
		return
	}
	f, err := snapshotFile(d, "state.db", before.MaxDatabaseBytes)
	if err != nil {
		return result, err
	}
	f.Close()
	db, err := snapshotDB(d, "state.db", false)
	if err != nil {
		return result, err
	}
	defer db.Close()
	s := &Store{db: db, dir: d, config: before, identity: identity, now: time.Now}
	if err = authority.check(s); err != nil {
		return result, err
	}
	var integrity, raw string
	var schema, quarantine int
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return result, fmt.Errorf("LED migration database integrity unavailable")
	}
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema); err != nil || schema != currentSchema {
		return result, fmt.Errorf("LED migration schema mismatch")
	}
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&raw); err != nil {
		return
	}
	var saved protocol.Identity
	if protocol.Decode([]byte(raw), &saved, 4096) != nil || saved != identity {
		return result, fmt.Errorf("LED migration identity mismatch")
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key='recovery'").Scan(&quarantine); err != nil || quarantine != 0 {
		return result, fmt.Errorf("LED migration database is quarantined")
	}
	m, e := readLEDMapping(ctx, db, v.ID)
	if e == sql.ErrNoRows {
		if err = checkLEDLayout(ctx, db, before, identity, true); err != nil {
			return
		}
		if err = checkLEDOperations(ctx, db, identity); err != nil {
			return
		}
		if err = checkLEDMigrations(ctx, db, before, identity, false); err != nil {
			return
		}
		var count int
		if err = db.QueryRowContext(ctx, "SELECT count(*) FROM led_migrations").Scan(&count); err != nil {
			return
		}
		hash, e := ledHistoryHash(ctx, db, "")
		if e != nil {
			return result, e
		}
		if count+1 != v.Position || hash != v.HistoryHash {
			return result, fmt.Errorf("LED migration preparation changed")
		}
		m = initial
		err = s.transaction(ctx, func(tx *sql.Tx) error {
			if e := checkLEDMappingPrerequisites(ctx, s, tx, v); e != nil {
				return e
			}
			return writeLEDMapping(ctx, tx, m)
		})
		if err != nil {
			return
		}
	} else if e != nil {
		return result, e
	}
	if protocol.Hash(m.Intent) != protocol.Hash(v) {
		return result, fmt.Errorf("LED migration fence disagrees with database")
	}
	current := before
	if m.Completed {
		current = after
	}
	if err = checkLEDLayout(ctx, db, current, identity, true); err != nil {
		return
	}
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
		return
	}
	var active ledLayout
	if err = protocol.Decode([]byte(raw), &active, protocol.MaxRequest); err != nil {
		return
	}
	expected := v.Before
	if m.Completed {
		expected = v.After
	}
	if protocol.Hash(active) != protocol.Hash(expected) {
		return result, fmt.Errorf("LED migration no longer matches the current mapping")
	}
	if err = checkLEDOperations(ctx, db, identity); err != nil {
		return
	}
	if err = checkLEDMigrations(ctx, db, current, identity, true); err != nil {
		return
	}
	return finishLEDMapping(ctx, s, after, api, fault, m)
}

func finishLEDMapping(ctx context.Context, s *Store, after Config, api LEDAPI, fault func(string) error, m ledMappingRecord) (result LEDMigrationResult, err error) {
	result = LEDMigrationResult{m.Intent.ID, protocol.Hash(m.Intent.Before), protocol.Hash(m.Intent.After), len(m.Intent.Steps)}
	for !m.Completed && m.Next < len(m.Intent.Steps) {
		if err = ctx.Err(); err != nil {
			return
		}
		if err = preflightLEDMapping(ctx, api, m.Intent, false); err != nil {
			return
		}
		step := m.Intent.Steps[m.Next]
		if err = checkLEDMappingStep(ctx, api, step, m.Intent.Before.UID); err != nil {
			return
		}
		if m.Attempts == math.MaxInt64 {
			return result, fmt.Errorf("LED migration attempt counter exhausted")
		}
		m.Attempts++
		m.Outcome = "pending"
		m.UpdatedAt = time.Now().UTC()
		if err = s.transaction(ctx, func(tx *sql.Tx) error { return writeLEDMapping(ctx, tx, m) }); err != nil {
			return
		}
		if err = recoveryBoundary(fault, "mapping_rpc_intent"); err != nil {
			return
		}
		if err = ctx.Err(); err != nil {
			return
		}
		if step.Kind == "clear" {
			err = api.Clear(ctx, step.ID)
		} else {
			err = api.Set(ctx, *step.State)
		}
		if e := recoveryBoundary(fault, "mapping_rpc_returned"); e != nil {
			return result, e
		}
		m.Outcome = "uncertain"
		if err == nil {
			m.Next++
			m.Outcome = "acknowledged"
		}
		m.UpdatedAt = time.Now().UTC()
		completion, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		saveErr := s.transaction(completion, func(tx *sql.Tx) error { return writeLEDMapping(completion, tx, m) })
		cancel()
		if saveErr != nil {
			return result, saveErr
		}
		if err != nil {
			return
		}
		if err = recoveryBoundary(fault, "mapping_rpc_ack"); err != nil {
			return
		}
	}
	if !m.Completed {
		if err = verifyLEDMappingOutput(ctx, api, m.Intent); err != nil {
			// Daemon restart can erase acknowledged baselines. Rewind only the safe
			// baseline-set suffix; old baseline clears must never run after remapping.
			if m.Reconciliations == math.MaxInt64 {
				return result, fmt.Errorf("LED migration reconciliation counter exhausted")
			}
			for i, step := range m.Intent.Steps {
				if step.Kind == "set" {
					m.Next = i
					break
				}
			}
			m.Reconciliations++
			m.UpdatedAt = time.Now().UTC()
			completion, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			saveErr := s.transaction(completion, func(tx *sql.Tx) error { return writeLEDMapping(completion, tx, m) })
			cancel()
			if saveErr != nil {
				return result, saveErr
			}
			return
		}
		if err = s.transaction(ctx, func(tx *sql.Tx) error {
			if e := checkLEDLayout(ctx, tx, s.config, s.identity, true); e != nil {
				return e
			}
			if e := checkLEDMigrations(ctx, tx, s.config, s.identity, true); e != nil {
				return e
			}
			if e := checkLEDMappingPrerequisites(ctx, s, tx, m.Intent); e != nil {
				return e
			}
			hash, e := ledHistoryHash(ctx, tx, "")
			if e != nil {
				return e
			}
			if hash != m.Intent.HistoryHash {
				return fmt.Errorf("LED history changed before publication")
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO led_migration_heads SELECT ?,binding,body,hash FROM led_operation_heads", m.Intent.ID); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO led_migration_operations SELECT ?,binding,revision,body,hash FROM led_operations", m.Intent.ID); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "DELETE FROM led_operations; DELETE FROM led_operation_heads"); e != nil {
				return e
			}
			raw, e := json.Marshal(m.Intent.After)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "UPDATE metadata SET value=? WHERE key='led-layout'", string(raw)); e != nil {
				return e
			}
			m.Completed = true
			m.UpdatedAt = time.Now().UTC()
			if e = writeLEDMapping(ctx, tx, m); e != nil {
				return e
			}
			return recoveryBoundary(fault, "mapping_before_commit")
		}); err != nil {
			return
		}
		if err = recoveryBoundary(fault, "mapping_committed"); err != nil {
			return
		}
	}
	if err = unix.Unlinkat(int(s.dir.File().Fd()), "led-migration.json", 0); err != nil {
		return
	}
	if err = s.dir.File().Sync(); err != nil {
		return
	}
	err = recoveryBoundary(fault, "mapping_unfenced")
	return
}
