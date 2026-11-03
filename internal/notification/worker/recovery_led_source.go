package worker

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const maxRecoveryLEDFaults = 1024

type recoveryLEDFault struct {
	ID        string `json:"id"`
	Binding   string `json:"binding"`
	Condition string `json:"condition"`
	Obsolete  bool   `json:"obsolete"`
	Identity  string `json:"identity"`
}

type recoveryLEDSource struct {
	Schema         int                `json:"schema"`
	Identity       protocol.Identity  `json:"identity"`
	LayoutPinned   bool               `json:"layout_pinned"`
	Layout         ledLayout          `json:"layout"`
	Faults         []recoveryLEDFault `json:"faults"`
	OperationsHash string             `json:"operations_hash,omitempty"`
	MigrationsHash string             `json:"migrations_hash,omitempty"`
}

type recoveryLEDSources struct {
	Snapshot recoveryLEDSource `json:"snapshot"`
	Latest   recoveryLEDSource `json:"latest"`
}

func (r *recoveryReconciler) collectLEDSources(ctx context.Context, tx *sql.Tx) (sources recoveryLEDSources, err error) {
	sources.Snapshot, err = r.recoveryLEDSnapshotSource(ctx, tx)
	if err != nil {
		return
	}
	if sources.Snapshot.Schema != r.journal.Snapshot.Schema {
		return sources, fmt.Errorf("restored LED schema changed")
	}
	// The original database was preserved by quarantine. It can contain faults
	// created after the snapshot; omitting it would hide newer ownership evidence.
	db, err := r.originalDB(ctx, tx)
	if err != nil {
		return sources, err
	}
	defer db.Close()
	sources.Latest, err = r.readLEDSource(ctx, db, r.journal.Before)
	return
}

func (r *recoveryReconciler) readLEDSource(ctx context.Context, q ledJournalReader, identity protocol.Identity) (recoveryLEDSource, error) {
	return r.readLEDSourceLimit(ctx, q, identity, maxRecoveryLEDFaults)
}

func (r *recoveryReconciler) readLEDSourceLimit(ctx context.Context, q ledJournalReader, identity protocol.Identity, limit int) (source recoveryLEDSource, err error) {
	var raw string
	if err = q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&source.Schema); err != nil {
		return
	}
	if !supportedSnapshotSchema(source.Schema) {
		return source, fmt.Errorf("unsupported LED ownership source schema")
	}
	if err = q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&raw); err != nil {
		return
	}
	if err = protocol.Decode([]byte(raw), &source.Identity, 4096); err != nil {
		return
	}
	if source.Identity != identity {
		return source, fmt.Errorf("LED ownership source identity mismatch")
	}
	source.Layout = desiredLEDLayout(r.config, r.journal.Before.Worker, os.Getuid())
	if source.Schema >= 7 {
		if source.Schema < 9 {
			if err = checkLEDLegacyMapping(ctx, q); err != nil {
				return
			}
		}
		if err = checkLEDLayout(ctx, q, r.config, r.journal.Before, false); err != nil {
			return
		}
		if err = q.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil {
			return
		}
		if err = protocol.Decode([]byte(raw), &source.Layout, protocol.MaxRequest); err != nil {
			return
		}
		source.LayoutPinned = true
	}
	if source.Schema >= 8 {
		if err = checkLEDOperations(ctx, q, r.journal.Before); err != nil {
			return
		}
		if source.OperationsHash, err = ledHistoryHash(ctx, q, ""); err != nil {
			return
		}
	}
	if source.Schema >= 9 {
		if err = checkLEDMigrations(ctx, q, r.config, r.journal.Before, false); err != nil {
			return
		}
		rows, e := q.QueryContext(ctx, "SELECT body,hash FROM led_migrations ORDER BY position LIMIT 17")
		if e != nil {
			return source, e
		}
		var history []struct{ Body, Hash string }
		for rows.Next() {
			var item struct{ Body, Hash string }
			if err = rows.Scan(&item.Body, &item.Hash); err != nil {
				break
			}
			history = append(history, item)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return
		}
		source.MigrationsHash = protocol.Hash(history)
	}
	query := fmt.Sprintf("SELECT id,condition,obsolete,binding,identity FROM led_states ORDER BY id LIMIT %d", limit+1)
	if source.Schema == 6 {
		query = fmt.Sprintf("SELECT id,condition,obsolete,'$worker','legacy' FROM led_states ORDER BY id LIMIT %d", limit+1)
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return source, err
	}
	defer rows.Close()
	for rows.Next() {
		var f recoveryLEDFault
		var obsolete int
		if err = rows.Scan(&f.ID, &f.Condition, &obsolete, &f.Binding, &f.Identity); err != nil {
			return
		}
		if obsolete != 0 && obsolete != 1 {
			return source, fmt.Errorf("invalid LED cleanup tombstone")
		}
		f.Obsolete = obsolete == 1
		source.Faults = append(source.Faults, f)
	}
	if err = rows.Err(); err != nil {
		return
	}
	err = r.validateLEDSourceLimit(source, identity, limit)
	return
}

func (r *recoveryReconciler) validateLEDSource(source recoveryLEDSource, identity protocol.Identity) error {
	return r.validateLEDSourceLimit(source, identity, maxRecoveryLEDFaults)
}

func (r *recoveryReconciler) validateLEDSourceLimit(source recoveryLEDSource, identity protocol.Identity, limit int) error {
	if !supportedSnapshotSchema(source.Schema) || source.Identity != identity || source.LayoutPinned != (source.Schema >= 7) || len(source.Faults) > limit {
		return fmt.Errorf("invalid or oversized LED ownership source")
	}
	if err := checkLEDLayoutValue(source.Layout, r.config, r.journal.Before, false); err != nil {
		return err
	}
	if source.Schema < 9 && (source.Layout.Version != 1 || source.Layout.Revision != "") {
		return fmt.Errorf("legacy source cannot prove a migrated layout")
	}
	if source.Schema >= 8 {
		if !safefs.DigestPattern.MatchString(source.OperationsHash) {
			return fmt.Errorf("missing LED operation evidence")
		}
	} else if source.OperationsHash != "" {
		return fmt.Errorf("unexpected LED operation evidence")
	}
	if source.Schema >= 9 {
		if !safefs.DigestPattern.MatchString(source.MigrationsHash) {
			return fmt.Errorf("missing LED mapping history")
		}
	} else if source.MigrationsHash != "" {
		return fmt.Errorf("unexpected LED mapping history")
	}
	for i, f := range source.Faults {
		if !ledPatternName.MatchString(f.ID) || (i > 0 && source.Faults[i-1].ID >= f.ID) || !slices.Contains([]string{"critical", "warning", "unknown", "paused"}, f.Condition) {
			return fmt.Errorf("invalid LED fault inventory")
		}
		if f.Binding != workerLEDBinding && !slices.ContainsFunc(source.Layout.Bindings, func(b LEDBinding) bool { return b.ID == f.Binding }) {
			return fmt.Errorf("LED fault binding is not enrolled")
		}
		if source.Schema == 6 {
			if f.Binding != workerLEDBinding || f.Identity != "legacy" {
				return fmt.Errorf("unproven legacy LED identity")
			}
			continue
		}
		if err := validateLEDFaultIdentity(f.ID, f.Binding, f.Identity, r.journal.Before, source.Layout.Legacy); err != nil {
			return err
		}
	}
	return nil
}
