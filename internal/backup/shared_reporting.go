package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// A fresh shared installation never possessed direct-provider authority. This
// proof is distinct from a reviewed migration of a historical legacy sender.
type freshBackupReporting struct {
	NotificationDesiredHash string `json:"notification_desired_hash,omitempty"`
	SetupID                 string `json:"setup_id"`
	DesiredHash             string `json:"desired_hash"`
	ReservationHash         string `json:"reservation_hash"`
}

func validateFreshBackupReportingSpec(s backupCutoverSpec, c WorkerInstallation) error {
	f := s.Fresh
	n := c.Policy.Notifications
	if (s.Version != 5 && s.Version != 6 && s.Version != 7 && s.Version != 8) || f == nil || s.Signal != nil || (s.Version == 5 && s.Predecessor != "") || n.Mode != "shared" || n.Primary != "" || n.Fallback != "" || n.Telegram != nil || n.Signal != nil || !safefs.ValidID(f.SetupID, "n") || !safefs.DigestPattern.MatchString(f.DesiredHash) || !safefs.DigestPattern.MatchString(f.ReservationHash) {
		return fmt.Errorf("fresh shared reporting requires its exact declarative reservation and no direct sender")
	}
	if f.NotificationDesiredHash != "" && !safefs.DigestPattern.MatchString(f.NotificationDesiredHash) || (s.Version == 6 || s.Version == 7 || s.Version == 8) && f.NotificationDesiredHash == "" {
		return fmt.Errorf("shared policy continuation requires its original notification scope")
	}
	return nil
}

func requireFreshBackupReportingSetup(root *safefs.Directory, home string, j WorkerSetupJournal, s backupCutoverSpec) error {
	// The original declaration remains the reporting authority. Exact scope
	// ancestry may additionally prove newer report units with expanded credential
	// exclusions; validate their current pins against disk after proving that chain.
	if j.SharedPolicyUpdate != nil || j.SharedPolicyExpansion != nil {
		return fmt.Errorf("finish pending backup changes before reporting")
	}
	owned := j.OwnedFiles
	current := j
	if err := checkSetupNotificationScope(j); err != nil {
		return err
	}
	if err := verifyNotificationScopeHistory(root, j); err != nil {
		return err
	}
	f := s.Fresh
	helperHash := s.HelperHash
	if s.Version == 7 || s.Version == 8 {
		if f == nil || j.Declaration == nil || j.Declaration.DesiredHash != s.CompletedPolicyHash || j.Shared == nil || j.Shared.Owner.CutoverHash != s.Predecessor {
			return fmt.Errorf("shared policy continuation lost current ownership")
		}
		c, err := loadInstalledWorker(home)
		if err != nil {
			return err
		}
		var original backupCutoverIntent
		dir, err := safefs.Open(backupCutoverPaths(home, c).State)
		if err != nil {
			return err
		}
		err = dir.ReadJSON(backupCutoverFile, &original)
		dir.Close()
		if err != nil {
			return err
		}
		if original.Spec.Version != 5 || notify.Hash(original) != s.Predecessor {
			return fmt.Errorf("shared policy continuation lost original reporting receipt")
		}
		helperHash = original.Spec.HelperHash
		j, _, err = reportingOrigin(root, j, c, f.DesiredHash, f.NotificationDesiredHash, original.Spec.ConfigHash, original.Spec.HelperHash)
		if err != nil {
			return err
		}
	} else if f != nil && j.Declaration != nil && j.Declaration.NotificationDesiredHash != f.NotificationDesiredHash {
		var err error
		j, err = commandScopeOrigin(root, j, f.NotificationDesiredHash)
		if err != nil {
			return err
		}
	}
	d := j.Declaration
	desired := ""
	if f != nil {
		desired = f.DesiredHash
	}
	if s.Version == 6 {
		desired = s.CompletedPolicyHash
	}
	if f == nil || d == nil || j.ID != f.SetupID || j.Updating || j.RecoveryNeeded || j.SourceExpansion != nil || j.SourceRetirement != nil || j.NotificationScope != nil || j.Stage != "complete" || d.Home != home || d.UID != os.Getuid() || d.DesiredHash != desired || d.HelperSHA256 != helperHash || d.NotificationRoot != s.Managed.Root || d.NotificationRoute != s.Managed.Route || d.NotificationDesiredHash != f.NotificationDesiredHash || j.Producer == nil || j.Producer.Validate(j.WorkerID) != nil || j.Producer.Producer != s.Managed.Producer || j.Producer.Generation != s.Managed.Generation || notify.Hash(j.Producer) != f.ReservationHash {
		return fmt.Errorf("fresh shared reporting differs from its completed setup and reserved producer")
	}
	if s.Version == 6 && (j.Shared == nil || j.Shared.Version != 1 || j.Shared.Owner.Mode != "shared" || j.Shared.Owner.CutoverHash != s.Predecessor || j.Shared.GatewayRoot != s.Managed.Root || j.Shared.Profile.ManifestHash != s.Managed.ManifestHash) {
		return fmt.Errorf("updated backup lost its captured shared ownership")
	}
	dir, err := root.Sub(".local/state/deploy/backup-worker")
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = checkSetupProducerReservation(dir, &j); err != nil {
		return err
	}
	layout, err := declarationPaths(home, d)
	if err != nil {
		return err
	}
	if j.OwnedFiles[layout.OwnedPath(layout.Runtime)] == "" {
		return fmt.Errorf("fresh shared reporting runtime receipt missing")
	}
	for path, hash := range owned {
		body, e := readBackupOwnedFile(root, path, 8<<20)
		if e != nil || sharedSetupDigest(body) != hash {
			return fmt.Errorf("fresh shared reporting installed artifacts drifted")
		}
	}
	if err := verifySetupSourceKeys(root, j); err != nil {
		return err
	}
	return verifySetupSourceKeys(root, current)
}

