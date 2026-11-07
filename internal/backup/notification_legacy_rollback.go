package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

const legacyRollbackFile = "notification-legacy-rollback.json"

type backupLegacyRollbackSpec struct {
	Version        int               `json:"version"`
	Cutover        backupCutoverSpec `json:"cutover"`
	Generation     string            `json:"generation"`
	Reason         string            `json:"reason"`
	CredentialHash string            `json:"credential_hash"`
}
type backupLegacyRollbackIntent struct {
	Spec           backupLegacyRollbackSpec   `json:"spec"`
	Owner          backupNotificationOwner    `json:"owner"`
	Evidence       backupCutoverEvidence      `json:"evidence"`
	CanonicalState string                     `json:"canonical_state"`
	Policy         config.WorkerNotifications `json:"policy"`
}

type backupLegacyServices interface {
	PrepareLegacy(context.Context, WorkerInstallation, WorkerUnitPaths, string) error
	StartLegacy(context.Context) error
}

func (s backupLegacyRollbackSpec) validate(c WorkerInstallation) error {
	if s.Cutover.Fresh != nil || c.Policy.Notifications.Mode == "shared" {
		return fmt.Errorf("a declared shared installation has no legacy sender to restore")
	}
	if s.Version != 1 || !safefs.DigestPattern.MatchString(s.CredentialHash) || !notify.ValidToken(s.Generation) || s.Generation == s.Cutover.Managed.Generation || strings.TrimSpace(s.Reason) == "" || len(s.Reason) > 512 {
		return fmt.Errorf("legacy rollback requires a new generation and bounded audit reason")
	}
	return validateBackupCutoverSpec(s.Cutover, c)
}

