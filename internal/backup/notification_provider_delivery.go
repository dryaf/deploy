package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type providerScopeDependencies struct {
	capture     backupActivityDependencies
	credentials []string
	profile     func(string, notify.Identity, notify.Producer, string) (install.ManagedReportProfile, error)
	captured    func(context.Context, string, string, install.ManagedReportProfile) (install.ManagedManifest, error)
	services    func(string, WorkerInstallation) reportCredentialServices
}

// ReconcileDeliveryProviderScope installs the expanded reporting exclusions
// before the supplied exact gateway transition. Credential contents, bot and
// destination are checked by that transition; this function grants no new
// delivery or producer authority.
func reportPolicyScope(version int) bool {
	return version == 16 || version == 17 || version == 18 || version == 19 || version == 20 || version == 21 || version == 23
}
func ReconcileDeliveryLimitsScope(ctx context.Context, home string, r VerificationNotificationScopeChange, credentials []string, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileReportPolicy(ctx, home, r, credentials, migrate, 17)
}
func ReconcileDeliveryProviderScope(ctx context.Context, home string, r VerificationNotificationScopeChange, credentials []string, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileReportPolicy(ctx, home, r, credentials, migrate, 16)
}
func reconcileReportPolicy(ctx context.Context, home string, r VerificationNotificationScopeChange, credentials []string, migrate func(ProducerReservation) (string, error), version int) (string, error) {
	paths := slices.Clone(credentials)
	slices.Sort(paths)
	deps := &providerScopeDependencies{capture: backupActivityDependencies{inspect: inspectBackupActivityUnits, stop: stopBackupActivityUnits}, credentials: slices.Compact(paths), profile: install.InspectManagedReportProfile, captured: install.InspectCapturedReportProfile, services: func(home string, c WorkerInstallation) reportCredentialServices {
		return reportCredentialManager{&backupCutoverManager{home: home, paths: backupCutoverPaths(home, c), freshShared: true}}
	}}
	return reconcileNotificationScopeReports(ctx, home, r, migrate, version, deps)
}

func (v setupNotificationScope) providerReportInstallation() WorkerInstallation {
	p, _ := declarationPaths(v.Before.Declaration.Home, v.Before.Declaration)
	return WorkerInstallation{Policy: v.After.Policy, Paths: &p}
}
func (v setupNotificationScope) providerReportPaths() WorkerUnitPaths {
	return backupCutoverPaths(v.Before.Declaration.Home, v.providerReportInstallation())
}
func (v setupNotificationScope) validateProviderReports() error {
	p := v.Reports
	b := v.Before
	if p == nil || b.Declaration == nil || b.Producer == nil || p.Owner.Mode != "shared" || p.Owner.CutoverHash == "" || p.Owner.Binding.Producer != b.Producer.Producer || p.Owner.Binding.Generation != b.Producer.Generation || p.Owner.Binding.Archive != b.ArchiveID || p.Owner.Binding.Route != b.Declaration.NotificationRoute || p.Owner.Binding.Identity != p.BeforeProfile.Identity || p.BeforeProfile.ManifestHash != v.BeforeGateway {
		return fmt.Errorf("delivery provider scope lost its reporting owner")
	}
	if _, err := declarationPaths(b.Declaration.Home, b.Declaration); err != nil {
		return err
	}
	if (v.Version == 17 || v.Version == 18 || v.Version == 19 || v.Version == 23) && !reflect.DeepEqual(p.Credentials, p.BeforeProfile.Credentials) {
		return fmt.Errorf("budget scope changed report credentials")
	}
	return p.validate(v.providerReportInstallation(), v.providerReportPaths())
}

