package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// The setup journal freezes notification ownership separately from backup
// configuration. Neither an update nor its rollback may transfer that ownership.
type sharedWorkerSetup struct {
	Version     int                          `json:"version"`
	Owner       backupNotificationOwner      `json:"owner"`
	GatewayRoot string                       `json:"gateway_root"`
	Profile     install.ManagedReportProfile `json:"profile"`
}

func requireWorkerSetupOwner(state *backupState, j *WorkerSetupJournal) error {
	if j.Shared == nil {
		return requireLegacyNotificationOwner(state)
	}
	p := j.Shared
	if j.Schema != 2 || p.Version != 1 || p.Owner.Mode != "shared" || p.Owner.CutoverHash == "" || p.Owner.Binding.Producer.ID != j.WorkerID || p.Owner.Binding.Archive != j.ArchiveID || p.Profile.Identity != p.Owner.Binding.Identity || !safefs.DigestPattern.MatchString(p.Profile.ManifestHash) || !backupRecoveryPath(p.GatewayRoot) {
		return fmt.Errorf("invalid shared setup ownership")
	}
	var owner backupNotificationOwner
	err := backupTransferTx(context.Background(), state, func(tx *sql.Tx) error { var e error; owner, e = readBackupNotificationOwner(tx); return e })
	if err != nil {
		return err
	}
	if owner != p.Owner {
		return fmt.Errorf("shared notification owner changed during setup")
	}
	return requireBackupCutoverOwner(state, owner)
}

func requireWorkerSetupOwnership(home string, j *WorkerSetupJournal) error {
	if j.Shared == nil {
		return requireLegacyWorkerSetup(home, j)
	}
	state, err := openBackupStateWithLease(filepath.Join(home, ".local/state/deploy/backup-worker/state"), false, false, true)
	if err != nil {
		return err
	}
	defer state.Close()
	return requireWorkerSetupOwner(state, j)
}

// Capture only a completed installation. An old unfinished legacy setup cannot
// acquire a shared owner retroactively and use its rollback to restart senders.
func prepareSharedWorkerSetup(home string, root, stateRoot *safefs.Directory, j *WorkerSetupJournal, operation string) error {
	return prepareSharedWorkerSetupUsing(home, root, stateRoot, j, operation, install.InspectManagedReportProfile)
}

func prepareSharedWorkerSetupUsing(home string, root, stateRoot *safefs.Directory, j *WorkerSetupJournal, operation string, inspect func(string, notify.Identity, notify.Producer, string) (install.ManagedReportProfile, error)) error {
	if (operation != "update" && operation != "setup" && operation != "") || j.Stage != "complete" || j.RecoveryNeeded || j.Updating {
		return nil
	}
	state, err := openBackupStateWithLease(filepath.Join(home, ".local/state/deploy/backup-worker/state"), false, false, true)
	if err != nil {
		return err
	}
	defer state.Close()
	var present int
	if err = state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_owner'").Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	var owner backupNotificationOwner
	err = backupTransferTx(context.Background(), state, func(tx *sql.Tx) error { var e error; owner, e = readBackupNotificationOwner(tx); return e })
	if err != nil {
		return err
	}
	if owner.Mode != "shared" || owner.CutoverHash == "" {
		return fmt.Errorf("shared setup requires completed notification cutover")
	}
	if err = requireBackupCutoverOwner(state, owner); err != nil {
		return err
	}
	d, err := safefs.Open(state.directory)
	if err != nil {
		return err
	}
	defer d.Close()
	var cutover backupCutoverIntent
	if err = d.ReadJSON(backupCutoverFile, &cutover); err != nil {
		return err
	}
	profile, err := inspect(cutover.Spec.Managed.Root, owner.Binding.Identity, owner.Binding.Producer, owner.Binding.Route)
	if err != nil {
		return err
	}
	if profile.UnitDirectory != filepath.Join(home, ".config/systemd/user") {
		return fmt.Errorf("shared report manager differs from backup manager")
	}
	c, err := loadWorkerInstallation(filepath.Join(home, ".config/deploy/backup-worker/runtime.json"))
	if err != nil {
		return err
	}
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	if c.WorkerID != j.WorkerID || c.ArchiveID != j.ArchiveID || control.WriterID != c.WorkerID || control.ArchiveID != c.ArchiveID || control.ConfigHash != c.ConfigHash {
		return fmt.Errorf("shared setup installation/control changed")
	}
	units, err := renderBackupReportUnits(backupCutoverPaths(home, c), c.Policy, profile.Credentials)
	if err != nil {
		return err
	}
	owned := map[string]string{}
	for path, hash := range j.OwnedFiles {
		body, e := safefs.ReadFile(root, path, 8<<20)
		if e != nil || sharedSetupDigest(body) != hash {
			return fmt.Errorf("shared setup owned file changed: %s", path)
		}
		owned[path] = hash
	}
	for name, body := range units {
		path := filepath.Join(".config/systemd/user", name)
		actual, e := safefs.ReadFile(root, path, 1<<20)
		if e != nil || string(actual) != body {
			return fmt.Errorf("shared report unit changed: %s", name)
		}
		owned[path] = sharedSetupDigest(actual)
	}
	next := *j
	next.Schema = 2
	next.Shared = &sharedWorkerSetup{Version: 1, Owner: owner, GatewayRoot: cutover.Spec.Managed.Root, Profile: profile}
	next.OwnedFiles = owned
	if err = requireWorkerSetupOwner(state, &next); err != nil {
		return err
	}
	if err = stateRoot.WriteJSON("setup.json", next, true); err != nil {
		return err
	}
	*j = next
	return nil
}

