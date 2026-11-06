package worker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func (j snapshotRecovery) originalSnapshot(schema int) SnapshotManifest {
	m := j.Snapshot
	m.Schema = schema
	return m
}

func retainedSnapshotRecovery(ctx context.Context, d *safefs.Directory, m SnapshotManifest, c Config) (*snapshotRecoveryEvidence, error) {
	if m.Version != 2 {
		return nil, nil
	}
	e, err := readSnapshotRecovery(d, m, c)
	if err != nil {
		return nil, err
	}
	if skip, _ := ctx.Value(recoveryProofWalkKey{}).(bool); !skip {
		if err = verifySnapshotProofArchive(ctx, d, c, *e); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func verifyRetainedSnapshotSQL(ctx context.Context, d *safefs.Directory, db *sql.DB, m SnapshotManifest, c Config) error {
	e, err := retainedSnapshotRecovery(ctx, d, m, c)
	if err != nil {
		return err
	}
	return verifySnapshotDatabase(ctx, db, m, c, e)
}

func quarantineAncestorSnapshot(ctx context.Context, d *safefs.Directory, db *sql.DB, j snapshotRecovery, c Config, fault func(string) error) error {
	if err := verifyRetainedDatabase(ctx, d, "source.db", j.Snapshot.DatabaseHash, j.Snapshot.DatabaseBytes, c.MaxDatabaseBytes); err != nil {
		return err
	}
	if err := verifyRetainedDatabase(ctx, d, "candidate.db", j.Snapshot.DatabaseHash, j.Snapshot.DatabaseBytes, c.MaxDatabaseBytes); err != nil {
		return err
	}
	if err := verifyRetainedSnapshotSQL(ctx, d, db, j.Snapshot, c); err != nil {
		return err
	}
	return classifyRecoverySnapshot(ctx, d, db, j, fault, true)
}

// The source database and its complete ancestor publication are independently
// retained before this transaction. Move prior derived reconciliation data out
// of the current namespace, preserving every row in the new projection too.
// Domain data (including commands, aliases, outcomes and LED faults) stays put.
func archiveAncestorProjection(ctx context.Context, tx *sql.Tx, j snapshotRecovery) error {
	prefix := "ancestor_" + strings.ReplaceAll(j.Before.Recovery, "-", "_") + "_"
	if !safefs.ValidID(j.Before.Recovery, "n") {
		return fmt.Errorf("invalid ancestor epoch")
	}
	rows, err := tx.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type='table' AND (name GLOB 'reconciliation_*' OR name IN ('recovery_messages','recovery_receipts','recovery_producers','recovery_commands')) ORDER BY name LIMIT 257")
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(names) > 256 {
		return fmt.Errorf("ancestor reconciliation table budget exceeded")
	}
	quote := func(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
	// Copy rather than rename: SQLite keeps globally named indexes when a table
	// is renamed. Old index names must be available to the next reconciliation.
	for _, name := range names {
		if _, err = tx.ExecContext(ctx, "CREATE TABLE "+quote(prefix+name)+" AS SELECT * FROM "+quote(name)); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DROP TABLE "+quote(name)); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, "CREATE TABLE "+quote(prefix+"metadata")+" AS SELECT * FROM metadata"); err != nil {
		return err
	}
	for _, key := range []string{
		"recovery", "reconciliation", originalArchiveKey, publicationAnchorKey,
		"recovery_applications_schema", "recovery_retries_schema", "recovery_dispositions_schema",
		"telegram_cursor_recovery_schema", "telegram_reconciliation_schema", "telegram_drain_schema",
		"signal_cursor_recovery_schema", "signal_reconciliation_schema", "signal_handoff_schema",
		"command_reconciliation_schema", "command_installation_schema", "request_recovery_schema",
		"led-import-version", "led-evidence-version", "led-adoption-version", "led-preservation-version",
		"led-epoch-version", "led-inventory-version", "led-clear-version",
	} {
		if _, err = tx.ExecContext(ctx, "DELETE FROM metadata WHERE key=?", key); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO metadata VALUES(?,?)", "recovery-ancestor:"+j.Operation, protocol.Hash(j.Snapshot))
	return err
}
