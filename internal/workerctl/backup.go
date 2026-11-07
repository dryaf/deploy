package workerctl

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

type BackupInstaller interface {
	Setup(context.Context, backup.SetupPlan) (backup.WorkerSetupResponse, error)
}
type BackupReserver interface {
	Reserve(context.Context, backup.SetupPlan) (backup.WorkerSetupResponse, error)
}
type BackupRemover interface {
	Remove(context.Context, config.ServerConfig, map[string]config.ServerConfig, string) (backup.RemovalResult, error)
}
type SSHBackupRemover struct{}

func (SSHBackupRemover) Remove(ctx context.Context, target config.ServerConfig, sources map[string]config.ServerConfig, desired string) (backup.RemovalResult, error) {
	return backup.UninstallWorker(ctx, target, sources, desired)
}

type SSHBackupInstaller struct{}

func (SSHBackupInstaller) Setup(ctx context.Context, p backup.SetupPlan) (backup.WorkerSetupResponse, error) {
	return backup.SetupWorker(ctx, p)
}
func (SSHBackupInstaller) Reserve(ctx context.Context, p backup.SetupPlan) (backup.WorkerSetupResponse, error) {
	return backup.ReserveWorker(ctx, p)
}

type BackupResult struct {
	ArchivePath string `json:"archive_path,omitempty"`
	Version     int    `json:"version"`
	State       string `json:"state"`
	Code        string `json:"code"`
	WorkerID    string `json:"worker_id,omitempty"`
	ArchiveID   string `json:"archive_id,omitempty"`
	Next        string `json:"next"`
}

func (c CLI) removeBackup(ctx context.Context, v Invocation, inv config.ServerInventory, p Plan) error {
	remove := c.BackupRemover
	if remove == nil {
		remove = SSHBackupRemover{}
	}
	removed, err := remove.Remove(ctx, inv.Servers[v.Server], inv.Servers, p.DesiredHash)
	result := BackupResult{Version: 1, State: "resumable", Code: "removal_unconfirmed", Next: "repeat the same uninstall command; retained source revocations and removal records determine completion"}
	if err == nil {
		if !safefs.ValidID(removed.WorkerID, "w") || !safefs.ValidID(removed.ArchiveID, "a") || !config.WorkerAbsolutePath(removed.ArchivePath) {
			return fmt.Errorf("invalid backup removal result")
		}
		result = BackupResult{Version: 1, State: "uninstalled", Code: "ok", WorkerID: removed.WorkerID, ArchiveID: removed.ArchiveID, ArchivePath: removed.ArchivePath, Next: "source access revoked and owned service/configuration files removed; archive and lifecycle history retained"}
	}
	if v.Options.JSON {
		if e := json.NewEncoder(c.Out).Encode(result); e != nil {
			return e
		}
	} else {
		if _, e := fmt.Fprintf(c.Out, "%s: backup-worker %s\n  Archive: %s\n  Next: %s\n", v.Server, result.State, result.ArchivePath, result.Next); e != nil {
			return e
		}
	}
	return err
}

func backupSetupSupport(b *config.BackupDeclaration) error {
	if b == nil || !config.WorkerEnabled(b.Enabled) {
		return fmt.Errorf("implementation_pending: disabling a backup installation requires its ownership transition")
	}
	if config.WorkerEnabled(b.Notifications.Enabled) && b.Notifications.Signal != nil {
		return fmt.Errorf("signal_setup_required: short backup setup does not yet link or adopt Signal accounts")
	}
	return nil
}