// Restoration is deliberately stricter than revocation: all uncertain exported
// work must be reconciled first. It never clears evidence to make rollback pass.
func runBackupLegacyRollback(ctx context.Context, home string, c WorkerInstallation, spec backupLegacyRollbackSpec, revoke bool, deps backupCutoverDependencies) error {
	if err := spec.validate(c); err != nil {
		return err
	}
	services, ok := deps.Services.(backupLegacyServices)
	if !ok {
		return fmt.Errorf("legacy restoration service capability unavailable")
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
	if err = readInstalledWorkerJSON(root, &current); err != nil {
		return err
	}
	if notify.Hash(current) != spec.Cutover.ConfigHash {
		return fmt.Errorf("backup configuration changed")
	}
	checkCredentials := func() error {
		if revoke {
			return nil
		}
		hash, err := notification.LegacyCredentialHash(c.Policy.Notifications, spec.Cutover.signalProfile())
		if err != nil {
			return err
		}
		if hash != spec.CredentialHash {
			return fmt.Errorf("reviewed legacy provider credentials changed")
		}
		return nil
	}
	if err = checkCredentials(); err != nil {
		return err
	}
	var journal WorkerSetupJournal
	if err = root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &journal); err != nil {
		return err
	}
	if journal.Stage != "complete" || journal.RecoveryNeeded || journal.WorkerID != c.WorkerID || journal.ArchiveID != c.ArchiveID {
		return fmt.Errorf("backup setup is not complete")
	}
	if err = deps.Services.Prepare(ctx, c, paths, spec.Cutover.HelperHash); err != nil {
		return err
	}
	if spec.Cutover.Version == 4 {
		if err = requireMigrationCycleCapability(ctx, deps.Services, paths, spec.Cutover.HelperHash); err != nil {
			return err
		}
	}
	if err = deps.Services.StopLegacy(ctx); err != nil {
		return err
	}
	// This probes the native helper, installs exact owned native units and stops
	// any previously restored sender before acquiring its attempt leases.
	if err = services.PrepareLegacy(ctx, c, paths, spec.Cutover.HelperHash); err != nil {
		return err
	}
	state, err := openBackupState(paths.State, false)
	if err != nil {
		return err
	}
	defer state.Close()
	rollbackFile, err := currentMigrationArtifact(state, legacyRollbackFile)
	if err != nil {
		return err
	}
	notifier, err := backupNotifierLease(state)
	if err != nil {
		return err
	}
	defer notifier.Close()
	check, release, err := lockLegacyDelivery(ctx, state)
	if err != nil {
		return err
	}
	defer release()
	dir, err := safefs.Open(paths.State)
	if err != nil {
		return err
	}
	defer dir.Close()
	boundary := func(at string) error {
		if err := check(); err != nil {
			return err
		}
		if err := backupCutoverLease(root, ".local/state/deploy/locks/backup-worker-setup.lock", setup); err != nil {
			return err
		}
		if err := backupCutoverLease(dir, "notifier.lock", notifier); err != nil {
			return err
		}
		if err := backupCutoverLease(dir, "schema.lock", state.schemaLease); err != nil {
			return err
		}
		return backupRecoveryBoundary(deps.Fault, at)
	}
	var cutover backupCutoverIntent
	if err = dir.ReadJSON(backupCutoverFile, &cutover); err != nil {
		return err
	}
	if matchCycleCutover(state, cutover, spec.Cutover) != nil {
		return fmt.Errorf("rollback changed original cutover")
	}
	var revoked string
	if err = state.get("notification_cutover_revoked", &revoked); err != nil || revoked != notify.Hash(cutover) {
		return fmt.Errorf("revoke and reconcile shared receipts before legacy rollback")
	}
	var intent backupLegacyRollbackIntent
	err = dir.ReadJSON(rollbackFile, &intent)
	fresh := os.IsNotExist(err)
	if err != nil && !fresh {
		return err
	}
	if !fresh && notify.Hash(intent.Spec) != notify.Hash(spec) {
		return fmt.Errorf("legacy rollback retry changed intent")
	}
	if fresh && revoke {
		return fmt.Errorf("no legacy rollback to revoke")
	}
	request := spec.Cutover.Managed
	request.Revoke, request.LegacyGeneration = true, spec.Generation
	request.LegacyRevoke = revoke
	err = deps.Managed(ctx, request, func(session install.ManagedProducerSession) error {
		if err := requireBackupSignalMigration(ctx, home, c, spec.Cutover, session, revoke); err != nil {
			return err
		}
		if !revoke {
			if err := requireBackupLegacyReadablePaths(paths, session.Manifest().Paths.State); err != nil {
				return err
			}
		}
		status, err := session.Status()
		if err != nil || status.State != "revoked" || status.Active {
			return fmt.Errorf("canonical shared generation is not revoked")
		}
		units, err := renderBackupReportUnits(paths, c.Policy, session.CredentialPaths())
		if err != nil {
			return err
		}
		if err = deps.Services.StopReports(ctx, units); err != nil {
			return err
		}
		var owner backupNotificationOwner
		if err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			var err error
			owner, err = readBackupNotificationOwner(tx)
			if err != nil {
				return err
			}
			if owner.CutoverHash != notify.Hash(cutover) || owner.Binding.Generation != request.Generation || owner.Binding.Producer != request.Producer || owner.Binding.Identity != session.Manifest().Identity {
				return fmt.Errorf("rollback owner binding changed")
			}
			if owner.Mode != "fenced" && owner.Mode != "legacy" {
				return fmt.Errorf("shared reports must be revoked before restoring legacy")
			}
			if err = requireNotificationRecoveryInactive(tx); err != nil {
				return err
			}
			if fresh {
				if owner.Mode != "fenced" {
					return fmt.Errorf("legacy ownership exists without rollback intent")
				}
				if err = requireResolvedLegacyRows(tx); err != nil {
					return err
				}
				evidence, err := readBackupCutoverEvidence(tx)
				if err != nil {
					return err
				}
				intent = backupLegacyRollbackIntent{Spec: spec, Owner: owner, Evidence: evidence, CanonicalState: session.Manifest().Paths.State, Policy: c.Policy.Notifications}
			}
			if !backupRecoveryPath(intent.CanonicalState) || intent.CanonicalState != session.Manifest().Paths.State {
				return fmt.Errorf("canonical legacy state path changed")
			}
			if !fresh && (intent.Owner.Binding != owner.Binding || intent.Owner.CutoverHash != owner.CutoverHash || intent.Owner.Socket != owner.Socket || owner.LegacyHash != "" && owner.LegacyHash != notify.Hash(intent)) {
				return fmt.Errorf("retained rollback authority changed")
			}
			return nil
		}); err != nil {
			return err
		}
		if err = requireBackupCutoverOwner(state, owner); err != nil {
			return err
		}
		if revoke {
			if err = session.RevokeLegacy(); err != nil {
				return err
			}
			if err = boundary("legacy-rollback-revoked"); err != nil {
				return err
			}
			owner.Mode = "fenced"
			return backupTransferTx(ctx, state, func(tx *sql.Tx) error {
				if err := storeBackupEvaluationMeta(tx, "notification_owner", owner); err != nil {
					return err
				}
				return storeBackupEvaluationMeta(tx, "notification_legacy_rollback_revoked", notify.Hash(intent))
			})
		}
		// Exact completed retries must not reinterpret later native attempts.
		var done string
		if err = state.get("notification_legacy_rollback", &done); err == nil {
			if fresh || done != notify.Hash(intent) || owner.Mode != "legacy" || owner.LegacyGeneration != spec.Generation {
				return fmt.Errorf("legacy rollback SQL/file disagreement")
			}
			return nil
		} else if err != sql.ErrNoRows {
			return err
		}
		if !fresh && owner.Mode != "fenced" {
			return fmt.Errorf("legacy owner published without rollback commit")
		}
		if err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			if err := requireResolvedLegacyRows(tx); err != nil {
				return err
			}
			return validateBackupCutoverFence(tx, backupCutoverIntent{Evidence: intent.Evidence})
		}); err != nil {
			return err
		}
		rows, err := state.db.Query("SELECT body,accepted,eligible FROM notification_exports ORDER BY seq LIMIT 10001")
		if err != nil {
			return err
		}
		var messages []notify.Message
		var acceptedMessages []notify.Message
		count := 0
		for rows.Next() {
			count++
			var raw string
			var accepted, eligible bool
			if err = rows.Scan(&raw, &accepted, &eligible); err != nil {
				rows.Close()
				return err
			}
			if count > 10000 {
				rows.Close()
				return fmt.Errorf("rollback export bound exceeded")
			}
			var m notify.Message
			if err = notify.Decode([]byte(raw), &m, notify.MaxRequest); err != nil {
				rows.Close()
				return err
			}
			if accepted && eligible {
				acceptedMessages = append(acceptedMessages, m)
			} else {
				messages = append(messages, m)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, m := range messages {
			if err = session.RequireLegacyUnsent(ctx, m); err != nil {
				return err
			}
		}
		for _, m := range acceptedMessages {
			if err = session.RequireLegacyAccepted(ctx, m); err != nil {
				return err
			}
		}
		if fresh {
			if err = dir.WriteJSON(rollbackFile, intent, false); err != nil {
				return err
			}
		}
		if err = boundary("legacy-rollback-intent"); err != nil {
			return err
		}
		if err = session.RestoreLegacy(); err != nil {
			return err
		}
		if err = boundary("legacy-rollback-canonical"); err != nil {
			return err
		}
		if err = checkCredentials(); err != nil {
			return err
		}
		owner.Mode = "legacy"
		owner.LegacyGeneration, owner.LegacyState, owner.LegacyHash = spec.Generation, session.Manifest().Paths.State, notify.Hash(intent)
		if err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			if err := storeBackupEvaluationMeta(tx, "notification_owner", owner); err != nil {
				return err
			}
			return storeBackupEvaluationMeta(tx, "notification_legacy_rollback", notify.Hash(intent))
		}); err != nil {
			return err
		}
		return boundary("legacy-rollback-published")
	})
	if err != nil {
		return err
	}
	if revoke {
		return nil
	}
	// Native service takes both leases; systemctl start waits for its oneshot.
	notifier.Close()
	release()
	return services.StartLegacy(ctx)
}

