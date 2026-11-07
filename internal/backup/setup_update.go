package backup

import (
	"fmt"
	"reflect"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/safefs"
)

// Ordinary YAML edits can use the existing quiesce/update/rollback transaction.
// Source additions also retain a separate expansion intent. Endpoint moves retain
// the exact source/key/helper identities through read-only source inspection.
// Removal, replacement accounts, paths and delivery ownership need migration.
func validateDeclarativeBackupUpdate(before WorkerInstallation, previous *SetupDeclaration, request WorkerSetupRequest) error {
	d := request.Declaration
	if previous == nil || d == nil {
		return fmt.Errorf("existing backup installation requires explicit declarative adoption")
	}
	if d.UID != previous.UID || d.User != previous.User || d.Home != previous.Home || d.Architecture != previous.Architecture {
		return fmt.Errorf("backup target identity change requires explicit migration")
	}
	if d.Version != previous.Version || !reflect.DeepEqual(d.Paths, previous.Paths) || !reflect.DeepEqual(before.Paths, previous.Paths) {
		return fmt.Errorf("backup installation path changes require explicit migration")
	}
	if d.NotificationRoot != previous.NotificationRoot || d.NotificationRoute != previous.NotificationRoute || d.NotificationDesiredHash != previous.NotificationDesiredHash {
		return fmt.Errorf("shared reporting dependency changes require explicit migration")
	}
	if err := validateSourceRetirementPolicy(before, previous, request); err != nil {
		return err
	}
	old, next := before.Policy, request.Policy
	if old.Storage.Mode == "" {
		old.Storage.Mode = "mount"
	}
	if next.Storage.Mode == "" {
		next.Storage.Mode = "mount"
	}
	if changedBackupTelegram(old, next) {
		if err := validateBackupTelegramRotation(before, previous, request); err != nil {
			return err
		}
		next.Notifications.Telegram = old.Notifications.Telegram
	}
	added, removed := setupSourceChanges(before, request)
	retiring := len(removed) > 0
	if retiring {
		next.Sources = old.Sources
	}
	if len(added) > 0 {
		if old.Notifications.Signal != nil || old.Notifications.Mode == "shared" && (d.NotificationRoot == "" || d.NotificationDesiredHash == "") {
			return fmt.Errorf("source expansion requires unchanged declarative delivery ownership")
		}
		if before.Image != request.Image || before.RelayImage != request.RelayImage || previous.HelperSHA256 != d.HelperSHA256 {
			return fmt.Errorf("apply source additions separately from artifact updates")
		}
		for alias, pin := range previous.Sources {
			if !removed[alias] && d.Sources[alias] != pin {
				return fmt.Errorf("existing source artifacts must remain unchanged during enrollment")
			}
		}
		next.Sources = old.Sources
		if !reflect.DeepEqual(next, old) {
			return fmt.Errorf("apply source additions separately from other backup policy changes")
		}
	}
	next.Schedule, next.Timezone, next.Freshness = old.Schedule, old.Timezone, old.Freshness
	next.MaxRunTime, next.MaxSnapshotBytes, next.Timeouts = old.MaxRunTime, old.MaxSnapshotBytes, old.Timeouts
	next.Retention, next.RestoreChecks = old.Retention, old.RestoreChecks
	next.Storage.MinFreeBytes = old.Storage.MinFreeBytes
	next.Notifications.Failure, next.Notifications.RepeatAfter = old.Notifications.Failure, old.Notifications.RepeatAfter
	next.Notifications.Recovery, next.Notifications.DailyAt = old.Notifications.Recovery, old.Notifications.DailyAt
	// Monitoring references are runtime policy, not messenger delivery ownership.
	// Publication validates the private HTTPS inputs, retains the previous runtime
	// for rollback and returns schedules to staged qualification. Applying the
	// references does not send a heartbeat or qualify the replacement checks.
	next.Monitoring = old.Monitoring
	if !reflect.DeepEqual(next, old) {
		return fmt.Errorf("source, storage path or provider changes require explicit migration")
	}
	if len(request.Sources) != len(request.Policy.Sources) {
		return fmt.Errorf("source endpoints differ from policy")
	}
	seen := map[string]bool{}
	for _, s := range request.Sources {
		if seen[s.Alias] {
			return fmt.Errorf("duplicate source endpoint")
		}
		seen[s.Alias] = true
	}
	for _, alias := range request.Policy.Sources {
		if !seen[alias] {
			return fmt.Errorf("source endpoints differ from policy")
		}
	}
	for _, s := range before.Sources {
		found := false
		for _, proposed := range request.Sources {
			if proposed.Alias == s.Alias {
				found = true
				if proposed.Host != s.Host || proposed.User != s.User || proposed.Port != s.Port {
					if err := validateSourceEndpointMove(before, previous, request); err != nil {
						return err
					}
				}
			}
		}
		if !found && !retiring {
			return fmt.Errorf("source set change requires explicit enrollment migration")
		}
	}
	return nil
}