// CompileBackup resolves target path references without reading credential files.
// Existing source enrollment and archive journals own all generated identities.
func CompileBackup(b config.BackupDeclaration, f TargetFacts) (config.BackupWorkerConfig, ResolvedPaths, error) {
	var policy config.BackupWorkerConfig
	p, err := resolveBackupPaths(b.Paths, f)
	if err != nil {
		return policy, p, err
	}
	if err = backupSetupSupport(&b); err != nil {
		return policy, p, err
	}
	if b.Storage.Path != "" {
		p.Archive, err = ResolveTargetPath(b.Storage.Path, f.Home)
		if err != nil {
			return policy, p, err
		}
	}
	for _, root := range []string{p.Config, p.State, p.Installation, p.Control, p.UnitDirectory, p.Credentials, filepath.Join(f.Home, ".local/lib/deploy/backup-worker")} {
		if within(p.Archive, root) || within(root, p.Archive) {
			return policy, p, fmt.Errorf("archive overlaps an operational directory")
		}
	}
	n := b.Notifications.WorkerNotifications
	if config.WorkerEnabled(b.Notifications.Enabled) && b.Notifications.Worker != "" {
		n.Mode, n.Primary, n.Fallback, n.Signal, n.Telegram = "shared", "", "", nil, nil
	}
	if !config.WorkerEnabled(b.Notifications.Enabled) {
		n.Mode, n.Primary, n.Fallback, n.Signal, n.Telegram = "disabled", "", "", nil, nil
	}
	if n.Telegram != nil {
		v := *n.Telegram
		v.TokenFile, err = ResolveTargetPath(v.TokenFile, f.Home)
		if err != nil {
			return policy, p, err
		}
		v.ChatIDFile, err = ResolveTargetPath(v.ChatIDFile, f.Home)
		if err != nil {
			return policy, p, err
		}
		if v.TokenFile == v.ChatIDFile {
			return policy, p, fmt.Errorf("Telegram credential files must be distinct")
		}
		n.Telegram = &v
	}
	monitoring := b.Monitoring
	for _, ref := range []*string{&monitoring.BackupPingURLFile, &monitoring.DailyDeliveryPingURLFile} {
		if *ref != "" {
			*ref, err = ResolveTargetPath(*ref, f.Home)
			if err != nil {
				return policy, p, err
			}
		}
	}
	policy = config.BackupWorkerConfig{Sources: append([]string(nil), b.Sources...), Storage: config.WorkerStorage{Mode: b.Storage.Mode, Path: p.Archive, RequireMount: b.Storage.RequireMount, MinFreeBytes: b.Storage.MinFreeBytes}, Schedule: b.Schedule, Timezone: b.Timezone, Freshness: b.Freshness, MaxRunTime: b.MaxRunTime, MaxSnapshotBytes: b.MaxSnapshotBytes, Timeouts: b.Timeouts, Retention: b.Retention, Notifications: n, Monitoring: monitoring, RestoreChecks: append([]config.WorkerRestoreCheck(nil), b.RestoreChecks...)}
	policy, err = config.NormalizeWorker(policy)
	if err != nil {
		return policy, p, err
	}
	layout, err := backup.ResolveInstallationPaths(f.Home, f.ConfigHome, f.StateHome, f.DataHome, b.Paths)
	if err != nil {
		return policy, p, err
	}
	_, err = layout.UnitPaths(p.Archive)
	return policy, p, err
}

func resolveBackupPaths(paths config.WorkerPaths, facts TargetFacts) (ResolvedPaths, error) {
	p, err := ResolvePaths("backup-worker", paths, facts)
	if err != nil {
		return p, err
	}
	layout, err := backup.ResolveInstallationPaths(facts.Home, facts.ConfigHome, facts.StateHome, facts.DataHome, paths)
	if err != nil {
		return p, err
	}
	if p.Config != layout.Config || p.Runtime != layout.Runtime || p.State != layout.State || p.Data != layout.Data || p.Installation != layout.Installation || p.UnitDirectory != layout.UnitDirectory {
		return p, fmt.Errorf("backup path compiler differs from installer")
	}
	p.DeniedPaths = layout.DeniedPaths
	return p, nil
}