func workerSetupUnits(c WorkerInstallation, paths WorkerUnitPaths, j *WorkerSetupJournal) (map[string]string, error) {
	units, err := renderWorkerUnits(c, paths, c.Image, c.RelayImage)
	if err != nil {
		return nil, err
	}
	if j.Shared == nil {
		return units, nil
	}
	if paths.State != filepath.Join(paths.Home, ".local/state/deploy/backup-worker/state") || j.Shared.Profile.UnitDirectory != filepath.Join(paths.Home, ".config/systemd/user") {
		return nil, fmt.Errorf("shared setup paths changed")
	}
	reports, err := renderBackupReportUnits(paths, c.Policy, j.Shared.Profile.Credentials)
	if err != nil {
		return nil, err
	}
	for name, body := range reports {
		units[name] = body
	}
	return units, nil
}

func sharedSetupDigest(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func currentSharedBackupOwner(state *backupState) (*backupNotificationOwner, error) {
	var present int
	if err := state.db.QueryRow("SELECT count(*) FROM meta WHERE key='notification_owner'").Scan(&present); err != nil {
		return nil, err
	}
	if present == 0 {
		return nil, requireLegacyNotificationOwner(state)
	}
	var owner backupNotificationOwner
	err := backupTransferTx(context.Background(), state, func(tx *sql.Tx) error { var e error; owner, e = readBackupNotificationOwner(tx); return e })
	if err != nil {
		return nil, fmt.Errorf("notification ownership is fenced: %w", err)
	}
	if owner.Mode != "shared" || owner.CutoverHash == "" {
		return nil, fmt.Errorf("backup lifecycle remains fenced until shared notification ownership is complete")
	}
	if err = requireBackupCutoverOwner(state, owner); err != nil {
		return nil, err
	}
	return &owner, nil
}

func requireSharedBackupReportUnits(home string, c WorkerInstallation, state *backupState, owner backupNotificationOwner) error {
	d, err := safefs.Open(state.directory)
	if err != nil {
		return err
	}
	defer d.Close()
	var cutover backupCutoverIntent
	if err = d.ReadJSON(backupCutoverFile, &cutover); err != nil {
		return err
	}
	profile, err := install.InspectManagedReportProfile(cutover.Spec.Managed.Root, owner.Binding.Identity, owner.Binding.Producer, owner.Binding.Route)
	if err != nil {
		return err
	}
	if profile.UnitDirectory != filepath.Join(home, ".config/systemd/user") {
		return fmt.Errorf("report manager changed")
	}
	units, err := renderBackupReportUnits(backupCutoverPaths(home, c), c.Policy, profile.Credentials)
	if err != nil {
		return err
	}
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	for name, body := range units {
		raw, err := safefs.ReadFile(root, filepath.Join(".config/systemd/user", name), 1<<20)
		if err != nil || string(raw) != body {
			return fmt.Errorf("report unit changed: %s", name)
		}
	}
	return nil
}