func (v *setupNotificationScope) prepareProviderReports(ctx context.Context, root *safefs.Directory, c WorkerInstallation, deps *providerScopeDependencies) error {
	if deps.profile == nil || deps.services == nil {
		return fmt.Errorf("report credential inspection unavailable")
	}
	home := v.Before.Declaration.Home
	state, err := openInstalledBackupState(home, false)
	if err != nil {
		return err
	}
	owner, err := currentSharedBackupOwner(state)
	if err == nil {
		err = requireCommandScopeOwner(state, v.Before)
	}
	var activity string
	if err == nil {
		err = state.get("installation", &activity)
	}
	state.Close()
	if err != nil {
		return err
	}
	if owner == nil || (activity != "active" && activity != "staged-awaiting-qualification") {
		return fmt.Errorf("report credential rotation requires completed shared reporting")
	}
	profile, err := deps.profile(v.Before.Declaration.NotificationRoot, owner.Binding.Identity, owner.Binding.Producer, owner.Binding.Route)
	if err != nil {
		return err
	}
	if profile.ManifestHash != v.BeforeGateway {
		return fmt.Errorf("report gateway changed before credential review")
	}
	p, err := newProviderReports(c, backupCutoverPaths(home, c), profile, deps.credentials, activity == "active")
	if err != nil {
		return err
	}
	p.Owner = *owner
	p.BackupActive = activity == "active"
	services := deps.services(home, c)
	// Reporting can already be active while backup capture awaits qualification.
	// Preserve the observed report triggers independently of capture activation.
	activeTriggers := true
	for _, name := range []string{"deploy-backup-report.timer", "deploy-backup-report.path"} {
		if e := services.Inspect(ctx, name, filepath.Join(p.BeforeProfile.UnitDirectory, name), false, true, false); e != nil {
			activeTriggers = false
		}
	}
	p.Active = activeTriggers
	for name, body := range p.BeforeUnits {
		path := filepath.Join(p.BeforeProfile.UnitDirectory, name)
		raw, e := readBackupOwnedFile(root, path, 1<<20)
		if e != nil || string(raw) != body {
			return fmt.Errorf("report unit changed before retaining credential intent")
		}
		if e = services.Inspect(ctx, name, path, !p.Active, p.Active && name != "deploy-backup-report.service", false); e != nil {
			return e
		}
	}
	v.Reports = &p
	return nil
}

func (v setupNotificationScope) stageProviderReports(ctx context.Context, root *safefs.Directory, deps *providerScopeDependencies, boundary func(string) error) error {
	c := v.providerReportInstallation()
	return v.Reports.stage(ctx, root, c, v.providerReportPaths(), deps.services(v.Before.Declaration.Home, c), boundary)
}

func (v setupNotificationScope) providerReportsAfter(j WorkerSetupJournal, gateway string) WorkerSetupJournal {
	p := v.Reports.BeforeProfile
	p.ManifestHash, p.Credentials = gateway, slices.Clone(v.Reports.Credentials)
	j.Schema = 2
	j.Shared = &sharedWorkerSetup{Version: 1, Owner: v.Reports.Owner, GatewayRoot: v.Before.Declaration.NotificationRoot, Profile: p}
	j.OwnedFiles = v.Reports.ownedFiles(j.OwnedFiles, v.providerReportPaths())
	return j
}

func (v setupNotificationScope) inspectProviderProfile(gateway string, deps *providerScopeDependencies) error {
	o := v.Reports.Owner
	profile, err := deps.profile(v.Before.Declaration.NotificationRoot, o.Binding.Identity, o.Binding.Producer, o.Binding.Route)
	if err != nil {
		return err
	}
	want := v.Reports.BeforeProfile
	want.ManifestHash, want.Credentials = gateway, v.Reports.Credentials
	if profile.ManifestHash != gateway && deps.captured != nil {
		if _, err = deps.captured(context.Background(), v.Before.Declaration.NotificationRoot, profile.ManifestHash, want); err != nil {
			return err
		}
		profile.ManifestHash = gateway
	}
	if !reflect.DeepEqual(profile, want) {
		return fmt.Errorf("provider scope gateway differs from reviewed report exclusions")
	}
	return nil
}
func (v setupNotificationScope) inspectProviderReports(ctx context.Context, root *safefs.Directory, gateway string, deps *providerScopeDependencies, stopped bool) error {
	if err := v.inspectProviderProfile(gateway, deps); err != nil {
		return err
	}
	c := v.providerReportInstallation()
	return v.Reports.inspect(ctx, root, v.providerReportPaths(), deps.services(v.Before.Declaration.Home, c), stopped)
}