func readRestoredLegacyOwner(state *backupState) (owner backupNotificationOwner, err error) {
	err = backupTransferTx(context.Background(), state, func(tx *sql.Tx) error { var err error; owner, err = readBackupNotificationOwner(tx); return err })
	if err != nil {
		return
	}
	if owner.Mode != "legacy" {
		return owner, fmt.Errorf("legacy delivery remains fenced")
	}
	if err = requireBackupCutoverOwner(state, owner); err != nil {
		return
	}
	d, err := safefs.Open(state.directory)
	if err != nil {
		return owner, err
	}
	defer d.Close()
	rollbackFile, err := currentMigrationArtifact(state, legacyRollbackFile)
	if err != nil {
		return owner, err
	}
	var intent backupLegacyRollbackIntent
	if err = d.ReadJSON(rollbackFile, &intent); err != nil {
		return
	}
	var committed string
	if err = state.get("notification_legacy_rollback", &committed); err != nil {
		return
	}
	if committed != owner.LegacyHash || notify.Hash(intent) != committed || intent.Owner.Binding != owner.Binding || intent.Spec.Generation != owner.LegacyGeneration || !filepath.IsAbs(owner.LegacyState) || owner.LegacyState != intent.CanonicalState {
		return owner, fmt.Errorf("legacy restoration authority changed")
	}
	hash, err := notification.LegacyCredentialHash(intent.Policy, intent.Spec.Cutover.signalProfile())
	if err != nil {
		return owner, err
	}
	if hash != intent.Spec.CredentialHash {
		return owner, fmt.Errorf("restored legacy credentials changed")
	}
	err = backupTransferTx(context.Background(), state, requireResolvedLegacyRows)
	return
}

