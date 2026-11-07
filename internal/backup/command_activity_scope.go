package backup

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func requireCommandScopeOwner(state *backupState, j WorkerSetupJournal) error {
	owner, err := currentSharedBackupOwner(state)
	if err != nil {
		return err
	}
	if owner == nil || j.Producer == nil || owner.Binding.Producer != j.Producer.Producer || owner.Binding.Generation != j.Producer.Generation || owner.Binding.Archive != j.ArchiveID || j.Declaration == nil || owner.Binding.Route != j.Declaration.NotificationRoute {
		return fmt.Errorf("command activity requires the completed shared reporting owner")
	}
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	var completed string
	if err = state.get("notification_cutover_complete", &completed); err != nil || completed != owner.CutoverHash || !control.SharedRequired || control.ReportingDisabled || control.WriterID != j.WorkerID || control.ArchiveID != j.ArchiveID {
		return fmt.Errorf("command activity lost its completed backup delivery fence")
	}
	return requireBackupActivityReady(state)
}

// commandScopeOrigin proves that only command policy or LED output permission changed since an
// earlier reporting scope. It cannot normalize unrelated backup updates, recover
// missing history, or turn a later desired declaration into the original proof.
func commandScopeOrigin(root *safefs.Directory, j WorkerSetupJournal, scope string) (WorkerSetupJournal, error) {
	if !safefs.DigestPattern.MatchString(scope) {
		return j, fmt.Errorf("command scope origin requires an exact scope")
	}
	for depth := 0; depth < 128; depth++ {
		if j.Declaration == nil || j.NotificationScope != nil {
			return j, fmt.Errorf("finish pending command activity before reporting")
		}
		if j.Declaration.NotificationDesiredHash == scope {
			return j, nil
		}
		if update, handled, err := sharedPolicyPredecessor(root, j); err != nil {
			return j, err
		} else if handled {
			j = update.Intent.Before
			continue
		}
		if err := verifyNotificationScopeHistory(root, j); err != nil {
			return j, err
		}
		ref := j.NotificationScopeHistory
		if ref == nil {
			return j, fmt.Errorf("reporting scope has no retained command continuation")
		}
		var receipt setupNotificationScopeReceipt
		if err := root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+ref.Operation+".json", &receipt); err != nil {
			return j, err
		}
		v := receipt.Intent
		if ((v.Version < 2 || v.Version > 5) && !reportPolicyScope(v.Version) && v.Version != 22) || v.validate() != nil || receipt.Version != 1 || notify.Hash(receipt) != ref.Receipt {
			return j, fmt.Errorf("reporting scope is not a command/output-only continuation")
		}
		want := v.Before
		want.Declaration, want.RequestHash = v.After.Declaration, workerSetupRequestHash(v.After)
		want.NotificationScopeHistory = ref
		if v.Version == 23 {
			want.OwnedFiles = v.freshnessOwnedFiles()
		}
		if reportPolicyScope(v.Version) {
			want = v.providerReportsAfter(want, receipt.Gateway)
		}
		if !reflect.DeepEqual(want, j) {
			if err := verifyCommandScopeCapture(root, j, want, receipt.Gateway); err != nil {
				return j, err
			}
		}
		j = v.Before
	}
	return j, fmt.Errorf("command activity history exceeds the review bound")
}

// An unchanged backup apply records the already completed shared owner and its
// report units. This is observation metadata, not a new backup policy. Accept
// exactly that capture after proving it against the current gateway and files;
// every other setup field must still match the immutable scope receipt.
func verifyCommandScopeCapture(root *safefs.Directory, current, want WorkerSetupJournal, gateway string) error {
	s := current.Shared
	if current.Schema != 2 || s == nil || s.Version != 1 || s.GatewayRoot != current.Declaration.NotificationRoot {
		return fmt.Errorf("apply backup changes separately from retained command activity")
	}
	state, err := openInstalledBackupState(current.Declaration.Home, false)
	if err != nil {
		return err
	}
	owner, err := currentSharedBackupOwner(state)
	if err == nil {
		err = requireCommandScopeOwner(state, current)
	}
	state.Close()
	if err != nil {
		return err
	}
	if owner == nil || *owner != s.Owner {
		return fmt.Errorf("command scope shared capture changed reporting owner")
	}
	profile, err := install.InspectManagedReportProfile(s.GatewayRoot, owner.Binding.Identity, owner.Binding.Producer, owner.Binding.Route)
	if err != nil {
		return err
	}
	// Earlier command scopes can contain a shared snapshot captured before a
	// later toggle or permission reduction. Prove that exact command-only
	// ancestry before comparing the unchanged reporting fields.
	if s.Profile.ManifestHash != gateway {
		if _, err = install.InspectCapturedDeliveryContinuation(context.Background(), s.GatewayRoot, profile.ManifestHash, s.Profile.ManifestHash, gateway); err != nil {
			return err
		}
	}
	observed := profile
	if observed.ManifestHash != s.Profile.ManifestHash {
		if _, err = install.InspectCommandDeliveryContinuation(context.Background(), s.GatewayRoot, observed.ManifestHash, s.Profile.ManifestHash); err != nil {
			return err
		}
		observed.ManifestHash = s.Profile.ManifestHash
	}
	if !reflect.DeepEqual(observed.Credentials, s.Profile.Credentials) {
		if _, err = install.InspectCapturedReportProfile(context.Background(), s.GatewayRoot, profile.ManifestHash, s.Profile); err != nil {
			return err
		}
		observed.Credentials = s.Profile.Credentials
	}
	if !reflect.DeepEqual(observed, s.Profile) {
		return fmt.Errorf("command scope shared profile changed")
	}
	c, err := loadInstalledWorker(current.Declaration.Home)
	if err != nil {
		return err
	}
	paths := backupCutoverPaths(current.Declaration.Home, c)
	if profile.UnitDirectory != paths.unitDirectory() {
		return fmt.Errorf("command scope report manager changed")
	}
	units, err := renderBackupReportUnits(paths, c.Policy, profile.Credentials)
	if err != nil {
		return err
	}
	capturedUnits, err := renderBackupReportUnits(paths, c.Policy, s.Profile.Credentials)
	if err != nil {
		return err
	}
	owned := make(map[string]string, len(want.OwnedFiles)+len(units))
	for path, hash := range want.OwnedFiles {
		owned[path] = hash
	}
	for name, body := range units {
		path := filepath.Join(paths.unitDirectory(), name)
		if c.Paths != nil {
			path = c.Paths.OwnedPath(path)
		} else {
			path, err = filepath.Rel(current.Declaration.Home, path)
			if err != nil {
				return err
			}
		}
		raw, e := readBackupOwnedFile(root, path, 1<<20)
		if e != nil || string(raw) != body {
			return fmt.Errorf("command scope report unit changed")
		}
		owned[path] = sharedSetupDigest([]byte(capturedUnits[name]))
	}
	want.Schema, want.Shared, want.OwnedFiles = current.Schema, current.Shared, owned
	if !reflect.DeepEqual(want, current) {
		return fmt.Errorf("apply backup changes separately from retained command activity")
	}
	return nil
}
