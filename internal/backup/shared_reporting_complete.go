package backup

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func inspectFreshReportingCompletion(ctx context.Context, home string, c WorkerInstallation, spec backupCutoverSpec, deps backupCutoverDependencies) (bool, error) {
	root, err := safefs.Open(home)
	if err != nil {
		return false, err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", false, false)
	if err != nil {
		return false, err
	}
	defer lock.Close()
	var j WorkerSetupJournal
	if err = root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &j); err != nil {
		return false, err
	}
	if err = requireFreshBackupReportingSetup(root, home, j, spec); err != nil {
		return false, err
	}
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	if err != nil {
		return false, err
	}
	defer state.Close()
	var complete string
	err = state.get("notification_cutover_complete", &complete)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var owner backupNotificationOwner
	if err = state.get("notification_owner", &owner); err != nil {
		return false, err
	}
	if complete == "" || owner.Mode != "shared" || owner.CutoverHash != complete || owner.Binding.Producer != spec.Managed.Producer || owner.Binding.Generation != spec.Managed.Generation || owner.Binding.Route != spec.Managed.Route {
		return false, fmt.Errorf("shared reporting completion differs from retained authority")
	}
	if spec.Version == 6 || spec.Version == 7 || spec.Version == 8 {
		var control ArchiveControl
		if err = state.get("control", &control); err != nil {
			return false, err
		}
		if control.ConfigHash != c.ConfigHash || !control.SharedRequired || control.ReportingDisabled || j.Shared == nil || j.Shared.Owner != owner || spec.Predecessor != complete {
			return false, fmt.Errorf("updated shared policy lost its archive or ownership authority")
		}
		d, e := safefs.Open(state.directory)
		if e != nil {
			return false, e
		}
		defer d.Close()
		var original backupCutoverIntent
		if err = d.ReadJSON(backupCutoverFile, &original); err != nil {
			return false, err
		}
		want := original.Spec
		want.Version = spec.Version
		want.Predecessor = notify.Hash(original)
		want.CompletedPolicyHash = j.Declaration.DesiredHash
		want.ConfigHash = notify.Hash(c)
		if spec.Version == 8 {
			want.HelperHash = j.Declaration.HelperSHA256
		}
		if original.Spec.Version != 5 || notify.Hash(want) != notify.Hash(spec) {
			return false, fmt.Errorf("shared policy inspection changed its original delivery evidence")
		}
	}
	if err = requireBackupCutoverOwner(state, owner); err != nil {
		return false, err
	}
	if deps.Completed == nil {
		return false, fmt.Errorf("completed report service inspection unavailable")
	}
	return true, deps.Completed(ctx, home, c, spec, owner)
}

// Completed retries only inspect. They cannot start a report, stop the gateway,
// republish a generation or deliver another provider diagnostic.
func verifyFreshCompletedReporting(ctx context.Context, home string, c WorkerInstallation, spec backupCutoverSpec, owner backupNotificationOwner) error {
	staged := false
	if spec.Version == 6 || spec.Version == 7 || spec.Version == 8 {
		state, e := openBackupState(backupCutoverPaths(home, c).State, false)
		if e != nil {
			return e
		}
		var installation string
		e = state.get("installation", &installation)
		state.Close()
		if e != nil {
			return e
		}
		staged = installation == "staged-awaiting-qualification"
		if (spec.Version == 7 || spec.Version == 8) && staged {
			root, e := safefs.Open(home)
			if e != nil {
				return e
			}
			var current WorkerSetupJournal
			e = root.ReadJSON(backupSetupFile, &current)
			var active bool
			if e == nil {
				active, e = reportingTriggersActive(root, current)
			}
			root.Close()
			if e != nil {
				return e
			}
			staged = !active
		}
		if installation != "staged-awaiting-qualification" && installation != "active" {
			return fmt.Errorf("updated shared installation state is not settled")
		}
	}
	paths := backupCutoverPaths(home, c)
	rootForRoute, err := safefs.Open(home)
	if err != nil {
		return err
	}
	var current WorkerSetupJournal
	err = rootForRoute.ReadJSON(backupSetupFile, &current)
	rootForRoute.Close()
	if err != nil {
		return err
	}
	if err = requireReportRouteOwner(current, owner); err != nil {
		return err
	}
	profile, err := install.InspectManagedReportProfile(spec.Managed.Root, owner.Binding.Identity, owner.Binding.Producer, current.Declaration.NotificationRoute)
	if err != nil {
		return err
	}
	if profile.UnitDirectory != paths.unitDirectory() {
		return fmt.Errorf("shared reporting gateway changed")
	}
	if profile.ManifestHash != spec.Managed.ManifestHash {
		if _, err = install.InspectCommandDeliveryContinuation(ctx, spec.Managed.Root, profile.ManifestHash, spec.Managed.ManifestHash); err != nil {
			return err
		}
	}
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := openBackupOwnedFile(root, paths.helperFile(), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 256<<20 || info.Mode().Perm()&0022 != 0 {
		f.Close()
		return fmt.Errorf("shared reporting helper is unsafe")
	}
	digest, err := safefs.DigestFile(f)
	f.Close()
	if err != nil || digest != spec.HelperHash {
		return fmt.Errorf("shared reporting helper changed")
	}
	units, err := renderBackupReportUnits(backupCutoverPaths(home, c), c.Policy, profile.Credentials)
	if err != nil {
		return err
	}
	manager := &backupCutoverManager{home: home, paths: paths, freshShared: true}
	if err = manager.requireNoDirectSenders(ctx); err != nil {
		return err
	}
	for name, body := range units {
		path := filepath.Join(paths.unitDirectory(), name)
		actual, e := readBackupOwnedFile(root, path, 1<<20)
		if e != nil || string(actual) != body {
			return fmt.Errorf("shared report unit changed")
		}
		if e = manager.inspect(ctx, name, path, staged, !staged && name != "deploy-backup-report.service"); e != nil {
			return e
		}
	}
	if owner.Binding.Identity != profile.Identity || !notify.ValidToken(owner.Binding.Generation) {
		return fmt.Errorf("shared report identity changed")
	}
	return verifyBackupCutoverGateway(ctx, owner)
}
