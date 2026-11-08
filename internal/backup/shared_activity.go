package backup

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"path/filepath"
)

// SharedActivityRequest pins the outer coordinator and original gateway receipt.
// It authorizes no producer enrollment, reporting handoff or command admission.
type SharedActivityRequest struct {
	Operation     string `json:"operation"`
	GatewayRoot   string `json:"gateway_root"`
	BeforeGateway string `json:"before_gateway"`
}

func (r SharedActivityRequest) validate() error {
	if !safefs.ValidID(r.Operation, "n") || !backupRecoveryPath(r.GatewayRoot) || !safefs.DigestPattern.MatchString(r.BeforeGateway) {
		return fmt.Errorf("shared activity requires exact coordinator and gateway pins")
	}
	return nil
}

// ReconcileSharedActivity fences capture and reports and verifies every owned
// unit has stopped before calling the gateway. Failed callbacks leave the durable
// execution fence closed. Restoring only stages capture; it never starts timers.
func ReconcileSharedActivity(ctx context.Context, home string, desired config.DesiredWorkers, helper string, r SharedActivityRequest, gateway func() error) (ActivityResult, error) {
	if gateway == nil {
		return ActivityResult{}, fmt.Errorf("shared activity requires gateway coordination")
	}
	return reconcileDeclaredActivity(ctx, home, desired, helper, backupActivityDependencies{inspect: inspectBackupActivityUnits, stop: stopBackupActivityUnits, shared: &r, gateway: gateway})
}

// ResumeSharedActivityReports resumes only the retained report units after the
// exact gateway renewal. No cutover, legacy sender, source or capture timer changes.
func ResumeSharedActivityReports(ctx context.Context, home, operation, gateway string) error {
	if !backupRecoveryPath(home) || !safefs.ValidID(operation, "n") || !safefs.DigestPattern.MatchString(gateway) {
		return fmt.Errorf("invalid shared activity reporting pins")
	}
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	var j WorkerSetupJournal
	if err = root.ReadJSON(backupSetupFile, &j); err != nil {
		return err
	}
	a := j.Activity
	if a == nil || a.validate() != nil || a.Version != 2 || a.Operation != operation || !a.Complete || a.Disabled || a.Setup != j.RequestHash {
		return fmt.Errorf("shared reporting restoration lost its activity receipt")
	}
	if _, err = inspectSetupLocked(ctx, root, home); err != nil {
		return err
	}
	state, err := openInstalledBackupState(home, false)
	if err != nil {
		return err
	}
	defer state.Close()
	if err = requireCommandScopeOwner(state, j); err != nil {
		return err
	}
	owner, err := currentSharedBackupOwner(state)
	if err != nil {
		return err
	}
	profile, err := install.InspectManagedReportProfile(a.Shared.GatewayRoot, owner.Binding.Identity, owner.Binding.Producer, j.Declaration.NotificationRoute)
	if err != nil {
		return err
	}
	if profile.ManifestHash != gateway {
		return fmt.Errorf("shared reporting restoration gateway changed")
	}
	if err = verifyBackupCutoverGateway(ctx, *owner); err != nil {
		return err
	}
	c, err := loadInstalledWorker(home)
	if err != nil {
		return err
	}
	paths := backupCutoverPaths(home, c)
	units, err := renderBackupReportUnits(paths, c.Policy, profile.Credentials)
	if err != nil {
		return err
	}
	layout, err := declarationPaths(home, j.Declaration)
	if err != nil {
		return err
	}
	for name, body := range units {
		path := layout.OwnedPath(filepath.Join(paths.unitDirectory(), name))
		actual, e := readBackupOwnedFile(root, path, 1<<20)
		if e != nil || string(actual) != body || j.OwnedFiles[path] != sharedSetupDigest(actual) {
			return fmt.Errorf("shared activity report unit ownership changed")
		}
	}
	manager := &backupCutoverManager{home: home, paths: paths, freshShared: true}
	if err = manager.requireNoDirectSenders(ctx); err != nil {
		return err
	}
	// A lost start reply may leave both triggers enabled. Confirm their complete
	// profile before acknowledging; do not start another immediate report then.
	active := true
	for name := range units {
		if e := manager.inspect(ctx, name, filepath.Join(paths.unitDirectory(), name), false, name != "deploy-backup-report.service"); e != nil {
			active = false
			break
		}
	}
	if !active {
		if err = manager.StartReports(ctx, units); err != nil {
			return err
		}
	}
	return nil
}

