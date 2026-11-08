package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// SetupSourceTarget is the retained source address used for a read-only inventory
// comparison. It grants no enrollment, trust or source-administration authority.
type SetupSourceTarget struct {
	Alias string `json:"alias"`
	Host  string `json:"host"`
	User  string `json:"user"`
	Port  int    `json:"port"`
}

type SetupObservation struct {
	SourceTargets            []SetupSourceTarget `json:"-"` // Private planning input; addresses are not status output.
	Paths                    *InstallationPaths  `json:"-"` // Private resolved-layout comparison input.
	HelperSHA256             string              `json:"-"` // Private local planning input, not user-facing output.
	ActivationPending        bool                `json:"activation_pending,omitempty"`
	RecentSuccessfulBackup   bool                `json:"recent_successful_backup"`
	ArchivePath              string              `json:"archive_path,omitempty"`
	Schedule                 string              `json:"schedule,omitempty"`
	Timezone                 string              `json:"timezone,omitempty"`
	Sources                  []string            `json:"sources,omitempty"`
	SharedReportingConnected bool                `json:"shared_reporting_connected,omitempty"`
	SharedReportingPending   bool                `json:"shared_reporting_pending,omitempty"`
	Stage                    string              `json:"stage"`
	WorkerID                 string              `json:"worker_id"`
	ArchiveID                string              `json:"archive_id"`
	DesiredHash              string              `json:"desired_hash,omitempty"`
	Installation             string              `json:"installation,omitempty"`
	ReportingDisabled        bool                `json:"reporting_disabled,omitempty"`
	PendingQualification     []string            `json:"pending_qualification,omitempty"`
}

