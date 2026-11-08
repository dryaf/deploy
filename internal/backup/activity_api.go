package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const backupSetupFile = ".local/state/deploy/backup-worker/setup.json"

// The setup journal carries the durable intent; the database carries the
// execution fence and completion. Missing either authority is never adoption.
type declaredBackupActivity struct {
	CreatedAt   *time.Time             `json:"created_at,omitempty"`
	Shared      *SharedActivityRequest `json:"shared,omitempty"`
	Version     int                    `json:"version"`
	Operation   string                 `json:"operation"`
	Previous    string                 `json:"previous,omitempty"`
	Setup       string                 `json:"setup"`
	Runtime     string                 `json:"runtime"`
	Helper      string                 `json:"helper"`
	DesiredHash string                 `json:"desired_hash"`
	Desired     config.DesiredWorkers  `json:"desired"`
	Disabled    bool                   `json:"disabled"`
	Complete    bool                   `json:"complete"`
	Units       []backupActivityUnit   `json:"units"`
}
type backupActivityUnit struct {
	Name       string `json:"name"`
	Source     string `json:"source"`
	Generated  bool   `json:"generated"`
	Activation bool   `json:"activation"`
}
type ActivityResult struct {
	Handled   bool
	State     string
	Unchanged bool
}

type ActivityPendingError struct{ Cause error }

func (e ActivityPendingError) Error() string { return e.Cause.Error() }
func (e ActivityPendingError) Unwrap() error { return e.Cause }

type backupActivityDependencies struct {
	setup    func(context.Context, *safefs.Directory, string) (SetupObservation, error)
	inspect  func(context.Context, string, []backupActivityUnit) (bool, error)
	stop     func(context.Context, string, []backupActivityUnit) error
	boundary func(string) error
	shared   *SharedActivityRequest
	gateway  func() error
}

func (a declaredBackupActivity) validate() error {
	h, err := a.Desired.Fingerprint()
	if err != nil || (a.Version != 1 && a.Version != 2) || !safefs.ValidID(a.Operation, "n") || a.Desired.Backup == nil || h != a.DesiredHash || len(a.Units) == 0 {
		return fmt.Errorf("invalid backup activity authority")
	}
	if a.Version == 1 {
		if a.Shared != nil || a.Disabled == config.WorkerEnabled(a.Desired.Backup.Enabled) {
			return fmt.Errorf("invalid standalone activity")
		}
	} else if a.CreatedAt == nil || a.CreatedAt.IsZero() || a.CreatedAt.Location() != time.UTC || a.Shared == nil || a.Shared.validate() != nil || a.Operation != a.Shared.Operation || a.Disabled != config.SharedActivityDisabled(a.Desired) {
		return fmt.Errorf("invalid shared backup activity")
	}
	for _, h := range []string{a.Setup, a.Runtime, a.Helper, a.DesiredHash} {
		if !safefs.DigestPattern.MatchString(h) {
			return fmt.Errorf("invalid backup activity pin")
		}
	}
	if a.Previous != "" && !safefs.DigestPattern.MatchString(a.Previous) {
		return fmt.Errorf("invalid backup activity predecessor")
	}
	return nil
}

func activityPolicy(j WorkerSetupJournal) *config.DesiredWorkers {
	if j.Activity != nil && j.Activity.Setup == j.RequestHash {
		return &j.Activity.Desired
	}
	if j.Declaration == nil {
		return nil
	}
	return j.Declaration.Desired
}