// Caller holds the setup lease; all predecessor checks precede the single intent write.
func beginWorkerSetupUpdate(root, stateRoot *safefs.Directory, journal *WorkerSetupJournal, request WorkerSetupRequest) error {
	if request.Declaration != nil {
		before, err := loadInstalledWorker(root.File().Name())
		if err != nil {
			return err
		}
		if err := validateDeclarativeBackupUpdate(before, journal.Declaration, request); err != nil {
			return err
		}
		if sourceEndpointsDiffer(before, request) {
			paths, err := declarationPaths(root.File().Name(), journal.Declaration)
			if err != nil {
				return err
			}
			if _, _, err = inspectSetupSourcePins(root, *journal, before, paths); err != nil {
				return err
			}
		}
		if !reflect.DeepEqual(before.Policy.Monitoring, request.Policy.Monitoring) {
			if err := notification.ValidateMonitoringChecks(request.Policy.Monitoring); err != nil {
				return err
			}
		}
		if err := prepareSharedPolicyHistory(root, journal, request); err != nil {
			return err
		}
		if err := prepareSetupSourceRetirement(root, journal, before, request); err != nil {
			return err
		}
		if err := prepareSetupSourceExpansion(root, journal, before, request); err != nil {
			return err
		}
		if err := prepareSetupTelegramRotation(journal, before, request); err != nil {
			return err
		}
	}
	for path, hash := range journal.OwnedFiles {
		body, e := readBackupOwnedFile(root, path, 8<<20)
		if e != nil || sharedSetupDigest(body) != hash {
			return fmt.Errorf("installed worker file drift: %s", path)
		}
	}
	journal.PreviousRequestHash = journal.RequestHash
	journal.PreviousDeclaration = journal.Declaration
	journal.Declaration = request.Declaration
	journal.RequestHash = workerSetupRequestHash(request)
	journal.Updating = true
	journal.Stage = "prepared"
	if err := stateRoot.WriteJSON("setup.json", journal, true); err != nil {
		return err
	}
	return nil
}

// Called only after qualification, runtime publication and the staged database marker.
func commitWorkerSetupJournal(stateRoot *safefs.Directory, journal *WorkerSetupJournal) error {
	completed := *journal
	completed.Stage = "complete"
	completed.RecoveryNeeded = false
	completed.BeforeHelper = nil
	completed.BeforeInstallation = ""
	completed.CandidateHelper = ""
	completed.Updating = false
	completed.BeforeControl = nil
	completed.BeforeMinFreeBytes = nil
	completed.PreviousRequestHash = ""
	completed.PreviousDeclaration = nil
	completed.SourceExpansion = nil
	completed.TelegramRotation = nil
	completed.EnabledUnits = nil
	completed.OwnedFiles = map[string]string{}
	for path, hash := range journal.OwnedFiles {
		completed.OwnedFiles[path] = hash
	}
	for path, change := range journal.Files {
		completed.OwnedFiles[path] = change.AfterHash
	}
	completed.Files = map[string]setupFileChange{}
	if err := commitSharedPolicyHistory(stateRoot, *journal, &completed, nil); err != nil {
		return err
	}
	if err := stateRoot.WriteJSON("setup.json", completed, true); err != nil {
		return err
	}
	*journal = completed
	return nil
}