func (c CLI) applyBackup(ctx context.Context, v Invocation, inv config.ServerInventory, f TargetFacts, p Plan, catalog string) error {
	server := inv.Servers[v.Server]
	b := server.Desired.Backup
	if v.Operation != "adopt" {
		if handled, err := c.applyBackupActivity(ctx, v, server, f, p); handled || err != nil {
			return err
		}
	} else if b.Notifications.Worker != "" || b.Notifications.Signal != nil {
		return fmt.Errorf("shared or Signal backup adoption requires its separate handoff")
	}
	policy, _, err := CompileBackup(*b, f)
	if err != nil {
		return err
	}
	if len(p.Missing) != 0 || p.Artifacts == nil || p.Artifacts.Image == "" {
		return fmt.Errorf("missing_input: resolve backup prerequisites and a matching image/helper bundle before applying")
	}
	toTarget := func(s config.ServerConfig, a Artifact, arch string) backup.SetupTarget {
		return backup.SetupTarget{Server: s, Helper: backup.SetupArtifact{File: a.File, SHA256: a.SHA256, Architecture: arch}}
	}
	plan := backup.SetupPlan{SourceAdministrators: inv.Servers, Target: toTarget(server, p.Artifacts.Helper, f.Arch), Sources: map[string]backup.SetupTarget{}, Request: backup.WorkerSetupRequest{Schema: 1, Operation: "apply", Policy: policy, Image: p.Artifacts.Image, RelayImage: p.Artifacts.RelayImage}}
	if v.Operation == "adopt" {
		plan.Request.Operation = "adopt"
	}
	plan.Request.Declaration = &backup.SetupDeclaration{Version: 1, DesiredHash: p.DesiredHash, HelperSHA256: p.Artifacts.Helper.SHA256, Sources: map[string]string{}, UID: f.UID, User: f.User, Home: f.Home, Architecture: f.Arch}
	desired := backupDesired(*server.Desired)
	plan.Request.Declaration.Desired = &desired
	layout, err := backup.ResolveInstallationPaths(f.Home, f.ConfigHome, f.StateHome, f.DataHome, b.Paths)
	if err != nil {
		return err
	}
	legacy, err := backup.ResolveInstallationPaths(f.Home, "", "", "", config.WorkerPaths{})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(layout, legacy) {
		plan.Request.Declaration.Version, plan.Request.Declaration.Paths = 2, &layout
	}
	trust := c.SourceTrust
	if trust == nil {
		trust = backup.VerifiedSetupKnownHosts
	}
	for _, alias := range policy.Sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		source, ok := inv.Servers[alias]
		if !ok {
			return fmt.Errorf("source is absent from inventory")
		}
		sf, err := c.Inspector.Inspect(ctx, source)
		if err != nil {
			return fmt.Errorf("source %s inspection failed: %w", alias, err)
		}
		if sf.OS != "linux" || sf.UID <= 0 || sf.User != source.User || (sf.Arch != "amd64" && sf.Arch != "arm64") {
			return fmt.Errorf("source requires a matching rootless Linux helper")
		}
		a, err := ResolveArtifacts(config.WorkerArtifacts{Catalog: b.Artifacts.Catalog}, sf.Arch, catalog)
		if err != nil {
			return fmt.Errorf("source %s: %w", alias, err)
		}
		known, err := trust(ctx, source)
		if err != nil {
			return fmt.Errorf("source %s: %w", alias, err)
		}
		port := source.SSHPort
		if port == 0 {
			port = 22
		}
		plan.Sources[alias] = toTarget(source, a.Helper, sf.Arch)
		plan.Request.Sources = append(plan.Request.Sources, backup.SetupSource{Alias: alias, Host: source.Host, User: source.User, Port: port, KnownHosts: known})
		plan.Request.Declaration.Sources[alias] = a.Helper.SHA256
	}
	installer := c.BackupInstaller
	if installer == nil {
		installer = SSHBackupInstaller{}
	}
	var reserved backupDependencyResult
	if policy.Notifications.Mode == "shared" {
		if reserved, err = c.prepareBackupDependency(ctx, v, inv, f, catalog, &plan, installer); err != nil {
			return err
		}
	}
	installed, err := installer.Setup(ctx, plan)
	result := BackupResult{Version: 1, State: "resumable", Code: "setup_unconfirmed", Next: "retry the same declaration; retained setup and source enrollment records determine the outcome"}
	if err == nil {
		if (reserved.WorkerID != "" && (installed.WorkerID != reserved.WorkerID || installed.ArchiveID != reserved.ArchiveID)) || installed.Schema != 1 || installed.Stage != "complete" || !safefs.ValidID(installed.WorkerID, "w") || !safefs.ValidID(installed.ArchiveID, "a") {
			err = fmt.Errorf("invalid completed backup setup response")
		} else {
			result = BackupResult{Version: 1, State: "qualification_required", Code: "ok", WorkerID: installed.WorkerID, ArchiveID: installed.ArchiveID, Next: "installation is staged with timers disabled; run a manual backup and complete the selected qualification checks before activation"}
			if installed.Unchanged {
				result.State = "unchanged"
				result.Next = "installed policy and owned files match; status shows runtime observations and any remaining qualification"
			}
			if v.Operation == "adopt" {
				result.State = "adopted"
				result.Next = "existing backup declaration adopted; source keys, archive writer, runtime and service activity retained; status shows pending qualification"
			}
			if policy.Notifications.Mode == "shared" {
				if reserved.State == "delivery_admitted" || reserved.State == "backup_policy_update" {
					var connected backupDependencyResult
					connected, err = c.prepareBackupDependency(ctx, v, inv, f, catalog, &plan, installer)
					if err == nil && (connected.WorkerID != installed.WorkerID || connected.ArchiveID != installed.ArchiveID) {
						err = fmt.Errorf("report connection changed backup identity")
					}
					if err == nil {
						reserved = connected
					}
				}
				result.State = "qualification_required"
				result.Next = "backup installed and real producer enrolled; finish notification qualification and reapply to connect shared reporting; direct sending is fenced"
				if reserved.State == "reporting_connected" || reserved.State == "commands_admitted" || reserved.State == "commands_disabled" {
					result.Next = "shared reporting connected; complete backup qualification and a successful manual backup before schedule activation"
					if installed.Unchanged {
						result.State = "unchanged"
						result.Next = "installed policy and shared reporting ownership match; status shows any remaining backup qualification or activation"
					}
				}
				if reserved.State == "needs_qualification" {
					result.State, result.Next = "qualification_required", "shared notification policy is updated and delivery is paused; run deploy notification-worker qualify "+v.Server+", then apply; complete backup qualification before activation"
				}
				if err != nil {
					result.State, result.Code, result.Next = "resumable", "reporting_unconfirmed", "backup is installed; reapply the same declaration to resume the retained shared report connection"
				}
			}
		}
	}
	if v.Options.JSON {
		if e := json.NewEncoder(c.Out).Encode(result); e != nil {
			return e
		}
	} else {
		if _, e := fmt.Fprintf(c.Out, "%s: backup-worker %s\n  Next: %s\n", v.Server, result.State, result.Next); e != nil {
			return e
		}
	}
	return err
}