type providerReportCompletion struct {
	Version int    `json:"version"`
	Intent  string `json:"intent"`
	Gateway string `json:"gateway"`
}

func (v setupNotificationScope) finishProviderReports(ctx context.Context, root *safefs.Directory, gateway string, deps *providerScopeDependencies) error {
	name := ".local/state/deploy/backup-worker/notification-report-" + v.Operation + ".json"
	want := providerReportCompletion{1, notify.Hash(v), gateway}
	var receipt providerReportCompletion
	err := root.ReadJSON(name, &receipt)
	if err == nil {
		if receipt != want {
			return fmt.Errorf("completed report exclusion receipt changed")
		}
		return v.inspectProviderReports(ctx, root, gateway, deps, false)
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err = v.inspectProviderProfile(gateway, deps); err != nil {
		return err
	}
	paths := v.providerReportPaths()
	d, err := safefs.Open(paths.State)
	if err != nil {
		return err
	}
	defer d.Close()
	lease, err := d.Lock("schema.lock", true, false)
	if err != nil {
		return err
	}
	state, err := openBackupStateWithLease(paths.State, false, false, true)
	if err != nil {
		lease.Close()
		return err
	}
	owner, err := currentSharedBackupOwner(state)
	if err == nil {
		err = requireCommandScopeOwner(state, v.Before)
	}
	var activity string
	if err == nil {
		err = state.get("installation", &activity)
	}
	state.Close()
	if err != nil {
		lease.Close()
		return err
	}
	if owner == nil || *owner != v.Reports.Owner || (activity != "active" && activity != "staged-awaiting-qualification") || (activity == "active") != (v.Reports.BackupActive && v.Version != 23) {
		lease.Close()
		return fmt.Errorf("reporting owner or activity changed during credential scope")
	}
	if v.Version == 23 {
		if e := v.inspectFreshnessCapture(ctx, deps); e != nil {
			lease.Close()
			return e
		}
	}
	c := v.providerReportInstallation()
	services := deps.services(v.Before.Declaration.Home, c)
	// Verify exact loaded restrictions before opening the runtime gate. A lost
	// final reply can leave triggers running already, so activity is not forced.
	for unit, body := range v.Reports.AfterUnits {
		path := filepath.Join(paths.unitDirectory(), unit)
		raw, e := readBackupOwnedFile(root, path, 1<<20)
		if e != nil || string(raw) != body {
			lease.Close()
			return fmt.Errorf("report exclusions changed before finishing credential scope")
		}
		if e = services.Inspect(ctx, unit, path, false, false, false); e != nil {
			lease.Close()
			return e
		}
	}
	var gate installationGate
	err = d.ReadJSON("installation.pending", &gate)
	if err == nil && gate.Operation != v.Operation {
		lease.Close()
		return fmt.Errorf("report scope belongs to another installation gate")
	}
	if err != nil && !os.IsNotExist(err) {
		lease.Close()
		return err
	}
	if err == nil && v.Version == 23 {
		err = v.finishFreshnessGate(root)
	} else if err == nil {
		err = clearInstallationGate(paths.State, v.Operation)
	} else {
		err = nil
	}
	lease.Close()
	if err != nil {
		return err
	}
	if err = v.Reports.resumeTriggers(ctx, root, paths, services); err != nil {
		return err
	}
	return root.WriteJSON(name, want, false)
}

func ReconcileDeliveryProducerScope(ctx context.Context, home string, r VerificationNotificationScopeChange, credentials []string, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileReportPolicy(ctx, home, r, credentials, migrate, 18)
}

func ReconcileDeliveryRoutesScope(ctx context.Context, home string, r VerificationNotificationScopeChange, credentials []string, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileReportPolicy(ctx, home, r, credentials, migrate, 19)
}

func ReconcileDeliveryDestinationScope(ctx context.Context, home string, r VerificationNotificationScopeChange, credentials []string, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileReportPolicy(ctx, home, r, credentials, migrate, 20)
}

func ReconcileDeliveryProviderEnrollmentScope(ctx context.Context, home string, r VerificationNotificationScopeChange, credentials []string, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileReportPolicy(ctx, home, r, credentials, migrate, 21)
}
