package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// A backup helper upgrade retains the source helpers and the gateway. Command
// receivers retain their own immutable permissions and require a capability check.
type sharedHelperReview struct {
	Before   helper.HelperArtifactManifest `json:"before"`
	Commands *sharedHelperCommands         `json:"commands,omitempty"`
}

func validateSharedHelperUpdate(v sharedPolicyUpdate, before WorkerInstallation) error {
	b, next := v.Before, v.After
	if v.Helper == nil || v.Helper.Before.Schema != 1 || v.Helper.Before.Role != "backup-worker" || v.Helper.Before.Pending != "" || v.Helper.Before.Current != b.Declaration.HelperSHA256 || next.Declaration.HelperSHA256 == b.Declaration.HelperSHA256 || !safefs.DigestPattern.MatchString(next.Declaration.HelperSHA256) {
		return fmt.Errorf("shared helper upgrade lost its original artifact ownership")
	}
	added, removed := setupSourceChanges(before, next)
	if len(added) != 0 || len(removed) != 0 || sourceEndpointsDiffer(before, next) || !reflect.DeepEqual(before.Policy, next.Policy) || !reflect.DeepEqual(b.Declaration.Sources, next.Declaration.Sources) {
		return fmt.Errorf("apply backup helper upgrades separately from source helpers, endpoints and policy")
	}
	declared := *next.Declaration.Desired.Backup
	declared.Artifacts = b.Declaration.Desired.Backup.Artifacts
	if !reflect.DeepEqual(declared, *b.Declaration.Desired.Backup) {
		return fmt.Errorf("apply helper upgrades separately from other YAML policy")
	}
	if v.Version == 8 {
		if err := validateSharedHelperCommands(b, v.Helper.Commands); err != nil {
			return err
		}
	} else if v.Helper.Commands != nil {
		return fmt.Errorf("command receiver requires its typed helper upgrade")
	}
	if n := b.Declaration.Desired.Notification; v.Version != 8 && n != nil && (n.Commands.Enabled || len(n.Commands.Workers) != 0 || len(n.Commands.Conversations) != 0 || len(n.Commands.Permissions) != 0) {
		return fmt.Errorf("command-bearing backups require a receiver-aware helper upgrade")
	}
	return config.PinnedWorkerImage(next.Image)
}

func requireSharedHelperCommandFree(paths InstallationPaths) error {
	d, err := safefs.Open(paths.State)
	if err != nil {
		return err
	}
	defer d.Close()
	for _, name := range []string{commandReceiverFile, "commands.lock"} {
		f, e := d.Open(name, os.O_RDONLY, 0)
		if e == nil {
			f.Close()
			return fmt.Errorf("retained command authority requires a receiver-aware helper upgrade")
		}
		if !os.IsNotExist(e) {
			return e
		}
	}
	return nil
}

func reviewSharedHelper(root *safefs.Directory, j WorkerSetupJournal, candidate string) (*sharedHelperReview, error) {
	paths, err := declarationPaths(j.Declaration.Home, j.Declaration)
	if err != nil {
		return nil, err
	}
	var original backupCutoverIntent
	if err = readBackupOwnedJSON(root, paths.OwnedPath(filepath.Join(paths.State, backupCutoverFile)), &original); err != nil {
		return nil, err
	}
	if j.Shared == nil || original.Spec.Version != 5 || original.Spec.Fresh == nil || notify.Hash(original) != j.Shared.Owner.CutoverHash || original.Spec.EvidenceHash != notify.Hash(original.Evidence) {
		return nil, fmt.Errorf("shared helper upgrade requires original fresh reporting ownership; recovered reporting needs its own migration")
	}
	commands, err := reviewSharedHelperCommands(paths, j, candidate)
	if err != nil {
		return nil, err
	}
	var manifest helper.HelperArtifactManifest
	if err = readBackupOwnedJSON(root, paths.OwnedPath(filepath.Join(paths.Installation, "artifact.json")), &manifest); err != nil {
		return nil, err
	}
	if manifest.Schema != 1 || manifest.Role != "backup-worker" || manifest.Current != j.Declaration.HelperSHA256 || manifest.Pending != "" {
		return nil, fmt.Errorf("backup helper ownership differs from its declaration")
	}
	file, err := openBackupOwnedFile(root, paths.OwnedPath(filepath.Join(paths.Installation, "deploy")), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	digest, err := safefs.DigestFile(file)
	file.Close()
	if err != nil {
		return nil, err
	}
	if digest != manifest.Current {
		return nil, fmt.Errorf("backup helper bytes changed outside setup")
	}
	return &sharedHelperReview{Before: manifest, Commands: commands}, nil
}

func captureSharedHelperCompletion(root *safefs.Directory, v sharedPolicyUpdate) (*helper.HelperArtifactManifest, error) {
	paths, err := declarationPaths(v.Before.Declaration.Home, v.After.Declaration)
	if err != nil {
		return nil, err
	}
	if v.Version == 8 {
		err = inspectSharedHelperCommands(paths, v.Before, v.Helper.Commands, true)
	} else {
		err = requireSharedHelperCommandFree(paths)
	}
	if err != nil {
		return nil, err
	}
	var manifest helper.HelperArtifactManifest
	if err = readBackupOwnedJSON(root, paths.OwnedPath(filepath.Join(paths.Installation, "artifact.json")), &manifest); err != nil {
		return nil, err
	}
	if err = validateSharedHelperCompletion(v, &manifest); err != nil {
		return nil, err
	}
	file, err := openBackupOwnedFile(root, paths.OwnedPath(filepath.Join(paths.Installation, "deploy")), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	digest, err := safefs.DigestFile(file)
	file.Close()
	if err != nil {
		return nil, err
	}
	if digest != manifest.Current {
		return nil, fmt.Errorf("published backup helper differs from its receipt")
	}
	return &manifest, nil
}

func validateSharedHelperCompletion(v sharedPolicyUpdate, after *helper.HelperArtifactManifest) error {
	if v.Helper == nil || after == nil {
		return fmt.Errorf("helper completion history missing")
	}
	expected := v.Helper.Before
	expected.Current, expected.Previous, expected.Pending = v.After.Declaration.HelperSHA256, expected.Current, ""
	if *after != expected {
		return fmt.Errorf("helper completion changed retained artifact ownership")
	}
	return nil
}

// A rollback keeps the upgrade operation pending, so old YAML cannot turn a
// partially attempted artifact migration into an apparently finished setup.
func requireSharedHelperRequest(j WorkerSetupJournal, request WorkerSetupRequest) error {
	if v := j.SharedPolicyUpdate; v != nil && sharedHelperVersion(v.Version) && workerSetupRequestHash(request) != workerSetupRequestHash(v.After) {
		return fmt.Errorf("resume the exact pending shared helper upgrade")
	}
	return nil
}

func sharedHelperVersion(version int) bool { return version == 7 || version == 8 }
