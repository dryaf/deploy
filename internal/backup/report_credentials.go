package backup

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

// The scope intent freezes both report-unit bodies before touching the gateway.
// Expanding exclusions cannot drop access restrictions for retired credentials.
// Publication requires the installation gate and schema lease; trigger restart
// happens only after the enclosing scope has published and cleared that gate.
type setupProviderReports struct {
	BackupActive  bool                         `json:"backup_active"`
	Owner         backupNotificationOwner      `json:"owner"`
	BeforeProfile install.ManagedReportProfile `json:"before_profile"`
	Credentials   []string                     `json:"credentials"`
	BeforeUnits   map[string]string            `json:"before_units"`
	AfterUnits    map[string]string            `json:"after_units"`
	Active        bool                         `json:"active"`
}

type reportCredentialServices interface {
	Inspect(context.Context, string, string, bool, bool, bool) error
	Stop(context.Context) error
	Reload(context.Context) error
	Start(context.Context) error
}

type reportCredentialManager struct{ manager *backupCutoverManager }

func (m reportCredentialManager) Inspect(ctx context.Context, name, path string, stopped, enabled, reload bool) error {
	return m.manager.inspectReload(ctx, name, path, stopped, enabled, reload)
}
func (m reportCredentialManager) Stop(ctx context.Context) error {
	return cutoverSystemctl(ctx, "stop", "deploy-backup-report.timer", "deploy-backup-report.path", "deploy-backup-report.service")
}
func (m reportCredentialManager) Reload(ctx context.Context) error {
	return cutoverSystemctl(ctx, "daemon-reload")
}
func (m reportCredentialManager) Start(ctx context.Context) error {
	// Never start the report service directly. Its existing triggers keep their
	// enablement and deliver any queued work through the unchanged owner.
	return cutoverSystemctl(ctx, "start", "deploy-backup-report.timer", "deploy-backup-report.path")
}

func newProviderReports(c WorkerInstallation, paths WorkerUnitPaths, profile install.ManagedReportProfile, credentials []string, active bool) (setupProviderReports, error) {
	v := setupProviderReports{BeforeProfile: profile, Credentials: slices.Clone(credentials), Active: active}
	slices.Sort(v.Credentials)
	v.Credentials = slices.Compact(v.Credentials)
	var err error
	v.BeforeUnits, err = renderBackupReportUnits(paths, c.Policy, profile.Credentials)
	if err != nil {
		return v, err
	}
	v.AfterUnits, err = renderBackupReportUnits(paths, c.Policy, v.Credentials)
	if err != nil {
		return v, err
	}
	return v, v.validate(c, paths)
}

func (v setupProviderReports) validate(c WorkerInstallation, paths WorkerUnitPaths) error {
	if !safefs.DigestPattern.MatchString(v.BeforeProfile.ManifestHash) || !v.BeforeProfile.Identity.Valid() || v.BeforeProfile.UnitDirectory != paths.unitDirectory() || !slices.IsSorted(v.Credentials) || len(slices.Compact(slices.Clone(v.Credentials))) != len(v.Credentials) {
		return fmt.Errorf("report credential transition lost its gateway profile")
	}
	for _, path := range v.BeforeProfile.Credentials {
		if !slices.Contains(v.Credentials, path) {
			return fmt.Errorf("report credential transition removed a retained exclusion")
		}
	}
	before, err := renderBackupReportUnits(paths, c.Policy, v.BeforeProfile.Credentials)
	if err != nil {
		return err
	}
	after, err := renderBackupReportUnits(paths, c.Policy, v.Credentials)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(v.BeforeUnits, before) || !reflect.DeepEqual(v.AfterUnits, after) {
		return fmt.Errorf("report credential transition changed unit authority")
	}
	return nil
}

func (v setupProviderReports) matches(path string, raw []byte, paths WorkerUnitPaths) bool {
	for name, before := range v.BeforeUnits {
		full := filepath.Join(paths.unitDirectory(), name)
		owned, err := filepath.Rel(paths.Home, full)
		if err != nil {
			return false
		}
		if (path == full || path == owned) && (string(raw) == before || string(raw) == v.AfterUnits[name]) {
			return true
		}
	}
	return false
}