func requireFreshBackupReportingEvidence(tx *sql.Tx, e backupCutoverEvidence) error {
	if !e.Control.SharedRequired || e.Control.ReportingDisabled || e.Journal.Head.Sequence != 0 || e.Journal.Attempts != 0 || e.Journal.Unsettled != 0 {
		return fmt.Errorf("fresh shared reporting lost its permanent direct-sender fence or has delivery history")
	}
	for _, row := range e.Rows {
		if row.Accepted.Valid || row.Attempts != 0 {
			return fmt.Errorf("fresh shared reporting has contradictory direct delivery outcomes")
		}
		var event notification.NotificationEvent
		if err := notify.Decode([]byte(row.Record), &event, notify.MaxRequest); err != nil {
			return err
		}
		if err := requireBackupUnattemptedSource(tx, event); err != nil {
			return err
		}
	}
	return nil
}

// ConnectSharedReporting installs and activates the reserved producer's report
// service. The managed gateway must already have its own qualified delivery
// admission. This does not activate backup capture or invent qualification.
// Retries retain the original evidence/identity, including after a lost reply.
func ConnectSharedReporting(ctx context.Context, home, desiredHash, manifestHash string) error {
	return connectSharedReporting(ctx, home, desiredHash, manifestHash, backupCutoverDependencies{
		Services: &backupCutoverManager{}, Completed: verifyFreshCompletedReporting, Managed: install.WithManagedProducer, Verify: verifyBackupCutoverGateway,
	})
}

func connectSharedReporting(ctx context.Context, home, desiredHash, manifestHash string, deps backupCutoverDependencies) error {
	continuation := deps.CommandContinuation
	if continuation == nil {
		continuation = install.InspectCommandDeliveryContinuation
	}
	c, spec, err := prepareFreshSharedReportingUsing(ctx, home, desiredHash, manifestHash, continuation)
	if err != nil {
		return err
	}
	complete, err := inspectFreshReportingCompletion(ctx, home, c, spec, deps)
	if err != nil || complete {
		return err
	}
	if spec.Version == 6 || spec.Version == 7 || spec.Version == 8 || manifestHash != spec.Managed.ManifestHash {
		return fmt.Errorf("shared policy continuation lost its original completion; ownership recovery required")
	}
	return runBackupCutover(ctx, home, c, spec, false, deps)
}

// Build under the setup lease; runBackupCutover revalidates every pin and the
// evidence before publishing its immutable intent under its exclusive leases.
func prepareFreshSharedReporting(ctx context.Context, home, desiredHash, manifestHash string) (c WorkerInstallation, spec backupCutoverSpec, err error) {
	return prepareFreshSharedReportingUsing(ctx, home, desiredHash, manifestHash, install.InspectCommandDeliveryContinuation)
}

