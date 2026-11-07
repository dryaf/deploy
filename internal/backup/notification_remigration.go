package backup

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
)

func runBackupRemigration(ctx context.Context, home string, c WorkerInstallation, spec backupRemigrationSpec, revoke bool, deps backupCutoverDependencies) error {
	if spec.Cutover.Fresh != nil || c.Policy.Notifications.Mode == "shared" || spec.Version != 1 || !safefs.DigestPattern.MatchString(spec.RollbackHash) || spec.Cutover.Managed.LegacyGeneration != "" {
		return fmt.Errorf("remigration requires the exact completed legacy rollback")
	}
	if err := validateBackupCutoverSpec(spec.Cutover, c); err != nil {
		return err
	}
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	setup, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if err != nil {
		return err
	}
	defer setup.Close()
	var installed WorkerInstallation
	if err = readInstalledWorkerJSON(root, &installed); err != nil {
		return err
	}
	if notify.Hash(installed) != spec.Cutover.ConfigHash {
		return fmt.Errorf("remigration installation changed")
	}
	var journal WorkerSetupJournal
	if err = root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &journal); err != nil {
		return err
	}
	if journal.Stage != "complete" || journal.RecoveryNeeded || journal.WorkerID != c.WorkerID || journal.ArchiveID != c.ArchiveID {
		return fmt.Errorf("finish backup installation first")
	}
	paths := backupCutoverPaths(home, c)
	if err = deps.Services.Prepare(ctx, c, paths, spec.Cutover.HelperHash); err != nil {
		return err
	}
	capable, ok := deps.Services.(interface {
		PrepareRemigration(context.Context, WorkerUnitPaths, string) error
	})
	if !ok {
		return fmt.Errorf("remigration helper capability unavailable")
	}
	if err = capable.PrepareRemigration(ctx, paths, spec.Cutover.HelperHash); err != nil {
		return err
	}
	if err = deps.Services.StopLegacy(ctx); err != nil {
		return err
	}
	// The native legacy scheduler is also stopped and its exact owned units
	// checked. Canonical revocation remains mandatory even after this stop.
	legacy, ok := deps.Services.(backupLegacyServices)
	if !ok {
		return fmt.Errorf("native legacy lifecycle unavailable")
	}
	if err = legacy.PrepareLegacy(ctx, c, paths, spec.Cutover.HelperHash); err != nil {
		return err
	}
	state, err := openBackupState(paths.State, false)
	if err != nil {
		return err
	}
	defer state.Close()
	cyclePath, err := currentMigrationArtifact(state, "cycle.json")
	if err != nil {
		return err
	}
	if cyclePath != "cycle.json" {
		if err = requireMigrationCycleCapability(ctx, deps.Services, paths, spec.Cutover.HelperHash); err != nil {
			return err
		}
	}
	rollbackFile, err := currentMigrationArtifact(state, legacyRollbackFile)
	if err != nil {
		return err
	}
	remigrationPath, err := currentMigrationArtifact(state, remigrationFile)
	if err != nil {
		return err
	}
	d, err := safefs.Open(paths.State)
	if err != nil {
		return err
	}
	defer d.Close()
	var record backupRemigration
	var units map[string]string
	request := spec.Cutover.Managed
	request.Revoke = revoke
	err = deps.Managed(ctx, request, func(session install.ManagedProducerSession) error {
		if err := requireBackupSignalMigration(ctx, home, c, spec.Cutover, session, revoke); err != nil {
			return err
		}
		predecessors, ok := session.(install.ManagedRemigrationProducerSession)
		m := session.Manifest()
		if !ok || m.UnitDirectory != backupCutoverPaths(home, c).unitDirectory() {
			return fmt.Errorf("remigration requires an initialized managed gateway with predecessor checks")
		}
		var err error
		units, err = renderBackupReportUnits(paths, c.Policy, session.CredentialPaths())
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
		var retained backupRemigration
		fileErr := d.ReadJSON(remigrationPath, &retained)
		missing := os.IsNotExist(fileErr)
		if fileErr != nil && !missing {
			return fileErr
		}
		var rollback backupLegacyRollbackIntent
		if err = d.ReadJSON(rollbackFile, &rollback); err != nil {
			return err
		}
		if notify.Hash(rollback) != spec.RollbackHash {
			return fmt.Errorf("legacy rollback pin changed")
		}
		if err = predecessors.RequireRevokedPredecessors(rollback.Owner.Binding.Generation, rollback.Spec.Generation); err != nil {
			return err
		}
		anchored := false
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			if err := requireNotificationRecoveryInactive(tx); err != nil {
				return err
			}
			old, err := readBackupRemigration(tx)
			if err != nil {
				return err
			}
			if old != nil {
				if missing || notify.Hash(*old) != notify.Hash(retained) {
					return fmt.Errorf("remigration lost independent intent")
				}
				record = *old
				anchored = true
				owner, err := readBackupNotificationOwner(tx)
				if err != nil {
					return err
				}
				fenced := record.After
				fenced.Mode = "fenced"
				if owner != record.After && owner != fenced {
					return fmt.Errorf("remigration current owner changed")
				}
				var done string
				if e := backupNotificationMeta(tx, remigrationKey+"_complete", &done); e == nil {
					var completed string
					if done != notify.Hash(record) || d.ReadJSON(remigrationPath+".complete", &completed) != nil || completed != done {
						return fmt.Errorf("completed remigration lost independent acknowledgement")
					}
				} else if e != sql.ErrNoRows {
					return e
				}
			} else {
				var complete string
				if e := d.ReadJSON(remigrationPath+".complete", &complete); !os.IsNotExist(e) {
					return fmt.Errorf("remigration completion exists without SQL intent")
				}
				var before backupNotificationOwner
				if before, err = readBackupNotificationOwner(tx); err != nil {
					return err
				}
				if !missing {
					record = retained
				} else {
					evidence, err := readBackupCutoverEvidence(tx)
					if err != nil {
						return err
					}
					after := before
					if after.Schema != 3 {
						after.Schema = 2
					}
					after.Mode, after.Binding.Generation = "shared", request.Generation
					after.LegacyGeneration, after.LegacyState, after.LegacyHash = "", "", ""
					record = backupRemigration{spec, rollback, evidence, before, after}
				}
				if before != record.Before {
					return fmt.Errorf("legacy owner changed since review")
				}
				var done, revoked string
				if err = backupNotificationMeta(tx, "notification_legacy_rollback", &done); err != nil {
					return err
				}
				if err = backupNotificationMeta(tx, "notification_legacy_rollback_revoked", &revoked); err != nil {
					return err
				}
				if done != spec.RollbackHash || revoked != done {
					return fmt.Errorf("legacy rollback must be completed and revoked")
				}
				if err = validateBackupCutoverFence(tx, backupCutoverIntent{Evidence: record.Evidence}); err != nil {
					return err
				}
				// Old duplicate-risk retry grants cannot masquerade as new unsent
				// proof. Anything attempted stays held unless acceptance is retained.
				for _, row := range record.Evidence.Rows {
					if record.Spec.Cutover.Decisions[row.ID].Disposition == "unsent" {
						var event notification.NotificationEvent
						if err = notify.Decode([]byte(row.Record), &event, notify.MaxRequest); err != nil {
							return err
						}
						if err = requireBackupUnattemptedSource(tx, event); err != nil {
							return err
						}
					}
				}
			}
			if notify.Hash(record.Spec) != notify.Hash(spec) || record.After.Binding.Identity != m.Identity || record.After.Socket != m.Paths.Socket {
				return fmt.Errorf("remigration request changed retained authority")
			}
			return record.validate()
		})
		if err != nil {
			return err
		}
		if !anchored {
			status, err := session.Status()
			if err != nil {
				return err
			}
			if status.State != "unknown" && !(revoke && !missing && status.State == "revoked") {
				return fmt.Errorf("new generation exists without remigration SQL intent")
			}
		}
		// Both the original cutover and rollback remain required forever.
		historical, err := backupHistoricalBinding(state, record.Before.Binding)
		if err != nil {
			return err
		}
		if err = requireBackupOriginalCutover(state, record.Before, historical); err != nil {
			return err
		}
		if missing && revoke {
			return fmt.Errorf("no remigration to revoke")
		}
		var revoked string
		revErr := d.ReadJSON(remigrationPath+".revoked", &revoked)
		if revErr != nil && !os.IsNotExist(revErr) {
			return revErr
		}
		if !revoke && !os.IsNotExist(revErr) {
			return fmt.Errorf("remigrated generation permanently revoked")
		}
		var sqlRevoked string
		sqlRevErr := state.get(remigrationKey+"_revoked", &sqlRevoked)
		if sqlRevErr != nil && sqlRevErr != sql.ErrNoRows {
			return sqlRevErr
		}
		if sqlRevErr == nil && (sqlRevoked != notify.Hash(record) || !revoke) {
			return fmt.Errorf("remigrated generation permanently revoked")
		}
		if revoke {
			if err = session.Revoke(ctx); err != nil {
				return err
			}
			if anchored {
				cursor := notify.Cursor{Identity: record.After.Binding.Identity, Producer: record.After.Binding.Producer}
				if err = state.db.QueryRowContext(ctx, "SELECT cursor FROM notification_producer WHERE singleton=1").Scan(&cursor.Position); err != nil {
					return err
				}
				for pages := 0; ; pages++ {
					if pages >= 10000 {
						return fmt.Errorf("receipt reconciliation bound reached; resume revocation")
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
			}
			if err = boundary("remigration-revoked"); err != nil {
				return err
			}
			if err = frozenRemigrationRecord(d, remigrationPath+".revoked", notify.Hash(record)); err != nil {
				return err
			}
			return backupTransferTx(ctx, state, func(tx *sql.Tx) error {
				owner := record.After
				owner.Mode = "fenced"
				if !anchored {
					owner = record.Before
				}
				if err := storeBackupEvaluationMeta(tx, "notification_owner", owner); err != nil {
					return err
				}
				return storeBackupEvaluationMeta(tx, remigrationKey+"_revoked", notify.Hash(record))
			})
		}
		if err = frozenRemigrationRecord(d, remigrationPath, record); err != nil {
			return err
		}
		if err = boundary("remigration-intent"); err != nil {
			return err
		}
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			old, err := readBackupRemigration(tx)
			if err != nil {
				return err
			}
			if old != nil {
				return nil
			}
			if err = installRemigrationSources(tx, record); err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE backup_notification_binding SET body=? WHERE singleton=1", mustCutoverJSON(record.After.Binding)); err != nil {
				return err
			}
			owner := record.After
			owner.Mode = "fenced"
			if err = storeBackupEvaluationMeta(tx, "notification_owner", owner); err != nil {
				return err
			}
			return storeBackupEvaluationMeta(tx, remigrationKey, record)
		})
		if err != nil {
			return err
		}
		if err = boundary("remigration-sql"); err != nil {
			return err
		}
		if err = session.Stage(); err != nil {
			return err
		}
		if err = boundary("remigration-staged"); err != nil {
			return err
		}
		if err = deps.Services.InstallReports(ctx, units); err != nil {
			return err
		}
		if err = session.Activate(); err != nil {
			return err
		}
		if err = boundary("remigration-active"); err != nil {
			return err
		}
		if err = frozenRemigrationRecord(d, remigrationPath+".complete", notify.Hash(record)); err != nil {
			return err
		}
		if err = boundary("remigration-file"); err != nil {
			return err
		}
		if err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			if err := storeBackupEvaluationMeta(tx, remigrationKey+"_complete", notify.Hash(record)); err != nil {
				return err
			}
			return storeBackupEvaluationMeta(tx, "notification_owner", record.After)
		}); err != nil {
			return err
		}
		return boundary("remigration-shared")
	})
	if err != nil || revoke {
		return err
	}
	if err = deps.Verify(ctx, record.After); err != nil {
		return err
	}
	return deps.Services.StartReports(ctx, units)
}

func lockRemigrationSources(ctx context.Context, state *backupState) (func() error, func(), error) {
	d, err := safefs.Open(state.directory)
	if err != nil {
		return nil, nil, err
	}
	notifier, err := d.Lock("notifier.lock", true, false)
	if err != nil {
		d.Close()
		return nil, nil, err
	}
	attempt, releaseAttempt, err := lockLegacyDelivery(ctx, state)
	if err != nil {
		notifier.Close()
		d.Close()
		return nil, nil, err
	}
	check := func() error {
		if err := attempt(); err != nil {
			return err
		}
		return backupCutoverLease(d, "notifier.lock", notifier)
	}
	return check, func() { releaseAttempt(); notifier.Close(); d.Close() }, nil
}

func frozenRemigrationRecord[T any](d *safefs.Directory, name string, value T) error {
	var got T
	err := d.ReadJSON(name, &got)
	if os.IsNotExist(err) {
		return d.WriteJSON(name, value, false)
	}
	if err != nil {
		return err
	}
	if notify.Hash(got) != notify.Hash(value) {
		return fmt.Errorf("retained remigration record changed")
	}
	return nil
}