func (v setupProviderReports) ownedFiles(before map[string]string, paths WorkerUnitPaths) map[string]string {
	owned := maps.Clone(before)
	if owned == nil {
		owned = map[string]string{}
	}
	for name, body := range v.AfterUnits {
		path := filepath.Join(paths.unitDirectory(), name)
		path = (InstallationPaths{Home: paths.Home}).OwnedPath(path)
		owned[path] = sharedSetupDigest([]byte(body))
	}
	return owned
}

// Stage is idempotent at every boundary. Even after a lost reload reply it
// accepts only the two exact retained unit bodies, never arbitrary drift.
func (v setupProviderReports) stage(ctx context.Context, root *safefs.Directory, c WorkerInstallation, paths WorkerUnitPaths, services reportCredentialServices, boundary func(string) error) error {
	if err := v.validate(c, paths); err != nil {
		return err
	}
	for _, name := range []string{"deploy-backup-report.timer", "deploy-backup-report.path", "deploy-backup-report.service"} {
		path := filepath.Join(paths.unitDirectory(), name)
		raw, err := readBackupOwnedFile(root, path, 1<<20)
		if err != nil || !v.matches(path, raw, paths) {
			return fmt.Errorf("report unit drift before credential publication: %s", name)
		}
		if err = services.Inspect(ctx, name, path, false, false, true); err != nil {
			return err
		}
	}
	if err := services.Stop(ctx); err != nil {
		return err
	}
	if err := boundary("report-stopped"); err != nil {
		return err
	}
	for _, name := range []string{"deploy-backup-report.timer", "deploy-backup-report.path", "deploy-backup-report.service"} {
		path := filepath.Join(paths.unitDirectory(), name)
		if err := services.Inspect(ctx, name, path, true, false, true); err != nil {
			return err
		}
		raw, err := readBackupOwnedFile(root, path, 1<<20)
		if err != nil || !v.matches(path, raw, paths) {
			return fmt.Errorf("report unit drift during credential publication: %s", name)
		}
		if string(raw) != v.AfterUnits[name] {
			d, err := openBackupOwnedParent(root, path)
			if err != nil {
				return err
			}
			err = safefs.ReplaceFile(d, name, raw, []byte(v.AfterUnits[name]), 0600)
			d.Close()
			if err != nil {
				return err
			}
		}
	}
	if err := boundary("report-files"); err != nil {
		return err
	}
	if err := services.Reload(ctx); err != nil {
		return err
	}
	if err := boundary("report-reloaded"); err != nil {
		return err
	}
	return v.inspect(ctx, root, paths, services, true)
}

func (v setupProviderReports) inspect(ctx context.Context, root *safefs.Directory, paths WorkerUnitPaths, services reportCredentialServices, stopped bool) error {
	for _, name := range []string{"deploy-backup-report.timer", "deploy-backup-report.path", "deploy-backup-report.service"} {
		path := filepath.Join(paths.unitDirectory(), name)
		raw, err := readBackupOwnedFile(root, path, 1<<20)
		if err != nil || string(raw) != v.AfterUnits[name] {
			return fmt.Errorf("completed report credential exclusion changed: %s", name)
		}
		if err = services.Inspect(ctx, name, path, stopped || !v.Active, !stopped && v.Active && name != "deploy-backup-report.service", false); err != nil {
			return err
		}
	}
	return nil
}

func (v setupProviderReports) resumeTriggers(ctx context.Context, root *safefs.Directory, paths WorkerUnitPaths, services reportCredentialServices) error {
	// Check the exact files before allowing any report process to run.
	for name, body := range v.AfterUnits {
		raw, err := readBackupOwnedFile(root, filepath.Join(paths.unitDirectory(), name), 1<<20)
		if err != nil || string(raw) != body {
			return fmt.Errorf("report credential exclusion changed before trigger restart")
		}
		if err = services.Inspect(ctx, name, filepath.Join(paths.unitDirectory(), name), false, false, false); err != nil {
			return err
		}
	}
	if v.Active {
		if err := services.Start(ctx); err != nil {
			return err
		}
	}
	return v.inspect(ctx, root, paths, services, false)
}
