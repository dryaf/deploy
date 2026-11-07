package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Publication and offline reconciliation do not start reporting. This explicit
// handoff checks both retained source history and the managed gateway's immutable
// checkpoint before activating its already allocated producer generation.
func runBackupRecoveryActivation(ctx context.Context, home string, c WorkerInstallation, spec backupRecoveryActivationSpec, revoke bool, deps backupCutoverDependencies) error {
	if spec.Version != 1 || spec.Managed.Validate() != nil || spec.Managed.Revoke || spec.Managed.LegacyGeneration != "" || spec.ConfigHash != notify.Hash(c) || !safefs.DigestPattern.MatchString(spec.HelperHash) || !safefs.DigestPattern.MatchString(spec.CommitHash) || spec.Managed.Producer.ID != c.WorkerID {
		return fmt.Errorf("recovery activation requires exact installation, helper, producer and source commit")
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
	if notify.Hash(installed) != spec.ConfigHash {
		return fmt.Errorf("backup installation changed")
	}
	var setupJournal WorkerSetupJournal
	if err = root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &setupJournal); err != nil {
		return err
	}
	if setupJournal.Stage != "complete" || setupJournal.RecoveryNeeded || setupJournal.WorkerID != c.WorkerID || setupJournal.ArchiveID != c.ArchiveID {
		return fmt.Errorf("backup setup is unfinished")
	}
	paths := backupCutoverPaths(home, c)
	if err = deps.Services.Prepare(ctx, c, paths, spec.HelperHash); err != nil {
		return err
	}
	capable, ok := deps.Services.(interface {
		PrepareRecovery(context.Context, WorkerUnitPaths, string) error
	})
	if !ok {
		return fmt.Errorf("backup helper lacks recovered reporting support")
	}
	if err = capable.PrepareRecovery(ctx, paths, spec.HelperHash); err != nil {
		return err
	}
	if err = deps.Services.StopLegacy(ctx); err != nil {
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
		if err = requireMigrationCycleCapability(ctx, deps.Services, paths, spec.HelperHash); err != nil {
			return err
		}
	}
	if err = backupTransferTx(ctx, state, requireNoPendingMigrationCycle); err != nil {
		return err
	}
	var currentRecovery backupNotificationRecovery
	if err = state.get("notification_recovery", &currentRecovery); err != nil {
		return err
	}
	if currentRecovery.Version == 2 {
		capable, ok := deps.Services.(interface {
			PrepareNextRecovery(context.Context, WorkerUnitPaths, string) error
		})
		if !ok {
			return fmt.Errorf("backup helper lacks repeated recovery support")
		}
		if err = capable.PrepareNextRecovery(ctx, paths, spec.HelperHash); err != nil {
			return err
		}
	}
	activationFile := backupRecoveryActivationName(currentRecovery)
	d, err := safefs.Open(paths.State)
	if err != nil {
		return err
	}
	defer d.Close()
	request := spec.Managed
	request.Revoke = revoke
	var result backupRecoveryActivation
	var units map[string]string
	err = deps.Managed(ctx, request, func(session install.ManagedProducerSession) error {
		m := session.Manifest()
		proofSession, ok := session.(install.ManagedRecoveryProducerSession)
		if !ok || m.DeliveryRecovery == nil || m.UnitDirectory != backupCutoverPaths(home, c).unitDirectory() || m.Mode != "delivery" {
			return fmt.Errorf("recovered reporting requires the exact managed recovered gateway")
		}
		var e error
		units, e = renderBackupReportUnits(paths, c.Policy, session.CredentialPaths())
		if e != nil {
			return e
		}
		if e = deps.Services.StopReports(ctx, units); e != nil {
			return e
		}
		check, release, e := lockBackupRecovery(ctx, state)
		if e != nil {
			return e
		}
		defer release()
		boundary := func(at string) error {
			if e := check(); e != nil {
				return e
			}
			if e := backupCutoverLease(root, ".local/state/deploy/locks/backup-worker-setup.lock", setup); e != nil {
				return e
			}
			if e := backupCutoverLease(d, "schema.lock", state.schemaLease); e != nil {
				return e
			}
			return backupRecoveryBoundary(deps.Fault, at)
		}
		var file backupRecoveryActivation
		e = d.ReadJSON(activationFile, &file)
		missing := os.IsNotExist(e)
		if e != nil && !missing {
			return e
		}
		var committed bool
		e = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			var prior backupRecoveryActivation
			err := backupNotificationMeta(tx, recoveryActivationKey, &prior)
			if err == nil {
				if missing || file != prior {
					return fmt.Errorf("recovery activation lost independent intent")
				}
				result, err = readRecoveryActivation(tx, false)
				if err != nil {
					return err
				}
			} else if err == sql.ErrNoRows {
				if f, e := d.Open(activationFile+".complete", os.O_RDONLY, 0); e == nil {
					f.Close()
					return fmt.Errorf("completed activation lost SQL intent")
				} else if !os.IsNotExist(e) {
					return e
				}
				if revoke {
					return fmt.Errorf("no recovered activation to revoke")
				}
				j, e := readBackupRecoveryAuthority(tx, c)
				if e != nil {
					return e
				}
				if e = validateBackupCommitPayloads(tx, j); e != nil {
					return e
				}
				var delivery backupRecoveryCommitRecord
				if e = backupNotificationMeta(tx, "notification_recovery_commit", &delivery); e != nil {
					return e
				}
				owner, e := readBackupNotificationOwner(tx)
				if e != nil {
					return e
				}
				evidence, e := recoveryActivationEvidence(tx)
				if e != nil {
					return e
				}
				after := owner
				after.Mode = "shared"
				result = backupRecoveryActivation{spec, delivery, owner, after, evidence}
				if !missing && result != file {
					return fmt.Errorf("pending recovery activation changed")
				}
			} else {
				return err
			}
			if result.Spec != spec || result.validate() != nil || result.After.Binding.Identity != m.Identity || result.After.Socket != m.Paths.Socket || result.Delivery.Proof.Generation.Ownership.Operation == "" {
				return fmt.Errorf("recovered activation pins changed")
			}
			if e := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key=?)", recoveryActivationKey+"_complete").Scan(&committed); e != nil {
				return e
			}
			if !committed {
				owner, e := readBackupNotificationOwner(tx)
				if e != nil {
					return e
				}
				if owner != result.Before {
					return fmt.Errorf("uncompleted recovery activation owner changed")
				}
			}
			if committed {
				var complete backupRecoveryActivation
				if e := d.ReadJSON(activationFile+".complete", &complete); e != nil {
					return e
				}
				if complete != result {
					return fmt.Errorf("completed activation lost file evidence")
				}
			}
			return nil
		})
		if e != nil {
			return e
		}
		originalBinding, e := backupOriginalRecoveryBinding(state, result.Delivery.Intent.Ownership.Intent.Binding)
		if e != nil {
			return e
		}
		if e = requireBackupOriginalCutover(state, result.Before, originalBinding); e != nil {
			return e
		}
		if e = proofSession.RequireRecoveryCommit(ctx, result.Delivery.Proof); e != nil {
			return e
		}
		if revoke {
			if e = session.Revoke(ctx); e != nil {
				return e
			}
			if e = boundary("recovery-activation-revoked"); e != nil {
				return e
			}
			if e = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
				if e := storeBackupEvaluationMeta(tx, recoveryActivationKey+"_revoked", result); e != nil {
					return e
				}
				return storeBackupEvaluationMeta(tx, "notification_owner", result.Before)
			}); e != nil {
				return e
			}
			return writeRecoveryActivation(d, activationFile+".revoked", result)
		}
		var revoked bool
		if e = state.db.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key=?)", recoveryActivationKey+"_revoked").Scan(&revoked); e != nil {
			return e
		}
		if revoked {
			return fmt.Errorf("recovered report generation is permanently revoked")
		}
		if f, e := d.Open(activationFile+".revoked", os.O_RDONLY, 0); e == nil {
			f.Close()
			return fmt.Errorf("recovered activation is revoked")
		} else if !os.IsNotExist(e) {
			return e
		}
		if e = writeRecoveryActivation(d, activationFile, result); e != nil {
			return e
		}
		if e = boundary("recovery-activation-intent"); e != nil {
			return e
		}
		if e = state.put(recoveryActivationKey, result); e != nil {
			return e
		}
		if e = boundary("recovery-activation-sql"); e != nil {
			return e
		}
		if e = deps.Services.InstallReports(ctx, units); e != nil {
			return e
		}
		if e = session.Activate(); e != nil {
			return e
		}
		if e = boundary("recovery-activation-active"); e != nil {
			return e
		}
		if e = writeRecoveryActivation(d, activationFile+".complete", result); e != nil {
			return e
		}
		if e = boundary("recovery-activation-file"); e != nil {
			return e
		}
		if e = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			if e := storeBackupEvaluationMeta(tx, recoveryActivationKey+"_complete", result); e != nil {
				return e
			}
			return storeBackupEvaluationMeta(tx, "notification_owner", result.After)
		}); e != nil {
			return e
		}
		return boundary("recovery-activation-shared")
	})
	if err != nil || revoke {
		return err
	}
	if err = deps.Verify(ctx, result.After); err != nil {
		return err
	}
	return deps.Services.StartReports(ctx, units)
}

func writeRecoveryActivation(d *safefs.Directory, name string, want backupRecoveryActivation) error {
	var got backupRecoveryActivation
	err := d.ReadJSON(name, &got)
	if os.IsNotExist(err) {
		return d.WriteJSON(name, want, false)
	}
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("recovery activation file changed")
	}
	return nil
}
