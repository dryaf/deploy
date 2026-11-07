package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// VerifyManagedLEDMapping checks an exact completed mapping without acquiring
// the runtime's lease or performing daemon I/O. It never resumes pending work.
func VerifyManagedLEDMapping(ctx context.Context, path string, before, after Config, socket string, identity protocol.Identity, operation string) error {
	if before.Validate() != nil || after.Validate() != nil || ledBaseConfigHash(before) != ledBaseConfigHash(after) ||
		!validLEDSocket(socket) || !identity.Valid() || !safefs.ValidID(operation, "n") {
		return fmt.Errorf("invalid managed LED mapping completion request")
	}
	return inspectManagedLEDState(ctx, path, after, identity, func(ctx context.Context, _ *safefs.Directory, tx *sql.Tx) error {
		m, err := readLEDMapping(ctx, tx, operation)
		if err != nil {
			return err
		}
		if !m.Completed || m.Intent.BeforeConfigHash != protocol.Hash(before) || m.Intent.AfterConfigHash != protocol.Hash(after) || m.Intent.Socket != socket || m.Intent.Identity != identity {
			return fmt.Errorf("LED mapping completion differs from installation intent")
		}
		var raw string
		if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
			return err
		}
		var layout ledLayout
		if protocol.Decode([]byte(raw), &layout, protocol.MaxRequest) != nil || layout.Revision != operation {
			return fmt.Errorf("LED mapping has a different successor")
		}
		return nil
	})
}

// PreflightManagedLEDBindings checks local prerequisites while the verification
// runtime may own its lease. It is advisory; apply repeats all checks under the
// exclusive lease. No intent, repair, schema change or daemon operation occurs.
func PreflightManagedLEDBindings(ctx context.Context, path string, before, after Config, identity protocol.Identity) error {
	if err := ValidateLEDBindingChange(before, after); err != nil {
		return err
	}
	return inspectManagedLEDState(ctx, path, before, identity, func(ctx context.Context, d *safefs.Directory, tx *sql.Tx) error {
		s := &Store{dir: d, config: before, identity: identity}
		if err := s.RequireInactiveProducers(); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM led_migrations").Scan(&count); err != nil {
			return err
		}
		if count >= maxLEDMigrations {
			return fmt.Errorf("LED mapping history budget exhausted")
		}
		v := ledMappingIntent{Before: desiredLEDLayout(before, identity.Worker, os.Getuid()), After: desiredLEDLayout(after, identity.Worker, os.Getuid())}
		if err := pinLEDMappingOutput(ctx, s, tx, &v); err != nil {
			return err
		}
		return checkLEDMappingPrerequisites(ctx, s, tx, v)
	})
}

func inspectManagedLEDState(ctx context.Context, path string, c Config, identity protocol.Identity, inspect func(context.Context, *safefs.Directory, *sql.Tx) error) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !identity.Valid() {
		return fmt.Errorf("managed LED inspection requires pinned identity")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	d, err := privateDirectory(path)
	if err != nil {
		return err
	}
	defer d.Close()
	check := func() error {
		var current protocol.Identity
		if err := d.ReadJSON("identity.json", &current); err != nil {
			return err
		}
		if current != identity {
			return fmt.Errorf("managed LED inspection identity changed")
		}
		for _, gate := range []string{"led-migration.json", "recovery.json", "schema-migration.json"} {
			f, err := d.Open(gate, os.O_RDONLY|unix.O_NONBLOCK, 0)
			if err == nil {
				f.Close()
				return fmt.Errorf("managed LED inspection has a pending state gate")
			}
			if !os.IsNotExist(err) {
				return err
			}
		}
		return nil
	}
	if err = check(); err != nil {
		return err
	}
	f, err := snapshotFile(d, "state.db", c.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	f.Close()
	db, err := snapshotDB(d, "state.db", true)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	var raw string
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != currentSchema {
		return fmt.Errorf("managed LED inspection schema changed")
	}
	var recovering int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM metadata WHERE key='recovery'").Scan(&recovering); err != nil {
		return err
	}
	if recovering != 0 {
		return fmt.Errorf("managed LED inspection database is quarantined")
	}
	if err = tx.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&raw); err != nil {
		return err
	}
	var stored protocol.Identity
	if protocol.Decode([]byte(raw), &stored, 4096) != nil || stored != identity {
		return fmt.Errorf("LED mapping database identity changed")
	}
	if err = checkLEDLayout(ctx, tx, c, identity, true); err != nil {
		return err
	}
	if err = checkLEDOperations(ctx, tx, identity); err != nil {
		return err
	}
	if err = checkLEDMigrations(ctx, tx, c, identity, false); err != nil {
		return err
	}
	if err = inspect(ctx, d, tx); err != nil {
		return err
	}
	return check()
}