// InspectSetup verifies retained setup authority without creating a journal,
// updating the schema or starting a service. Qualification private inputs are
// hashed locally and never returned in the observation. Activity and
// qualification are separate observations; an active marker is not fresh health.
func InspectSetup(ctx context.Context, home string) (SetupObservation, error) {
	var observed SetupObservation
	root, err := safefs.Open(home)
	if err != nil {
		return observed, err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", false, false)
	if err != nil {
		return observed, fmt.Errorf("backup setup ownership unavailable or busy")
	}
	defer lock.Close()
	return inspectSetupLocked(ctx, root, home)
}

// InspectSetupPolicy holds the same lease across ownership inspection and policy
// reading. The policy is for private local diffing, never a desired-state fallback.
func InspectSetupPolicy(ctx context.Context, home string) (SetupObservation, *config.DesiredWorkers, error) {
	var observed SetupObservation
	root, err := safefs.Open(home)
	if err != nil {
		return observed, nil, err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", false, false)
	if err != nil {
		return observed, nil, fmt.Errorf("backup setup ownership unavailable or busy")
	}
	defer lock.Close()
	observed, err = inspectSetupLocked(ctx, root, home)
	if err != nil || observed.Stage != "complete" {
		return observed, nil, err
	}
	var j WorkerSetupJournal
	if err = root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &j); err != nil {
		return observed, nil, err
	}
	if err = validateSetupDesired(j.Declaration); err != nil {
		return observed, nil, err
	}
	return observed, activityPolicy(j), nil
}

// Caller holds the setup lease for the entire observation/use transaction.
func inspectSetupLocked(ctx context.Context, root *safefs.Directory, home string) (SetupObservation, error) {
	return inspectSetupForReviewLocked(ctx, root, home, false)
}

func inspectSetupForReviewLocked(ctx context.Context, root *safefs.Directory, home string, legacyReview bool) (SetupObservation, error) {
	var observed SetupObservation
	var err error
	var j WorkerSetupJournal
	if err = root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &j); err != nil {
		return observed, fmt.Errorf("backup setup journal unavailable")
	}
	if j.Schema != 1 && j.Schema != 2 || !safefs.ValidID(j.WorkerID, "w") || !safefs.ValidID(j.ArchiveID, "a") || !safefs.ValidID(j.ID, "n") {
		return observed, fmt.Errorf("invalid backup setup authority")
	}
	observed = SetupObservation{Stage: j.Stage, WorkerID: j.WorkerID, ArchiveID: j.ArchiveID}
	if pending, e := inspectSetupAdoption(root, j); e != nil {
		return observed, e
	} else if pending && !legacyReview {
		observed.Stage = "adoption_pending"
		return observed, nil
	}
	if j.Declaration == nil && !legacyReview {
		observed.Stage = "adoption_required"
		return observed, nil
	}
	if j.Declaration != nil && ((j.Declaration.Version != 1 && j.Declaration.Version != 2) || !safefs.DigestPattern.MatchString(j.Declaration.DesiredHash) || !safefs.DigestPattern.MatchString(j.Declaration.HelperSHA256) || j.Declaration.Home != home || j.Declaration.UID != os.Getuid()) {
		return observed, fmt.Errorf("invalid backup declaration pin")
	}
	if j.Declaration != nil {
		if err = validateSetupDesired(j.Declaration); err != nil {
			return observed, err
		}
	} else if j.Shared != nil || j.Activity != nil || j.Producer != nil {
		return observed, fmt.Errorf("legacy review cannot reconstruct missing shared or activity authority")
	}
	paths, err := declarationPaths(home, j.Declaration)
	if err != nil {
		return observed, err
	}
	observed.Paths = &paths
	if j.SharedPolicyUpdate != nil || j.SharedPolicyExpansion != nil {
		if err = checkSharedPolicyUpdate(j); err != nil {
			return observed, err
		}
	}
	if j.SharedPolicyHistory != nil {
		if _, err = readSharedPolicyHistory(root, j.SharedPolicyHistory); err != nil {
			return observed, err
		}
	}
	if err = checkSetupTelegramRotation(j, nil, false); err != nil {
		return observed, err
	}
	if j.Activity != nil {
		if err = verifyBackupActivityHistory(root, *j.Activity); err != nil {
			return observed, err
		}
	}

	if j.Declaration != nil {
		observed.DesiredHash = j.Declaration.DesiredHash
		observed.HelperSHA256 = j.Declaration.HelperSHA256
	}
	if err = verifyNotificationScopeHistory(root, j); err != nil {
		return observed, err
	}
	if err = checkSetupNotificationScope(j); err != nil {
		return observed, err
	}
	if j.NotificationScope != nil {
		observed.Stage = "notification_policy_pending"
		observed.DesiredHash = j.NotificationScope.After.Declaration.DesiredHash
		return observed, nil
	}
	if j.SourceRetirement == nil {
		if err = validateSourceRetirementHistory(j); err != nil {
			return observed, err
		}
	}
	if j.SourceRetirement != nil {
		if err = checkSetupSourceRetirement(root, j, nil); err != nil {
			return observed, err
		}
		observed.Stage = "source_retirement_pending"
		if j.SourceRetirement.Version == 2 {
			if err = checkSetupSourceExpansion(root, j, nil); err != nil {
				return observed, err
			}
			observed.Stage = "source_replacement_pending"
		}
		observed.DesiredHash = j.SourceRetirement.Request.Declaration.DesiredHash
		return observed, nil
	}
	if j.SourceExpansion != nil {
		if err = checkSetupSourceExpansion(root, j, nil); err != nil {
			return observed, err
		}
		observed.Stage = "source_enrollment_pending"
		observed.DesiredHash = j.SourceExpansion.Request.Declaration.DesiredHash
		return observed, nil
	}
	if j.Stage != "uninstalled" {
		var removal workerLifecycleJournal
		if e := root.ReadJSON(".local/state/deploy/backup-worker/lifecycle.json", &removal); e == nil && removal.Operation == "uninstall" && removal.Stage != "complete" {
			if removal.Installation.WorkerID != j.WorkerID || removal.Installation.ArchiveID != j.ArchiveID {
				return observed, fmt.Errorf("backup removal identity differs")
			}
			observed.Stage, observed.Installation = "removing", "removal_pending"
			observed.ArchivePath = removal.Installation.Policy.Storage.Path
			return observed, nil
		} else if e != nil && !errors.Is(e, os.ErrNotExist) {
			return observed, e
		}
	}
	if j.Stage == "uninstalled" {
		var removal workerLifecycleJournal
		if err = root.ReadJSON(".local/state/deploy/backup-worker/lifecycle.json", &removal); err != nil {
			return observed, err
		}
		if removal.Operation != "uninstall" || (removal.Stage != "complete" && removal.Stage != "prepared") || removal.Installation.WorkerID != j.WorkerID || removal.Installation.ArchiveID != j.ArchiveID {
			return observed, fmt.Errorf("backup removal receipt unavailable or incomplete")
		}
		observed.Installation = "uninstalled-archive-retained"
		observed.ArchivePath = removal.Installation.Policy.Storage.Path
		if removal.Stage != "complete" {
			observed.Stage = "removing"
			observed.Installation = "removal_pending"
		} else {
			for path := range j.OwnedFiles {
				f, e := openBackupOwnedFile(root, path, os.O_RDONLY, 0)
				if e == nil {
					f.Close()
					return observed, fmt.Errorf("removed backup installation has unexpected owned files; review required")
				}
				if !errors.Is(e, os.ErrNotExist) {
					return observed, e
				}
			}
		}
		return observed, nil
	}
	if j.SharedPolicyUpdate != nil && sharedHelperVersion(j.SharedPolicyUpdate.Version) && !j.Updating && !j.RecoveryNeeded {
		observed.Stage = "helper_upgrade_pending"
		return observed, nil
	}
	if j.Stage != "complete" || j.RecoveryNeeded || j.Updating {
		observed.Stage = "pending"
		return observed, nil
	}
	if err = verifySetupSourceKeys(root, j); err != nil {
		return observed, err
	}
	if j.OwnedFiles[paths.OwnedPath(paths.Runtime)] == "" {
		return observed, fmt.Errorf("backup runtime ownership receipt missing")
	}
	for path, hash := range j.OwnedFiles {
		body, err := readBackupOwnedFile(root, path, 8<<20)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(body)) != hash {
			return observed, fmt.Errorf("installed backup artifacts drifted")
		}
	}
	f, err := openBackupOwnedFile(root, paths.OwnedPath(paths.Helper()), os.O_RDONLY, 0)
	if err != nil {
		return observed, fmt.Errorf("installed backup helper unavailable")
	}
	helperInfo, statErr := f.Stat()
	if statErr != nil || !helperInfo.Mode().IsRegular() || helperInfo.Size() <= 0 || helperInfo.Size() > 128<<20 || helperInfo.Mode().Perm()&0022 != 0 {
		f.Close()
		return observed, fmt.Errorf("installed backup helper is unsafe")
	}
	digest, err := safefs.DigestFile(f)
	f.Close()
	if err != nil || j.Declaration != nil && digest != j.Declaration.HelperSHA256 {
		return observed, fmt.Errorf("installed backup helper changed")
	}
	var artifact helper.HelperArtifactManifest
	if err = readBackupOwnedJSON(root, paths.OwnedPath(filepath.Join(paths.Installation, "artifact.json")), &artifact); err != nil || artifact.Schema != 1 || artifact.Role != "backup-worker" || artifact.Current != digest || artifact.Pending != "" {
		return observed, fmt.Errorf("backup helper ownership record is unavailable or inconsistent")
	}
	observed.HelperSHA256 = digest
	var c WorkerInstallation
	if err = readInstalledWorkerJSON(root, &c); err != nil {
		return observed, err
	}
	if err = validateWorkerInstallation(c); err != nil {
		return observed, err
	}
	observed.ArchivePath, observed.Schedule, observed.Timezone = c.Policy.Storage.Path, c.Policy.Schedule, c.Policy.Timezone
	observed.Sources = append([]string(nil), c.Policy.Sources...)
	for _, s := range c.Sources {
		observed.SourceTargets = append(observed.SourceTargets, SetupSourceTarget{Alias: s.Alias, Host: s.Host, User: s.User, Port: s.Port})
	}
	if c.WorkerID != j.WorkerID || c.ArchiveID != j.ArchiveID {
		return observed, fmt.Errorf("backup runtime identity differs from setup")
	}
	dir, err := safefs.Open(paths.State)
	if err != nil {
		return observed, fmt.Errorf("retained backup state unavailable")
	}
	defer dir.Close()
	stateInfo, err := dir.File().Stat()
	if err != nil || stateInfo.Mode().Perm() != 0700 {
		return observed, fmt.Errorf("backup state directory must be private")
	}
	lease, err := dir.Lock("schema.lock", legacyReview, false)
	if err != nil {
		if legacyReview {
			return observed, fmt.Errorf("backup database is in use; repeat import review after the current worker activity finishes")
		}
		return observed, err
	}
	defer lease.Close()
	pending, e := dir.Open("installation.pending", os.O_RDONLY, 0)
	if e == nil {
		pending.Close()
		observed.Stage = "pending"
		return observed, nil
	}
	if !errors.Is(e, os.ErrNotExist) {
		return observed, e
	}
	f, err = dir.Open("worker.db", os.O_RDONLY, 0)
	if err != nil {
		return observed, fmt.Errorf("retained backup database unavailable")
	}
	info, err := f.Stat()
	f.Close()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return observed, fmt.Errorf("unsafe backup database")
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 || st.Uid != uint32(os.Getuid()) {
		return observed, fmt.Errorf("unsafe backup database owner")
	}
	uri := url.URL{Scheme: "file", Path: filepath.Join(paths.State, "worker.db"), RawQuery: url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)", "query_only(1)"}}.Encode()}
	if legacyReview {
		// The exclusive schema lease fences every managed writer. An immutable
		// read avoids SQLite creating WAL sidecars on the installed filesystem;
		// it is permitted only after proving no uncheckpointed WAL is present.
		wal, e := dir.Open("worker.db-wal", os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if e == nil {
			info, statErr := wal.Stat()
			wal.Close()
			if statErr != nil || !info.Mode().IsRegular() || info.Size() != 0 {
				return observed, fmt.Errorf("backup has retained WAL data; settle or recover its existing database before import review")
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return observed, e
		}
		q := uri.Query()
		q.Set("immutable", "1")
		uri.RawQuery = q.Encode()
	}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return observed, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return observed, err
	}
	defer tx.Rollback()
	var version, owners int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != workerStateSchema {
		return observed, fmt.Errorf("unsupported retained backup schema")
	}
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM meta WHERE key='notification_owner'").Scan(&owners); err != nil {
		return observed, err
	}
	var shared *backupNotificationOwner
	if owners != 0 {
		owner, e := readBackupNotificationOwner(tx)
		if e != nil {
			return observed, e
		}
		if owner.Mode != "shared" || j.Declaration == nil || j.Declaration.NotificationRoot == "" || j.Producer == nil {
			observed.Stage = "ownership_review_required"
			return observed, nil
		}
		if owner.Binding.Producer != j.Producer.Producer || owner.Binding.Generation != j.Producer.Generation || !reportRouteOwnerShape(j, owner) || owner.Binding.Archive != c.ArchiveID || j.Shared != nil && j.Shared.Owner != owner {
			return observed, fmt.Errorf("shared report owner differs from setup reservation")
		}
		if e = requireReportRouteOwner(j, owner); e != nil {
			return observed, e
		}
		route, e := effectiveBackupReportRoute(tx, owner.Binding)
		if e != nil {
			return observed, e
		}
		if route != j.Declaration.NotificationRoute {
			return observed, fmt.Errorf("applied reporting route differs from retained route authority")
		}
		var completed string
		if e = backupEvaluationMeta(tx, "notification_cutover_complete", &completed); e == sql.ErrNoRows {
			observed.Stage = "pending"
			return observed, nil
		} else if e != nil {
			return observed, e
		}
		if completed == "" || completed != owner.CutoverHash {
			return observed, fmt.Errorf("shared report completion receipt changed")
		}
		shared = &owner
	}
	control, err := incidentControl(tx)
	if err != nil {
		return observed, err
	}
	if control.WriterID != c.WorkerID || control.ArchiveID != c.ArchiveID || control.ConfigHash != c.ConfigHash || (control.ReportingDisabled != (c.Policy.Notifications.Mode == "disabled") || control.SharedRequired != (c.Policy.Notifications.Mode == "shared")) {
		return observed, fmt.Errorf("retained backup control differs from installation")
	}
	observed.ReportingDisabled = control.ReportingDisabled
	observed.SharedReportingPending = control.SharedRequired && shared == nil
	if observed.SharedReportingPending {
		observed.PendingQualification = append(observed.PendingQualification, "shared_report_activation")
	}
	a, err := openBackupArchive(c.Policy.Storage.Path)
	if err != nil {
		return observed, fmt.Errorf("retained backup archive unavailable")
	}
	err = a.checkWriter(control)
	a.Close()
	if err != nil {
		return observed, err
	}
	if err = backupEvaluationMeta(tx, "installation", &observed.Installation); err != nil {
		return observed, err
	}
	if err = inspectBackupActivity(tx, j, c, &observed); err != nil {
		return observed, err
	}
	if observed.Installation == "disabled" && (j.Activity == nil || j.Activity.Complete && !j.Activity.Disabled) {
		return observed, fmt.Errorf("disabled backup has no activity authority")
	}
	if observed.Installation != "active" && observed.Installation != "staged-awaiting-qualification" && observed.Installation != "disabled" {
		return observed, fmt.Errorf("backup installation needs recovery review")
	}
	var proof WorkerQualification
	qualified := copyQualification(c.Qualification)
	for _, gate := range []string{"storage_durability", "application_restore", "notification_delivery", "external_monitor"} {
		qualified[gate] = false
	}
	if e := backupEvaluationMeta(tx, "qualification", &proof); e == nil {
		var binding backupQualificationBindingRecord
		bindingErr := backupEvaluationMeta(tx, "qualification_binding", &binding)
		if bindingErr != nil && bindingErr != sql.ErrNoRows {
			return observed, bindingErr
		}
		current, inputErr := backupQualificationBinding(j, c)
		if inputErr == nil && bindingErr == nil && binding.Version == 1 && binding.Binding == current && binding.Proof == notify.Hash(proof) && backupQualificationMatches(proof, c, time.Now().UTC()) {
			passed := func(name string) bool {
				check := proof.Checks[name]
				return check.Passed && len(check.Evidence) >= 8 && len(check.Evidence) <= 4096
			}
			qualified["storage_durability"] = passed("storage_crash_recovery")
			qualified["application_restore"] = passed("isolated_application_restore")
			qualified["notification_delivery"] = passed("notification_receipt")
			qualified["external_monitor"] = passed("missed_worker_route") && passed("daily_delivery_route")
		}
	} else if e != sql.ErrNoRows {
		return observed, e
	}
	for _, gate := range workerQualificationGates(c, true) {
		if !qualified[gate] {
			observed.PendingQualification = append(observed.PendingQualification, gate)
		}
	}
	var run WorkerRun
	if e := backupEvaluationMeta(tx, "run", &run); e == nil {
		activityRun := j.Activity == nil || j.Activity.Version != 2 || j.Activity.Setup != j.RequestHash || j.Activity.CreatedAt != nil && !run.StartedAt.Before(*j.Activity.CreatedAt)
		observed.RecentSuccessfulBackup = activityRun && run.Complete && run.ConfigHash == c.ConfigHash && len(run.Failures) == 0 && !run.FinishedAt.IsZero() && !run.FinishedAt.After(time.Now()) && time.Since(run.FinishedAt) <= time.Hour
	} else if e != sql.ErrNoRows {
		return observed, e
	}
	var activation declaredActivation
	if e := backupEvaluationMeta(tx, "declared_activation", &activation); e == nil {
		if activation.Version != 1 {
			return observed, fmt.Errorf("invalid retained activation authority")
		}
		observed.ActivationPending = !activation.Complete
	} else if e != sql.ErrNoRows {
		return observed, e
	}
	if err = tx.Commit(); err != nil {
		return observed, err
	}
	if shared != nil {
		state := &backupState{db: db, directory: paths.State}
		if err = requireBackupCutoverOwner(state, *shared); err != nil {
			return observed, err
		}
		if err = requireSharedBackupReportUnits(home, c, state, *shared); err != nil {
			return observed, err
		}
		setupDir, e := root.Sub(".local/state/deploy/backup-worker")
		if e != nil {
			return observed, e
		}
		err = checkSetupProducerReservation(setupDir, &j)
		setupDir.Close()
		if err != nil {
			return observed, err
		}
		observed.SharedReportingConnected = true
	}
	return observed, nil
}