type backupDependencyResult struct {
	backup.WorkerSetupResponse
	State string
}

func (c CLI) prepareBackupDependency(ctx context.Context, v Invocation, inv config.ServerInventory, f TargetFacts, catalog string, plan *backup.SetupPlan, installer BackupInstaller) (backupDependencyResult, error) {
	var zero backupDependencyResult
	desired := *inv.Servers[v.Server].Desired
	if desired.Backup.Notifications.Worker != v.Server {
		return zero, fmt.Errorf("shared notifications require the same server and UID")
	}
	if err := directNotificationSupport(desired); err != nil {
		return zero, err
	}
	np, err := DeclarationPlan(inv, "notification-worker", v.Server, "apply")
	if err != nil {
		return zero, err
	}
	np, err = ResolvePlan(np, inv.Servers[v.Server], f, catalog)
	if err != nil {
		return zero, err
	}
	if len(np.Missing) != 0 || np.Artifacts == nil {
		return zero, fmt.Errorf("missing_input: notification dependency prerequisites unavailable")
	}
	plan.Request.Declaration.NotificationDesiredHash = np.DesiredHash
	plan.Request.Declaration.NotificationRoot = np.Paths.Installation
	plan.Request.Declaration.NotificationRoute = desired.Backup.Notifications.Route
	r := ControlRequest{Version: 1, Worker: "notification-worker", Server: v.Server, Action: "apply", Facts: f, Desired: desired, DesiredScope: np.DesiredScope, DesiredHash: np.DesiredHash, Inputs: map[string][]byte{}}
	if err = prepareOperatorNotificationInputs(&r, *np.Artifacts, f.Arch); err != nil {
		return zero, err
	}
	reserver, ok := installer.(BackupReserver)
	if !ok {
		return zero, fmt.Errorf("backup installer lacks durable identity reservation")
	}
	reserved, err := reserver.Reserve(ctx, *plan)
	if err != nil {
		return zero, err
	}
	if reserved.Schema != 1 || reserved.Stage != "reserved" || reserved.Producer == nil || reserved.Producer.Validate(reserved.WorkerID) != nil || !safefs.ValidID(reserved.ArchiveID, "a") {
		return zero, fmt.Errorf("invalid backup producer reservation")
	}
	if reserved.NotificationPolicyUpdateRequired && reserved.PolicyUpdateRequired {
		return zero, fmt.Errorf("backup dependency returned conflicting update order")
	}
	if reserved.PolicyUpdateRequired {
		// The target proved an existing producer and unchanged gateway scope.
		// Finish the backup transaction before asking the gateway to inspect the
		// completed dependency. A prepared update is deliberately not admissible
		// as a fresh producer reservation at the gateway.
		return backupDependencyResult{WorkerSetupResponse: reserved, State: "backup_policy_update"}, nil
	}
	if c.Controller == nil {
		c.Controller = SSHController{}
	}
	result, err := c.applyNotificationRequest(ctx, inv.Servers[v.Server], np.Artifacts.Helper, r, v.Options)
	if result.LEDPrerequisite != nil {
		var writeErr error
		if v.Options.JSON {
			writeErr = json.NewEncoder(c.Out).Encode(result)
		} else {
			writeErr = printControlResult(c.Out, v.Server, "notification-worker", result)
		}
		if writeErr != nil {
			return zero, writeErr
		}
		if err != nil {
			return zero, err
		}
		return zero, fmt.Errorf("led_host_prerequisite: apply the reviewed host prerequisite before resuming backup setup")
	}
	if err != nil {
		return zero, err
	}
	if result.Version != 1 || result.Code != "ok" || (result.State != "verification" && result.State != "needs_qualification" && result.State != "delivery_admitted" && result.State != "reporting_connected" && result.State != "commands_admitted" && result.State != "commands_disabled") || result.Identity == nil || !result.Identity.Valid() {
		return zero, fmt.Errorf("notification dependency has not confirmed real producer enrollment")
	}
	return backupDependencyResult{WorkerSetupResponse: reserved, State: result.State}, nil
}