// Called with the setup lease and a read-only database transaction. A prepared
// file may precede its database fence, or a committed database may precede the
// final file acknowledgement. Only those exact adjacent states are resumable.
func inspectBackupActivity(tx *sql.Tx, j WorkerSetupJournal, c WorkerInstallation, observed *SetupObservation) error {
	var db declaredBackupActivity
	err := backupEvaluationMeta(tx, "declared_activity", &db)
	if j.Activity == nil {
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("backup activity database has no setup authority")
	}
	a := *j.Activity
	if err := a.validate(); err != nil {
		return err
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows {
		if a.Complete || a.Previous != "" {
			return fmt.Errorf("backup activity fence disappeared")
		}
	} else {
		if err := db.validate(); err != nil {
			return err
		}
		ack := a
		ack.Complete = true
		matched := notify.Hash(db) == notify.Hash(a)
		if !a.Complete {
			matched = matched || notify.Hash(db) == notify.Hash(ack) || a.Previous != "" && notify.Hash(db) == a.Previous && db.Complete
		}
		if !matched {
			return fmt.Errorf("backup activity database differs from retained intent")
		}
	}
	if a.Setup != j.RequestHash {
		if !a.Complete || a.Disabled {
			return fmt.Errorf("pending or disabled backup activity was retargeted by setup")
		}
		return nil // completed enabled activity retained as history after an update
	}
	units, e := backupActivityUnits(j)
	if e != nil {
		return e
	}
	if a.Runtime != notify.Hash(c) || a.Helper != j.Declaration.HelperSHA256 || !reflect.DeepEqual(a.Units, units) && !sharedActivityCapturedReportUnits(tx, j, a, units) {
		return fmt.Errorf("backup activity installation pins changed (runtime %t, helper %t, owned units %d, retained units %d)", a.Runtime == notify.Hash(c), a.Helper == j.Declaration.HelperSHA256, len(units), len(a.Units))
	}
	observed.DesiredHash = a.DesiredHash
	if !a.Complete {
		if db.Operation == a.Operation && !db.Complete {
			var mode string
			if err := backupEvaluationMeta(tx, "mode", &mode); err != nil {
				return err
			}
			if mode != "maintenance" {
				return fmt.Errorf("pending backup activity lost execution fence")
			}
		}
		observed.Stage = "activity_pending"
		return nil
	}
	if a.Disabled {
		var mode string
		if err = backupEvaluationMeta(tx, "mode", &mode); err != nil {
			return err
		}
		if mode != "maintenance" || observed.Installation != "disabled" {
			return fmt.Errorf("disabled backup execution fence changed")
		}
	}
	return nil
}

func backupActivityUnits(j WorkerSetupJournal) ([]backupActivityUnit, error) {
	var units []backupActivityUnit
	unitDirectory, quadletDirectory := ".config/systemd/user", ".config/containers/systemd"
	if j.Declaration != nil && j.Declaration.Paths != nil {
		p, err := declarationPaths(j.Declaration.Home, j.Declaration)
		if err != nil {
			return nil, err
		}
		unitDirectory, quadletDirectory = p.OwnedPath(p.UnitDirectory), p.OwnedPath(p.QuadletDirectory)
	}
	seen := map[string]bool{}
	for path := range j.OwnedFiles {
		var u backupActivityUnit
		switch filepath.Dir(path) {
		case quadletDirectory:
			if filepath.Ext(path) != ".container" {
				return nil, fmt.Errorf("unsupported owned backup container unit")
			}
			u.Name, u.Generated = strings.TrimSuffix(filepath.Base(path), ".container")+".service", true
		case unitDirectory:
			u.Name = filepath.Base(path)
			u.Activation = strings.HasSuffix(u.Name, ".timer") || strings.HasSuffix(u.Name, ".path")
			if !u.Activation && !strings.HasSuffix(u.Name, ".service") {
				return nil, fmt.Errorf("unsupported owned backup unit")
			}
		default:
			continue
		}
		if !strings.HasPrefix(u.Name, "deploy-backup") || seen[u.Name] {
			return nil, fmt.Errorf("ambiguous backup unit ownership")
		}
		u.Source, seen[u.Name] = path, true
		units = append(units, u)
	}
	for _, name := range []string{"deploy-backup.timer", "deploy-backup.service", "deploy-backup-health.timer", "deploy-backup-health.service", "deploy-backup-maintenance.timer", "deploy-backup-maintenance.service"} {
		if !seen[name] {
			return nil, fmt.Errorf("backup activity requires complete owned unit inventory")
		}
	}
	sort.Slice(units, func(i, j int) bool {
		if units[i].Activation != units[j].Activation {
			return units[i].Activation
		}
		return units[i].Name < units[j].Name
	})
	return units, nil
}

func ReconcileDeclaredActivity(ctx context.Context, home string, desired config.DesiredWorkers, helper string) (ActivityResult, error) {
	return reconcileDeclaredActivity(ctx, home, desired, helper, backupActivityDependencies{inspect: inspectBackupActivityUnits, stop: stopBackupActivityUnits})
}

func reconcileDeclaredActivity(ctx context.Context, home string, desired config.DesiredWorkers, helper string, deps backupActivityDependencies) (out ActivityResult, err error) {
	if !backupRecoveryPath(home) || desired.Backup == nil || !safefs.DigestPattern.MatchString(helper) {
		return out, fmt.Errorf("backup activity requires explicit target and policy")
	}
	hash, err := desired.Fingerprint()
	if err != nil {
		return out, err
	}
	disabled := !config.WorkerEnabled(desired.Backup.Enabled)
	if deps.shared != nil {
		if err = deps.shared.validate(); err != nil {
			return out, err
		}
		disabled = config.SharedActivityDisabled(desired)
	}
	root, err := safefs.Open(home)
	if err != nil {
		return out, err
	}
	defer root.Close()
	var j WorkerSetupJournal
	err = root.ReadJSON(backupSetupFile, &j)
	if errors.Is(err, os.ErrNotExist) {
		layout, e := ResolveInstallationPaths(home, os.Getenv("XDG_CONFIG_HOME"), os.Getenv("XDG_STATE_HOME"), os.Getenv("XDG_DATA_HOME"), desired.Backup.Paths)
		if e != nil {
			return out, e
		}
		declaration := &SetupDeclaration{Version: 2, Home: home, Desired: &desired, DesiredHash: hash, Paths: &layout}
		if err = checkDeclarativeSetupReservation(root, declaration); err != nil {
			return out, err
		}
		if disabled {
			return ActivityResult{Handled: true, State: "not_installed"}, nil
		}
		return out, nil
	}
	if err != nil {
		return out, err
	}
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if err != nil {
		return out, err
	}
	defer lock.Close()
	if err = root.ReadJSON(backupSetupFile, &j); err != nil {
		return out, err
	}
	defer func() {
		if err != nil && j.Activity != nil && !j.Activity.Complete {
			err = ActivityPendingError{Cause: err}
		}
	}()
	if deps.shared == nil && !disabled && (j.Activity == nil || j.Activity.Version == 2 && j.Activity.Complete && !j.Activity.Disabled) {
		return out, nil
	}
	if !disabled && j.Activity != nil && j.Activity.Complete && !j.Activity.Disabled && (j.Activity.Setup != j.RequestHash || j.Activity.DesiredHash != hash || j.Activity.Helper != helper) {
		return out, nil
	}
	out.Handled = true
	inspectSetup := deps.setup
	if inspectSetup == nil {
		inspectSetup = inspectSetupLocked
	}
	observed, err := inspectSetup(ctx, root, home)
	if err != nil {
		return out, fmt.Errorf("inspect backup before activity: %w", err)
	}
	if j.Declaration == nil || j.Declaration.HelperSHA256 != helper || (observed.Stage != "complete" && observed.Stage != "activity_pending") || j.Declaration.Desired == nil || j.Updating || j.RecoveryNeeded {
		return out, fmt.Errorf("backup activity requires unchanged completed declarative setup")
	}
	var pending workerLifecycleJournal
	if e := root.ReadJSON(".local/state/deploy/backup-worker/lifecycle.json", &pending); e == nil && pending.Stage != "complete" {
		return out, fmt.Errorf("finish the pending backup lifecycle before changing activity")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return out, e
	}
	layout, e := declarationPaths(home, j.Declaration)
	if e != nil {
		return out, e
	}
	resolved, e := ResolveInstallationPaths(home, os.Getenv("XDG_CONFIG_HOME"), os.Getenv("XDG_STATE_HOME"), os.Getenv("XDG_DATA_HOME"), desired.Backup.Paths)
	if e != nil {
		return out, e
	}
	if !reflect.DeepEqual(layout, resolved) {
		return out, fmt.Errorf("backup activity cannot change installation paths; restore the installed declaration and target XDG roots")
	}
	var c WorkerInstallation
	if err = readInstalledWorkerJSON(root, &c); err != nil {
		return out, err
	}
	if deps.shared == nil && (c.Policy.Notifications.Mode == "shared" || j.Producer != nil || j.Shared != nil || j.Declaration.NotificationRoot != "") {
		return out, fmt.Errorf("shared backup activity requires coordinated producer fencing")
	}
	s, err := openInstalledBackupState(home, false)
	if err != nil {
		return out, err
	}
	defer s.Close()
	if err = requireNoPendingDeclaredActivation(s); err != nil {
		return out, err
	}
	if deps.shared != nil {
		if c.Policy.Notifications.Mode != "shared" || j.Declaration.NotificationRoot != deps.shared.GatewayRoot || j.NotificationScope != nil || j.SourceExpansion != nil || j.SourceRetirement != nil {
			return out, fmt.Errorf("shared activity requires unchanged completed shared setup")
		}
		if err = reviewSharedActivityOwner(s, j); err != nil {
			return out, err
		}
	} else if owner, e := currentSharedBackupOwner(s); e != nil || owner != nil {
		return out, fmt.Errorf("backup notification ownership requires activity migration")
	}
	units, err := backupActivityUnits(j)
	if err != nil {
		return out, err
	}
	a := j.Activity
	retry := a != nil && a.Setup == j.RequestHash && a.DesiredHash == hash
	if retry && (deps.shared == nil) != (a.Shared == nil) {
		return out, fmt.Errorf("activity family changed")
	}
	if retry && deps.shared != nil && !reflect.DeepEqual(a.Shared, deps.shared) {
		return out, fmt.Errorf("shared activity operation changed")
	}
	if !retry {
		if a != nil && !a.Complete {
			return out, fmt.Errorf("resume pending backup activity with its original YAML")
		}
		before := activityPolicy(j)
		if before == nil || before.Backup == nil {
			return out, fmt.Errorf("backup activity policy history missing")
		}
		if deps.shared != nil {
			if e := config.ValidateSharedActivityDeclaration(*before, desired); e != nil {
				return out, e
			}
			if !disabled && (a == nil || a.Version != 2 || !a.Complete || !a.Disabled || a.Setup != j.RequestHash) {
				return out, fmt.Errorf("shared restoration requires retained YAML disable authority")
			}
		} else {
			next := *before
			b := *before.Backup
			next.Backup = &b
			b.Enabled = desired.Backup.Enabled
			nextHash, e := next.Fingerprint()
			if e != nil || nextHash != hash || config.WorkerEnabled(before.Backup.Enabled) == !disabled {
				return out, fmt.Errorf("apply backup_worker.enabled separately from other policy changes")
			}
			if !disabled && (a == nil || !a.Complete || !a.Disabled || a.Setup != j.RequestHash) {
				return out, fmt.Errorf("re-enable requires retained YAML disable authority")
			}
		}
		var mode string
		if err = s.get("mode", &mode); err != nil {
			return out, err
		}
		if (a == nil || !a.Disabled) && mode != "writer" || a != nil && a.Disabled && mode != "maintenance" {
			return out, fmt.Errorf("backup execution mode requires recovery before activity change")
		}
		// Preflight the entire owned set before retaining a new stop intent.
		if _, err = deps.inspect(ctx, home, units); err != nil {
			return out, err
		}
		id, e := safefs.NewID("n")
		if e != nil {
			return out, e
		}
		if deps.shared != nil {
			id = deps.shared.Operation
		}
		nextActivity := declaredBackupActivity{Version: 1, Operation: id, Setup: j.RequestHash, Runtime: notify.Hash(c), Helper: helper, DesiredHash: hash, Desired: desired, Disabled: disabled, Units: units}
		if deps.shared != nil {
			created := time.Now().UTC()
			nextActivity.CreatedAt = &created
			nextActivity.Version = 2
			nextActivity.Shared = deps.shared
		}
		if a != nil {
			nextActivity.Previous = notify.Hash(*a)
			if err = root.Mkdir(".local/state/deploy/backup-worker/activity-history"); err != nil {
				return out, err
			}
			name := ".local/state/deploy/backup-worker/activity-history/" + nextActivity.Previous + ".json"
			var prior declaredBackupActivity
			if e = root.ReadJSON(name, &prior); errors.Is(e, os.ErrNotExist) {
				e = root.WriteJSON(name, *a, false)
			} else if e == nil && notify.Hash(prior) != nextActivity.Previous {
				e = fmt.Errorf("backup activity history changed")
			}
			if e != nil {
				return out, e
			}
		}
		j.Activity = &nextActivity
		a = j.Activity
		if err = root.WriteJSON(backupSetupFile, j, true); err != nil {
			return out, err
		}
		if deps.boundary != nil {
			if err = deps.boundary("intent"); err != nil {
				return out, err
			}
		}
	}
	if a.Previous != "" {
		var prior declaredBackupActivity
		if err = root.ReadJSON(".local/state/deploy/backup-worker/activity-history/"+a.Previous+".json", &prior); err != nil {
			return out, err
		}
		if prior.validate() != nil || !prior.Complete || notify.Hash(prior) != a.Previous {
			return out, fmt.Errorf("backup activity predecessor unavailable")
		}
	}
	if a.Complete {
		if disabled {
			stopped, e := deps.inspect(ctx, home, units)
			if e != nil {
				return out, e
			}
			if !stopped {
				return out, fmt.Errorf("disabled backup units restarted outside declared activity")
			}
		}
		return ActivityResult{Handled: true, State: observed.Installation, Unchanged: true}, nil
	}
	var db declaredBackupActivity
	err = s.get("declared_activity", &db)
	if err != nil && err != sql.ErrNoRows {
		return out, err
	}
	ack := *a
	ack.Complete = true
	if err == nil && notify.Hash(db) == notify.Hash(ack) {
		// The canonical completion committed before the outer acknowledgement.
		j.Activity = &ack
		if err = root.WriteJSON(backupSetupFile, j, true); err != nil {
			return out, err
		}
		return ActivityResult{Handled: true, State: activityInstallation(disabled)}, nil
	}
	if err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		if a.Version == 2 {
			if e := storeBackupEvaluationMeta(tx, "shared_activity_required", true); e != nil {
				return e
			}
		}
		if e := storeBackupEvaluationMeta(tx, "declared_activity", *a); e != nil {
			return e
		}
		return storeBackupEvaluationMeta(tx, "mode", "maintenance")
	}); err != nil {
		return out, err
	}
	if deps.boundary != nil {
		if err = deps.boundary("fenced"); err != nil {
			return out, err
		}
	}
	stopped, err := deps.inspect(ctx, home, units)
	if err != nil {
		return out, err
	}
	if !stopped {
		if err = deps.stop(ctx, home, units); err != nil {
			return out, err
		}
	}
	if stopped, err = deps.inspect(ctx, home, units); err != nil {
		return out, err
	} else if !stopped {
		return out, fmt.Errorf("backup unit stop remains unconfirmed; resume the same YAML")
	}
	if deps.boundary != nil {
		if err = deps.boundary("stopped"); err != nil {
			return out, err
		}
	}
	if deps.gateway != nil {
		if err = deps.gateway(); err != nil {
			return out, err
		}
	}
	if deps.boundary != nil {
		if err = deps.boundary("gateway"); err != nil {
			return out, err
		}
	}
	if err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		if !disabled {
			var activation declaredActivation
			if e := backupEvaluationMeta(tx, "declared_activation", &activation); e == nil {
				if !activation.Complete {
					return fmt.Errorf("backup activation changed during activity")
				}
				if e = storeBackupEvaluationMeta(tx, "declared_activation_history/"+a.Operation, activation); e != nil {
					return e
				}
				if _, e = tx.Exec("DELETE FROM meta WHERE key='declared_activation'"); e != nil {
					return e
				}
			} else if e != sql.ErrNoRows {
				return e
			}
			if e := storeBackupEvaluationMeta(tx, "mode", "writer"); e != nil {
				return e
			}
		}
		if e := storeBackupEvaluationMeta(tx, "installation", activityInstallation(disabled)); e != nil {
			return e
		}
		return storeBackupEvaluationMeta(tx, "declared_activity", ack)
	}); err != nil {
		return out, err
	}
	if deps.boundary != nil {
		if err = deps.boundary("committed"); err != nil {
			return out, err
		}
	}
	j.Activity = &ack
	if err = root.WriteJSON(backupSetupFile, j, true); err != nil {
		return out, err
	}
	if deps.boundary != nil {
		if err = deps.boundary("complete"); err != nil {
			return out, err
		}
	}
	return ActivityResult{Handled: true, State: activityInstallation(disabled)}, nil
}

func activityInstallation(disabled bool) string {
	if disabled {
		return "disabled"
	}
	return "staged-awaiting-qualification"
}

func requireBackupActivityReady(s *backupState) error {
	var a declaredBackupActivity
	err := s.get("declared_activity", &a)
	if err == sql.ErrNoRows {
		var required bool
		if e := s.get("shared_activity_required", &required); e != sql.ErrNoRows {
			return fmt.Errorf("shared backup activity fence disappeared")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if a.validate() != nil || !a.Complete || a.Disabled {
		return fmt.Errorf("backup is disabled or its YAML activity transition is pending")
	}
	return nil
}