func prepareFreshSharedReportingUsing(ctx context.Context, home, desiredHash, manifestHash string, continuation func(context.Context, string, string, string) (install.ManagedManifest, error)) (c WorkerInstallation, spec backupCutoverSpec, err error) {
	if !backupRecoveryPath(home) || !safefs.DigestPattern.MatchString(desiredHash) || !safefs.DigestPattern.MatchString(manifestHash) {
		return c, spec, fmt.Errorf("shared reporting requires a target home and exact policy/manifest pins")
	}
	root, err := safefs.Open(home)
	if err != nil {
		return c, spec, err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", false, false)
	if err != nil {
		return c, spec, err
	}
	defer lock.Close()
	if err = readInstalledWorkerJSON(root, &c); err != nil {
		return c, spec, err
	}
	var j WorkerSetupJournal
	if err = root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &j); err != nil {
		return c, spec, err
	}
	if err = checkSetupNotificationScope(j); err != nil {
		return c, spec, err
	}
	if err = verifyNotificationScopeHistory(root, j); err != nil {
		return c, spec, err
	}
	if j.Declaration == nil || j.Producer == nil || j.Declaration.DesiredHash != desiredHash || c.WorkerID != j.WorkerID || c.ArchiveID != j.ArchiveID {
		return c, spec, fmt.Errorf("shared reporting requires the matching declarative installation")
	}
	dir, err := safefs.Open(backupCutoverPaths(home, c).State)
	if err != nil {
		return c, spec, err
	}
	defer dir.Close()
	reportingRuntime := c
	var retained backupCutoverIntent
	err = dir.ReadJSON(backupCutoverFile, &retained)
	if err == nil {
		spec = retained.Spec
		if spec.Version != 5 || spec.Fresh == nil || notify.Hash(retained.Evidence) != spec.EvidenceHash {
			return c, spec, fmt.Errorf("shared reporting retry changed retained admission or policy")
		}
		reportingDesired := desiredHash
		if j.SharedPolicyHistory != nil || j.Declaration.NotificationDesiredHash != spec.Fresh.NotificationDesiredHash {
			origin, projected, e := reportingOrigin(root, j, c, spec.Fresh.DesiredHash, spec.Fresh.NotificationDesiredHash, spec.ConfigHash, spec.HelperHash)
			if e != nil {
				return c, spec, e
			}
			reportingRuntime = projected
			if origin.Declaration.DesiredHash != spec.Fresh.DesiredHash || spec.ConfigHash != notify.Hash(reportingRuntime) {
				return c, spec, fmt.Errorf("command scope continuation changed original reporting policy")
			}
			reportingDesired = origin.Declaration.DesiredHash
		}
		if spec.Managed.ManifestHash != manifestHash || notify.Hash(reportingRuntime) != notify.Hash(c) {
			if spec.Fresh.DesiredHash != reportingDesired || spec.ConfigHash != notify.Hash(reportingRuntime) {
				return c, spec, fmt.Errorf("apply backup policy changes separately from command admission continuation")
			}
			if err = inspectFreshCommandContinuation(ctx, home, c, retained, manifestHash, continuation); err != nil {
				return c, spec, err
			}
		}
		if spec.Fresh.DesiredHash != reportingDesired || spec.ConfigHash != notify.Hash(reportingRuntime) {
			if j.Declaration.NotificationDesiredHash == "" || spec.Fresh.NotificationDesiredHash != j.Declaration.NotificationDesiredHash || spec.HelperHash != j.Declaration.HelperSHA256 {
				return c, spec, fmt.Errorf("shared policy update lacks the unchanged gateway scope and helper")
			}
			spec.Version = 6
			spec.Predecessor = notify.Hash(retained)
			spec.CompletedPolicyHash = desiredHash
			spec.ConfigHash = notify.Hash(c)
			reportingRuntime = c
		}
		if j.SharedPolicyHistory != nil {
			spec.Version, spec.Predecessor, spec.CompletedPolicyHash = 7, notify.Hash(retained), desiredHash
			spec.ConfigHash, reportingRuntime = notify.Hash(c), c
			if spec.HelperHash != j.Declaration.HelperSHA256 {
				spec.Version, spec.HelperHash = 8, j.Declaration.HelperSHA256
			}
		}
	} else if os.IsNotExist(err) {
		state, e := openInstalledBackupStateWithLease(home, false, false, true)
		if e != nil {
			return c, spec, e
		}
		defer state.Close()
		var evidence backupCutoverEvidence
		err = backupTransferTx(ctx, state, func(tx *sql.Tx) error {
			var e error
			evidence, e = readBackupCutoverEvidence(tx)
			if e != nil {
				return e
			}
			return requireFreshBackupReportingEvidence(tx, evidence)
		})
		if err != nil {
			return c, spec, err
		}
		spec = backupCutoverSpec{Version: 5, Fresh: &freshBackupReporting{SetupID: j.ID, DesiredHash: desiredHash, ReservationHash: notify.Hash(j.Producer), NotificationDesiredHash: j.Declaration.NotificationDesiredHash}, Managed: install.ManagedProducerRequest{Root: j.Declaration.NotificationRoot, ManifestHash: manifestHash, Producer: j.Producer.Producer, Generation: j.Producer.Generation, Route: j.Declaration.NotificationRoute}, HelperHash: j.Declaration.HelperSHA256, ConfigHash: notify.Hash(c), EvidenceHash: notify.Hash(evidence), Decisions: map[string]backupCutoverDecision{}}
		for _, row := range evidence.Rows {
			spec.Decisions[row.ID] = backupCutoverDecision{notify.Hash(row), "unsent", "permanent shared-only installation fence; no direct delivery authority or attempts"}
		}
	} else {
		return c, spec, err
	}
	if err = validateBackupCutoverSpec(spec, reportingRuntime); err != nil {
		return c, spec, err
	}
	err = requireFreshBackupReportingSetup(root, home, j, spec)
	return c, spec, err
}