func reviewSharedActivityOwner(s *backupState, j WorkerSetupJournal) error {
	var present int
	if err := s.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_owner'").Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return requirePendingSharedSetup(s, &j)
	}
	if err := requireSharedActivityOwner(s, j); err != nil {
		return err
	}
	if j.Shared == nil {
		return fmt.Errorf("apply the unchanged backup declaration to capture its owned reporting units before shared activity")
	}
	units, err := backupActivityUnits(j)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, unit := range units {
		names[unit.Name] = true
	}
	for _, name := range []string{"deploy-backup-report.timer", "deploy-backup-report.path", "deploy-backup-report.service"} {
		if !names[name] {
			return fmt.Errorf("shared activity requires its complete owned reporting unit inventory")
		}
	}
	return nil
}

// ReviewSharedActivity proves dependency ownership without retaining an intent or
// stopping units. The authoritative transition repeats these checks under its lease.
func ReviewSharedActivity(ctx context.Context, home string, desired config.DesiredWorkers, helper, gatewayRoot string) error {
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", false, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	observed, err := inspectSetupLocked(ctx, root, home)
	if err != nil {
		return err
	}
	full, err := desired.Fingerprint()
	if err != nil {
		return err
	}
	if observed.Stage != "complete" || observed.DesiredHash != full || observed.HelperSHA256 != helper {
		return fmt.Errorf("shared activity requires its exact completed backup policy and helper")
	}
	var j WorkerSetupJournal
	if err = root.ReadJSON(backupSetupFile, &j); err != nil {
		return err
	}
	if j.Declaration == nil || j.Declaration.NotificationRoot != gatewayRoot || j.NotificationScope != nil || j.SourceExpansion != nil || j.SourceRetirement != nil || j.RecoveryNeeded || j.Updating {
		return fmt.Errorf("shared activity requires completed unrecovered backup dependencies")
	}
	state, err := openInstalledBackupState(home, false)
	if err != nil {
		return err
	}
	defer state.Close()
	if err = requireNoPendingDeclaredActivation(state); err != nil {
		return err
	}
	if err = reviewSharedActivityOwner(state, j); err != nil {
		return err
	}
	units, err := backupActivityUnits(j)
	if err != nil {
		return err
	}
	_, err = inspectBackupActivityUnits(ctx, home, units)
	return err
}

// Initial delivery can capture report units after an earlier V0 activity cycle
// without changing setup policy. Only the three newly owned report units may be
// added to that completed, enabled capture-unit receipt; the original SQL cutover
// and captured profile are checked by setup inspection as independent authority.
func sharedActivityCapturedReportUnits(tx *sql.Tx, j WorkerSetupJournal, a declaredBackupActivity, units []backupActivityUnit) bool {
	if a.Version != 2 || !a.Complete || a.Disabled || len(a.Units) < 6 || len(units) != len(a.Units)+3 || j.Shared == nil {
		return false
	}
	old := map[string]backupActivityUnit{}
	for _, u := range a.Units {
		old[u.Name] = u
	}
	added := map[string]bool{"deploy-backup-report.timer": false, "deploy-backup-report.path": false, "deploy-backup-report.service": false}
	for _, u := range units {
		if before, ok := old[u.Name]; ok {
			if before != u {
				return false
			}
		} else {
			if _, ok = added[u.Name]; !ok {
				return false
			}
			added[u.Name] = true
		}
	}
	for _, found := range added {
		if !found {
			return false
		}
	}
	owner, err := readBackupNotificationOwner(tx)
	if err != nil || owner != j.Shared.Owner {
		return false
	}
	var completed string
	return backupEvaluationMeta(tx, "notification_cutover_complete", &completed) == nil && completed == owner.CutoverHash
}

func requireSharedActivityRun(s *backupState, run WorkerRun) error {
	var a declaredBackupActivity
	err := s.get("declared_activity", &a)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if a.Version != 2 {
		return nil
	}
	if a.validate() != nil || !a.Complete || a.Disabled || a.CreatedAt == nil || run.StartedAt.Before(*a.CreatedAt) {
		return fmt.Errorf("shared activity restoration requires a new successful manual backup before capture activation")
	}
	return nil
}

func verifyBackupActivityHistory(root *safefs.Directory, a declaredBackupActivity) error {
	for depth := 0; depth < 128; depth++ {
		if err := a.validate(); err != nil {
			return err
		}
		if a.Previous == "" {
			return nil
		}
		var prior declaredBackupActivity
		if err := root.ReadJSON(".local/state/deploy/backup-worker/activity-history/"+a.Previous+".json", &prior); err != nil {
			return err
		}
		if !prior.Complete || notify.Hash(prior) != a.Previous {
			return fmt.Errorf("backup activity predecessor changed")
		}
		a = prior
	}
	return fmt.Errorf("backup activity history exceeds review bound")
}