func inspectBackupControl(ctx context.Context, r ControlRequest) (ControlResult, error) {
	result := ControlResult{Version: 1, State: "needs_action", Code: "admission_unverified"}
	if r.Desired.Backup == nil {
		return result, fmt.Errorf("backup declaration missing")
	}
	if _, err := resolveBackupPaths(r.Desired.Backup.Paths, r.Facts); err != nil {
		return result, err
	}
	observed, err := backup.InspectSetup(ctx, r.Facts.Home)
	if err != nil {
		return result, err
	}
	if observed.Paths != nil {
		layout, e := backup.ResolveInstallationPaths(r.Facts.Home, r.Facts.ConfigHome, r.Facts.StateHome, r.Facts.DataHome, r.Desired.Backup.Paths)
		if e != nil {
			return result, e
		}
		if !reflect.DeepEqual(layout, *observed.Paths) {
			return result, fmt.Errorf("path_migration_required: resolved backup layout differs from retained installation; restore the previous declaration or use an explicit migration")
		}
	}
	result.Backup = &observed
	var endpointChanges []Change
	if observed.Stage == "complete" {
		endpointChanges, err = sourcePlanChanges(r.SourceTargets, observed.SourceTargets)
		if err != nil {
			return result, err
		}
	}
	switch {
	case observed.Stage == "notification_policy_pending":
		result.State, result.Code = "resumable", "notification_policy_pending"
		result.Next = "deploy notification-worker resume " + r.Server + " with the exact retained notification policy declaration"
	case observed.Stage == "source_replacement_pending":
		result.State, result.Code, result.Next = "resumable", "source_replacement_pending", backup.ErrSourceReplacementPending.Error()
	case observed.Stage == "source_retirement_pending":
		result.State, result.Code = "resumable", "source_retirement_pending"
		result.Next = backup.ErrSourceRetirementPending.Error()
	case observed.Stage == "helper_upgrade_pending":
		result.State, result.Code = "resumable", "helper_upgrade_pending"
		result.Next = "deploy backup-worker resume " + r.Server + " with the exact retained helper and image selection"
	case observed.Stage == "source_enrollment_pending":
		result.State, result.Code = "resumable", "source_enrollment_pending"
		result.Next = "deploy backup-worker resume " + r.Server + " with the exact source-addition declaration; reserved keys and remote enrollments survive rollback"
	case observed.Stage == "removing":
		result.State, result.Code = "resumable", "pending_removal"
		result.Next = "repeat uninstall to finish the retained removal; archive and lifecycle history remain"
	case observed.Stage == "uninstalled":
		result.State, result.Code = "uninstalled", "ok"
		result.Next = "source access and owned services removed; archive and lifecycle history retained"
	case observed.Stage == "adoption_required":
		result.Code = "adoption_required"
		result.Next = "existing specialist installation requires explicit declarative adoption"
	case observed.Stage == "activity_pending":
		result.State, result.Code = "resumable", "activity_pending"
		result.Next = "deploy backup-worker resume " + r.Server + " with the unchanged YAML activity declaration"
	case observed.DesiredHash != r.DesiredHash:
		result.Code = "policy_changed"
		result.Next = "apply reconciles ordinary policy edits, source additions, explicit retire_sources dispositions and moves retaining source identity; combined new-alias replacements retain enrollment and revocation; retired identity reuse, path and delivery ownership changes require migration"
	case observed.Stage == "pending":
		result.State, result.Code = "resumable", "pending_operation"
		result.Next = "resume the same declaration; existing backup identities and source enrollments are retained"
	case observed.ActivationPending:
		result.State, result.Code = "resumable", "pending_activation"
		result.Next = "deploy backup-worker activate " + r.Server
	case observed.Stage != "complete":
		result.Code = "ownership_review_required"
		result.Next = "inspect the retained notification ownership transition"
	case len(endpointChanges) > 0:
		result.State, result.Code, result.Changes = "needs_action", "source_migration_required", endpointChanges
		result.Next = "source inventory differs from installed endpoints; plan and apply the move with policy/artifacts unchanged, retaining the same source identity and key"
	case observed.Installation == "disabled":
		result.State, result.Code = "disabled", "ok"
		result.Next = "backup services disabled by YAML; archive, keys and history retained; set enabled: true and apply to stage reactivation"
	case len(observed.PendingQualification) != 0 || observed.Installation != "active":
		result.State, result.Code = "qualification_required", "ok"
		result.Next = "deploy backup-worker qualify " + r.Server
		if len(observed.PendingQualification) == 0 {
			result.Next = "deploy backup-worker activate " + r.Server
			if !observed.RecentSuccessfulBackup {
				result.Next = "deploy backup-worker run " + r.Server + "; after success, deploy backup-worker activate " + r.Server
			}
		}
	default:
		result.State, result.Code = "active_recorded", "ok"
		result.Next = "activation is recorded; verify current service and backup health separately"
	}
	return result, nil
}
