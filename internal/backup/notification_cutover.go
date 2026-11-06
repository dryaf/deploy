package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const backupCutoverFile = "notification-cutover.json"

type backupCutoverSpec struct {
	Version      int                              `json:"version"`
	Managed      install.ManagedProducerRequest   `json:"managed"`
	HelperHash   string                           `json:"helper_hash"`
	ConfigHash   string                           `json:"config_hash"`
	EvidenceHash string                           `json:"evidence_hash"`
	Decisions    map[string]backupCutoverDecision `json:"decisions"`
}
type backupCutoverIntent struct {
	Spec     backupCutoverSpec     `json:"spec"`
	Evidence backupCutoverEvidence `json:"evidence"`
}
type backupCutoverServices interface {
	Prepare(context.Context, WorkerInstallation, WorkerUnitPaths, string) error
	StopLegacy(context.Context) error
	InstallReports(context.Context, map[string]string) error
	StartReports(context.Context, map[string]string) error
	StopReports(context.Context, map[string]string) error
}
type backupCutoverDependencies struct {
	Services backupCutoverServices
	Managed  func(context.Context, install.ManagedProducerRequest, func(install.ManagedProducerSession) error) error
	Verify   func(context.Context, backupNotificationOwner) error
	Fault    func(string) error
}

func mustCutoverJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func backupCutoverPaths(home string, c WorkerInstallation) WorkerUnitPaths {
	return WorkerUnitPaths{Home: home, Config: filepath.Join(home, ".config/deploy/backup-worker/runtime.json"), State: filepath.Join(home, ".local/state/deploy/backup-worker/state"), Archive: c.Policy.Storage.Path, Keys: filepath.Join(home, ".config/deploy/backup-worker/ssh"), RelayState: filepath.Join(home, ".local/share/deploy/backup-worker/signal"), RelaySocket: filepath.Join(home, ".local/state/deploy/backup-worker/relay")}
}
func validateBackupCutoverSpec(spec backupCutoverSpec, c WorkerInstallation) error {
	if spec.Managed.Validate() != nil || spec.Version != 1 || spec.Managed.Revoke || !safefs.DigestPattern.MatchString(spec.HelperHash) || !safefs.DigestPattern.MatchString(spec.EvidenceHash) || spec.ConfigHash != notify.Hash(c) || spec.Managed.Producer.ID != c.WorkerID {
		return fmt.Errorf("cutover requires exact helper, installation, evidence and producer pins")
	}
	if c.Policy.Notifications.Signal != nil || c.Policy.Notifications.Primary != "telegram" || c.Policy.Notifications.Fallback != "" {
		return fmt.Errorf("backup cutover currently requires Telegram only; Signal account/relay handoff is not supported")
	}
	return nil
}