func requireResolvedLegacyRows(tx *sql.Tx) error {
	held, err := backupHasHeldSources(tx)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("legacy delivery has unresolved held rows")
	}
	return nil
}

func acquireBackupLegacyAuthority(state *backupState, c *WorkerInstallation) (check func() error, release func(), err error) {
	if err = requireLegacyNotificationOwner(state); err == nil {
		return func() error { return requireLegacyNotificationOwner(state) }, func() {}, nil
	}
	owner, err := readRestoredLegacyOwner(state)
	if err != nil {
		return nil, nil, err
	}
	if c != nil {
		rollbackFile, err := currentMigrationArtifact(state, legacyRollbackFile)
		if err != nil {
			return nil, nil, err
		}
		var intent backupLegacyRollbackIntent
		d, err := safefs.Open(state.directory)
		if err != nil {
			return nil, nil, err
		}
		err = d.ReadJSON(rollbackFile, &intent)
		d.Close()
		if err != nil {
			return nil, nil, err
		}
		if notify.Hash(*c) != intent.Spec.Cutover.ConfigHash {
			return nil, nil, fmt.Errorf("restored legacy runtime changed")
		}
	}
	canonicalCheck, release, err := worker.AcquireLegacyAuthority(owner.LegacyState, owner.Binding.Identity, owner.Binding.Producer, owner.LegacyGeneration)
	if err != nil {
		return nil, nil, err
	}
	check = func() error {
		current, err := readRestoredLegacyOwner(state)
		if err != nil {
			return err
		}
		if current != owner {
			return fmt.Errorf("legacy authority changed during delivery")
		}
		return canonicalCheck()
	}
	return check, release, nil
}

func requireLegacyDeliveryOwner(state *backupState) error {
	check, release, err := acquireBackupLegacyAuthority(state, nil)
	if err != nil {
		return err
	}
	defer release()
	return check()
}
