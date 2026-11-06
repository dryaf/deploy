package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Begin a new legacy/shared cycle by tombstoning the current shared generation
// and archiving its completed migration. A separate rollback grants the next
// legacy generation only after all exported outcomes have been reconciled.
func runBackupMigrationCycle(ctx context.Context, home string, c WorkerInstallation, spec backupMigrationCycleSpec, deps backupCutoverDependencies) (record backupMigrationCycle, err error) {
	if spec.Version != 1 || spec.Managed.Validate() != nil || spec.Managed.Revoke || spec.Managed.LegacyGeneration != "" || spec.ConfigHash != notify.Hash(c) || !safefs.DigestPattern.MatchString(spec.OwnerHash) || !safefs.DigestPattern.MatchString(spec.HelperHash) || spec.Reason == "" || len(spec.Reason) > 512 {
		return record, fmt.Errorf("migration cycle requires current installation, owner and audit pins")
	}
	root, err := safefs.Open(home)
	if err != nil {
		return record, err
	}
	defer root.Close()
	setup, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if err != nil {
		return record, err
	}
	defer setup.Close()
	paths := backupCutoverPaths(home, c)
	var current WorkerInstallation
	if err = root.ReadJSON(".config/deploy/backup-worker/runtime.json", &current); err != nil {
		return record, err
	}
	var journal WorkerSetupJournal
	if err = root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &journal); err != nil {
		return record, err
	}
	if notify.Hash(current) != spec.ConfigHash || journal.Stage != "complete" || journal.RecoveryNeeded || journal.WorkerID != c.WorkerID || journal.ArchiveID != c.ArchiveID {
		return record, fmt.Errorf("finish the pinned backup installation first")
	}
	state, err := openBackupState(paths.State, false)
	if err != nil {
		return record, err
	}
	defer state.Close()
	d, err := safefs.Open(paths.State)
	if err != nil {
		return record, err
	}
	defer d.Close()
	var completed bool
	err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		history, err := readBackupMigrationCycles(tx, d)
		if err != nil {
			return err
		}
		for _, prior := range history {
			if notify.Hash(prior.Spec) == notify.Hash(spec) {
				record, completed = prior, true
				return nil
			}
		}
		if len(history) >= 32 {
			return fmt.Errorf("migration cycle limit reached")
		}
		return nil
	})
	if err != nil || completed {
		return record, err
	}
	if err = deps.Services.Prepare(ctx, c, paths, spec.HelperHash); err != nil {
		return record, err
	}
	capable, ok := deps.Services.(interface {
		PrepareMigrationCycle(context.Context, WorkerUnitPaths, string) error
	})
	if !ok {
		return record, fmt.Errorf("migration cycle helper capability unavailable")
	}
	if err = capable.PrepareMigrationCycle(ctx, paths, spec.HelperHash); err != nil {
		return record, err
	}
	if err = deps.Services.StopLegacy(ctx); err != nil {
		return record, err
	}
	request := spec.Managed
	request.Revoke = true
	err = deps.Managed(ctx, request, func(session install.ManagedProducerSession) error {
		m := session.Manifest()
		if m.UnitDirectory != filepath.Join(home, ".config/systemd/user") {
			return fmt.Errorf("migration cycle manager changed")
		}
		units, err := renderBackupReportUnits(paths, c.Policy, session.CredentialPaths())
		if err != nil {
			return err
		}
		if err = deps.Services.StopReports(ctx, units); err != nil {
			return err
		}
		check, release, err := lockRemigrationSources(ctx, state)
		if err != nil {
			return err
		}
		defer release()
		boundary := func(at string) error {
			if err := check(); err != nil {
				return err
			}
			if err := backupCutoverLease(root, ".local/state/deploy/locks/backup-worker-setup.lock", setup); err != nil {
				return err
			}
			if err := backupCutoverLease(d, "schema.lock", state.schemaLease); err != nil {
				return err
			}
			return backupRecoveryBoundary(deps.Fault, at)
		}
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			owner, err := readBackupNotificationOwner(tx)
			if err != nil {
				return err
			}
			var pending backupMigrationCycle
			if e := backupNotificationMeta(tx, migrationCyclePendingKey, &pending); e == nil {
				fenced := pending.Before
				fenced.Mode = "fenced"
				if pending.validate() != nil || notify.Hash(pending.Spec) != notify.Hash(spec) || owner != fenced || owner.Binding.Identity != m.Identity || owner.Socket != m.Paths.Socket {
					return fmt.Errorf("pending cycle request or authority changed")
				}
				record = pending
				_, err := readMigrationExports(tx, d, record)
				return err
			} else if e != sql.ErrNoRows {
				return e
			}
			if notify.Hash(owner) != spec.OwnerHash || owner.Mode != "shared" || owner.Binding.Identity != m.Identity || owner.Socket != m.Paths.Socket {
				return fmt.Errorf("migration cycle owner changed")
			}
			if err = requireNotificationRecoveryInactive(tx); err != nil {
				return err
			}
			held, err := backupHasHeldSources(tx)
			if err != nil {
				return err
			}
			if held {
				return fmt.Errorf("resolve held source decisions before the next migration cycle")
			}
			parent, err := migrationCycleHead(tx)
			if err != nil {
				return err
			}
			prior, err := readBackupRemigration(tx)
			if err != nil {
				return err
			}
			record = backupMigrationCycle{Version: 1, Parent: parent, Spec: spec, Before: owner, Remigration: prior}
			if prior != nil {
				var rollback backupLegacyRollbackIntent
				if err = d.ReadJSON(migrationArtifact(parent, legacyRollbackFile), &rollback); err != nil {
					return err
				}
				record.Rollback = &rollback
			}
			rows, err := tx.Query("SELECT key,value FROM meta WHERE key LIKE 'notification_remigration%' OR key LIKE 'notification_legacy_rollback%' ORDER BY key")
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var item [2]string
				if err = rows.Scan(&item[0], &item[1]); err != nil {
					return err
				}
				record.Metadata = append(record.Metadata, item)
			}
			if err = rows.Err(); err != nil {
				return err
			}
			if err = rows.Close(); err != nil {
				return err
			}
			if err = retainMigrationExports(tx, d, &record); err != nil {
				return err
			}
			if err = record.validate(); err != nil {
				return err
			}
			if err = storeBackupEvaluationMeta(tx, migrationCyclePendingKey, record); err != nil {
				return err
			}
			owner.Mode = "fenced"
			return storeBackupEvaluationMeta(tx, "notification_owner", owner)
		})
		if err != nil {
			return err
		}
		if err = boundary("cycle-captured"); err != nil {
			return err
		}
		if err = requireBackupCutoverHistory(state, record.Before); err != nil {
			return err
		}
		intentName := filepath.Join("notification-cycle-intents", notify.Hash(spec)+".json")
		if err = d.Mkdir("notification-cycle-intents"); err != nil {
			return err
		}
		var prior backupMigrationCycle
		if e := d.ReadJSON(intentName, &prior); e == nil {
			if notify.Hash(prior) != notify.Hash(record) {
				return fmt.Errorf("migration cycle retry changed retained intent")
			}
		} else if !os.IsNotExist(e) {
			return e
		} else if err = d.WriteJSON(intentName, record, false); err != nil {
			return err
		}
		if err = boundary("cycle-intent"); err != nil {
			return err
		}
		if err = session.Revoke(ctx); err != nil {
			return err
		}
		if err = boundary("cycle-revoked"); err != nil {
			return err
		}
		cursor := notify.Cursor{Identity: record.Before.Binding.Identity, Producer: record.Before.Binding.Producer}
		if err = state.db.QueryRowContext(ctx, "SELECT cursor FROM notification_producer WHERE singleton=1").Scan(&cursor.Position); err != nil {
			return err
		}
		for pages := 0; ; pages++ {
			if pages >= 10000 {
				return fmt.Errorf("cycle receipt reconciliation bound reached; resume")
			}
			page, err := session.Receipts(ctx, cursor)
			if err != nil {
				return err
			}
			if err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
				return importBackupNotificationPage(tx, backupReceiptPage{Receipts: page.Receipts, Cursor: page.Cursor})
			}); err != nil {
				return err
			}
			if len(page.Receipts) == 0 {
				break
			}
			cursor = page.Cursor
		}
		head := notify.Hash(record)
		if err = d.Mkdir(filepath.Join("notification-migration-cycles", head)); err != nil {
			return err
		}
		if err = frozenRemigrationRecord(d, migrationArtifact(head, "cycle.json"), record); err != nil {
			return err
		}
		if err = boundary("cycle-archive"); err != nil {
			return err
		}
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			if err := check(); err != nil {
				return err
			}
			current, err := readBackupNotificationOwner(tx)
			if err != nil {
				return err
			}
			fenced := record.Before
			fenced.Mode = "fenced"
			if current != fenced {
				return fmt.Errorf("migration cycle owner changed before archive commit")
			}
			if _, err = tx.Exec("CREATE TABLE IF NOT EXISTS backup_notification_migration_cycles(hash TEXT PRIMARY KEY,body TEXT NOT NULL)"); err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO backup_notification_migration_cycles VALUES(?,?)", head, mustCutoverJSON(record)); err != nil {
				return err
			}
			if _, err = tx.Exec("DELETE FROM meta WHERE key LIKE 'notification_remigration%' OR key LIKE 'notification_legacy_rollback%'"); err != nil {
				return err
			}
			if err = storeBackupEvaluationMeta(tx, migrationCycleKey, head); err != nil {
				return err
			}
			if _, err = tx.Exec("DELETE FROM meta WHERE key=?", migrationCyclePendingKey); err != nil {
				return err
			}
			owner := record.Before
			owner.Schema, owner.Mode = 3, "fenced"
			owner.LegacyGeneration, owner.LegacyState, owner.LegacyHash = "", "", ""
			if err = storeBackupEvaluationMeta(tx, "notification_owner", owner); err != nil {
				return err
			}
			if err = storeBackupEvaluationMeta(tx, "notification_cutover_revoked", owner.CutoverHash); err != nil {
				return err
			}
			return boundary("cycle-transaction")
		})
		if err != nil {
			return err
		}
		return boundary("cycle-committed")
	})
	return record, err
}