// The setup lease excludes legacy installer recovery. The notifier and attempt
// leases drain both scheduled passes and direct compatible senders. Worker and
// installation leases remain held until the shared owner is durably published.
func runBackupCutover(ctx context.Context, home string, c WorkerInstallation, spec backupCutoverSpec, revoke bool, deps backupCutoverDependencies) error {
	if err := validateBackupCutoverSpec(spec, c); err != nil {
		return err
	}
	paths := backupCutoverPaths(home, c)
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
	var current WorkerInstallation
	if err = root.ReadJSON(".config/deploy/backup-worker/runtime.json", &current); err != nil {
		return err
	}
	if notify.Hash(current) != spec.ConfigHash {
		return fmt.Errorf("installed backup configuration changed under setup lease")
	}

	var setupJournal WorkerSetupJournal
	if err = root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &setupJournal); err != nil {
		return err
	}
	if setupJournal.Stage != "complete" || setupJournal.RecoveryNeeded || setupJournal.WorkerID != c.WorkerID || setupJournal.ArchiveID != c.ArchiveID {
		return fmt.Errorf("backup setup is unfinished or belongs to another archive")
	}
	if err = deps.Services.Prepare(ctx, c, paths, spec.HelperHash); err != nil {
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
	notifier, err := backupNotifierLease(state)
	if err != nil {
		return err
	}
	defer notifier.Close()
	attemptCheck, release, err := lockLegacyDelivery(ctx, state)
	if err != nil {
		return err
	}
	defer release()
	dir, err := safefs.Open(paths.State)
	if err != nil {
		return err
	}
	defer dir.Close()
	if f, e := dir.Open(legacyRollbackFile, os.O_RDONLY, 0); e == nil {
		f.Close()
		return fmt.Errorf("legacy restoration has its own retained lifecycle; use notification-rollback --revoke")
	} else if !os.IsNotExist(e) {
		return e
	}
	boundary := func(at string) error {
		if err := attemptCheck(); err != nil {
			return err
		}
		for name, l := range map[string]*safefs.Lock{"notifier.lock": notifier, "schema.lock": state.schemaLease} {
			if err := backupCutoverLease(dir, name, l); err != nil {
				return err
			}
		}
		if err := backupCutoverLease(root, ".local/state/deploy/locks/backup-worker-setup.lock", setup); err != nil {
			return err
		}
		if deps.Fault != nil {
			return deps.Fault(at)
		}
		return nil
	}
	var intent backupCutoverIntent
	err = dir.ReadJSON(backupCutoverFile, &intent)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	fresh := os.IsNotExist(err)
	if !fresh && (notify.Hash(intent.Spec) != notify.Hash(spec) || notify.Hash(intent.Evidence) != spec.EvidenceHash) {
		return fmt.Errorf("cutover retry changed retained intent")
	}
	if fresh && revoke {
		return fmt.Errorf("no cutover to revoke")
	}
	if fresh {
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			var exists bool
			if e := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM meta WHERE key IN ('notification_owner','notification_cutover'))").Scan(&exists); e != nil {
				return e
			}
			if exists {
				return fmt.Errorf("notification authority exists without its cutover file")
			}
			e, err := readBackupCutoverEvidence(tx)
			if err != nil {
				return err
			}
			if e.Control.ArchiveID != c.ArchiveID || e.Control.WriterID != c.WorkerID || e.Control.ConfigHash != c.ConfigHash {
				return fmt.Errorf("backup archive/configuration binding changed")
			}
			if notify.Hash(e) != spec.EvidenceHash {
				return fmt.Errorf("legacy evidence changed; review again before cutover")
			}
			if err = validateBackupCutoverDecisions(tx, e, spec.Decisions); err != nil {
				return err
			}
			intent = backupCutoverIntent{spec, e}
			return dir.WriteJSON(backupCutoverFile, intent, false)
		})
		if err != nil {
			return err
		}
	}
	if err = boundary("cutover-intent"); err != nil {
		return err
	}
	request := spec.Managed
	request.Revoke = revoke
	var owner backupNotificationOwner
	var reportUnits map[string]string
	err = deps.Managed(ctx, request, func(session install.ManagedProducerSession) error {
		m := session.Manifest()
		if m.UnitDirectory != filepath.Join(home, ".config/systemd/user") {
			return fmt.Errorf("report and gateway must use the same canonical user manager")
		}
		binding := backupNotificationBinding{Identity: m.Identity, Producer: spec.Managed.Producer, Archive: c.ArchiveID, Epoch: intent.Evidence.Control.Epoch, Generation: spec.Managed.Generation, Route: spec.Managed.Route}
		owner = backupNotificationOwner{Schema: 1, Mode: "fenced", Binding: binding, Socket: m.Paths.Socket, CutoverHash: notify.Hash(intent)}
		reportUnits, err = renderBackupReportUnits(paths, c.Policy, session.CredentialPaths())
		if err != nil {
			return err
		}
		if err = boundary("cutover-worker-leased"); err != nil {
			return err
		}
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			var old string
			err := backupNotificationMeta(tx, "notification_cutover", &old)
			if err == nil {
				if old != notify.Hash(intent) {
					return fmt.Errorf("cutover SQL/file disagreement")
				}
				current, e := readBackupNotificationOwner(tx)
				if e != nil {
					return e
				}
				if current.Binding != binding || current.Socket != owner.Socket || current.CutoverHash != owner.CutoverHash {
					return fmt.Errorf("cutover owner changed")
				}
				return nil
			}
			if err != sql.ErrNoRows {
				return err
			}
			if err = validateBackupCutoverFence(tx, intent); err != nil {
				return err
			}
			evidence := intent.Evidence
			initialized, err := backupCommandsInstalled(tx)
			if err != nil {
				return err
			}
			if !initialized {
				if err = initializeBackupCommands(tx); err != nil {
					return err
				}
			}
			if err = installBackupNotificationPipeline(tx, binding); err != nil {
				return err
			}
			if err = installBackupCutoverEvidence(tx, evidence, spec.Decisions); err != nil {
				return err
			}
			if err = storeBackupEvaluationMeta(tx, "notification_cutover", notify.Hash(intent)); err != nil {
				return err
			}
			return storeBackupEvaluationMeta(tx, "notification_owner", owner)
		})
		if err != nil {
			return err
		}
		if err = boundary("cutover-fenced"); err != nil {
			return err
		}
		if revoke {
			if err = deps.Services.StopReports(ctx, reportUnits); err != nil {
				return err
			}

			// Even an interrupted, merely staged handoff must receive a tombstone.
			if err = session.Revoke(ctx); err != nil {
				return err
			}
			if err = boundary("cutover-revoked"); err != nil {
				return err
			}
			owner.Mode = "fenced"
			if err = state.put("notification_owner", owner); err != nil {
				return err
			}
			cursor := notify.Cursor{Identity: binding.Identity, Producer: binding.Producer}
			if err = state.db.QueryRowContext(ctx, "SELECT cursor FROM notification_producer WHERE singleton=1").Scan(&cursor.Position); err != nil {
				return err
			}
			for pages := 0; pages < 10000; pages++ {
				page, e := session.Receipts(ctx, cursor)
				if e != nil {
					return e
				}
				if e = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
					return importBackupNotificationPage(tx, backupReceiptPage{Receipts: page.Receipts, Cursor: page.Cursor})
				}); e != nil {
					return e
				}
				if len(page.Receipts) == 0 {
					return state.put("notification_cutover_revoked", notify.Hash(intent))
				}
				cursor = page.Cursor
			}
			return fmt.Errorf("revocation receipt reconciliation bound reached; resume")
		}
		if err = session.Stage(); err != nil {
			return err
		}
		if err = boundary("cutover-staged"); err != nil {
			return err
		}
		if err = deps.Services.InstallReports(ctx, reportUnits); err != nil {
			return err
		}
		if err = boundary("cutover-units"); err != nil {
			return err
		}
		if err = session.Activate(); err != nil {
			return err
		}
		if err = boundary("cutover-active"); err != nil {
			return err
		}
		owner.Mode = "shared"
		if err = state.put("notification_owner", owner); err != nil {
			return err
		}
		return boundary("cutover-shared")
	})
	if err != nil {
		return err
	}
	if revoke {
		return nil
	}
	if err = deps.Verify(ctx, owner); err != nil {
		return err
	}
	if err = boundary("cutover-verified"); err != nil {
		return err
	}
	// Release the notifier before starting its replacement: the service acquires
	// this same lease, and systemctl start waits for the initial oneshot pass.
	notifier.Close()
	notifier = nil
	if err = deps.Services.StartReports(ctx, reportUnits); err != nil {
		return err
	}
	return state.put("notification_cutover_complete", notify.Hash(intent))
}
func backupCutoverLease(d *safefs.Directory, name string, l *safefs.Lock) error {
	if l == nil {
		return fmt.Errorf("cutover lease missing")
	}
	held, err := l.File().Stat()
	if err != nil {
		return err
	}
	f, err := d.Open(name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	current, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(held, current) {
		return fmt.Errorf("cutover lease replaced: %s", name)
	}
	return nil
}
