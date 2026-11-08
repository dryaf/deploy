package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/safefs"
)

var ErrSourceReplacementPending = errors.New("source replacement is pending; resume the exact YAML and retain both new and old source endpoints until enrollment and revocation are acknowledged")

var ErrSourceRetirementPending = errors.New("source retirement is pending; resume the exact YAML and retain the old source administrator endpoints until revocation is acknowledged")

// SetupSourceRetirement is non-secret, retained authority for one revocation.
// The YAML reason remains part of desired policy after completion.
type SetupSourceRetirement struct {
	Target       SetupSourceTarget `json:"target"`
	SourceID     string            `json:"source_id"`
	Fingerprint  string            `json:"fingerprint"`
	HelperSHA256 string            `json:"helper_sha256"`
	Reason       string            `json:"reason"`
}

type setupSourceRetirement struct {
	Version      int                              `json:"version"`
	Before       WorkerSetupJournal               `json:"before"`
	Installation WorkerInstallation               `json:"installation"`
	Request      WorkerSetupRequest               `json:"request"`
	Sources      map[string]SetupSourceRetirement `json:"sources"`
}

func validateSourceRetirementPolicy(before WorkerInstallation, previous *SetupDeclaration, r WorkerSetupRequest) error {
	if previous == nil || previous.Desired == nil || previous.Desired.Backup == nil || r.Declaration == nil || r.Declaration.Desired == nil || r.Declaration.Desired.Backup == nil {
		if len(r.Sources) >= len(before.Sources) {
			return nil
		}
		return fmt.Errorf("source retirement requires retained YAML policy")
	}
	old, next := previous.Desired.Backup, r.Declaration.Desired.Backup
	removed := map[string]bool{}
	active := map[string]SetupSource{}
	for _, s := range r.Sources {
		active[s.Alias] = s
	}
	for _, s := range before.Sources {
		if _, ok := active[s.Alias]; !ok {
			removed[s.Alias] = true
		}
	}
	for alias, reason := range old.RetireSources {
		if next.RetireSources[alias] != reason {
			return fmt.Errorf("retain completed source retirement reasons in YAML")
		}
	}
	for alias, reason := range next.RetireSources {
		if strings.TrimSpace(reason) == "" || len(reason) > 1024 || strings.ContainsAny(reason, "\x00\r\n") {
			return fmt.Errorf("source retirement requires a bounded single-line reason")
		}
		if _, ok := active[alias]; ok {
			return fmt.Errorf("retired source cannot be reactivated by enrollment")
		}
		if old.RetireSources[alias] == "" && !removed[alias] {
			return fmt.Errorf("retirement names a source outside the retained installation")
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if len(r.Sources) == 0 || before.Policy.Notifications.Signal != nil {
		return fmt.Errorf("source retirement requires remaining sources and unchanged declarative reporting ownership")
	}
	if before.Image != r.Image || before.RelayImage != r.RelayImage || previous.HelperSHA256 != r.Declaration.HelperSHA256 {
		return fmt.Errorf("apply source retirement separately from artifact updates")
	}
	for _, s := range before.Sources {
		if removed[s.Alias] {
			if next.RetireSources[s.Alias] == "" {
				return fmt.Errorf("removed source requires backup_worker.retire_sources reason")
			}
		} else {
			n := active[s.Alias]
			if n.Host != s.Host || n.User != s.User || n.Port != s.Port || r.Declaration.Sources[s.Alias] != previous.Sources[s.Alias] {
				return fmt.Errorf("retain remaining source endpoints and artifacts during retirement")
			}
		}
	}
	policy := r.Policy
	policy.Sources = before.Policy.Sources
	if !reflect.DeepEqual(policy, before.Policy) {
		return fmt.Errorf("apply source retirement separately from other policy edits")
	}
	copy := *next
	copy.Sources, copy.RetireSources = old.Sources, old.RetireSources
	if !reflect.DeepEqual(copy, *old) {
		return fmt.Errorf("apply source retirement separately from other YAML edits")
	}
	return nil
}

func prepareSetupSourceRetirement(root *safefs.Directory, j *WorkerSetupJournal, before WorkerInstallation, r WorkerSetupRequest) error {
	if j.SourceRetirement != nil {
		return checkSetupSourceRetirement(root, *j, &r)
	}
	added, removed := setupSourceChanges(before, r)
	if len(removed) == 0 {
		return nil
	}
	if err := validateSourceRetirementPolicy(before, j.Declaration, r); err != nil {
		return err
	}
	if err := verifySetupSourceKeys(root, *j); err != nil {
		return err
	}
	paths, err := declarationPaths(root.File().Name(), j.Declaration)
	if err != nil {
		return err
	}
	_, trust, err := inspectSetupSourcePins(root, *j, before, paths)
	if err != nil {
		return err
	}
	for _, s := range r.Sources {
		for _, old := range before.Sources {
			if s.Alias == old.Alias && trust[paths.OwnedPath(old.KnownHosts)] != sharedSetupDigest([]byte(s.KnownHosts)) {
				return fmt.Errorf("retain source trust during retirement")
			}
		}
	}
	v := setupSourceRetirement{Version: 1, Before: *j, Installation: before, Request: r, Sources: map[string]SetupSourceRetirement{}}
	if len(added) > 0 {
		v.Version = 2
	}
	for _, s := range before.Sources {
		if r.Declaration.Sources[s.Alias] == "" {
			v.Sources[s.Alias] = SetupSourceRetirement{Target: SetupSourceTarget{Alias: s.Alias, Host: s.Host, User: s.User, Port: s.Port}, SourceID: s.SourceID, Fingerprint: j.SourceKeys[s.Alias], HelperSHA256: j.Declaration.Sources[s.Alias], Reason: r.Declaration.Desired.Backup.RetireSources[s.Alias]}
		}
	}
	if err = v.validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var retained setupSourceRetirement
	if err = json.Unmarshal(raw, &retained); err != nil {
		return err
	}
	j.SourceRetirement = &retained
	return nil
}

func (v setupSourceRetirement) validate() error {
	b, r := v.Before, v.Request
	added, removed := setupSourceChanges(v.Installation, r)
	if (v.Version != 1 && v.Version != 2) || (v.Version == 1 && len(added) > 0) || (v.Version == 2 && len(added) == 0) || b.SourceRetirement != nil || b.SourceExpansion != nil || b.TelegramRotation != nil || (b.Schema != 1 || b.Shared != nil) && (b.Schema != 2 || b.Shared == nil) || b.Stage != "complete" || b.Updating || b.RecoveryNeeded || !b.StateInitialized || b.Declaration == nil || r.Declaration == nil || len(v.Sources) == 0 || b.WorkerID != v.Installation.WorkerID || b.ArchiveID != v.Installation.ArchiveID || !safefs.ValidID(b.WorkerID, "w") || !safefs.ValidID(b.ArchiveID, "a") || !safefs.DigestPattern.MatchString(b.RequestHash) {
		return fmt.Errorf("invalid source retirement authority")
	}
	if v.Installation.Policy.Notifications.Mode == "shared" {
		if b.Producer == nil || b.Producer.Validate(b.WorkerID) != nil || b.Declaration.NotificationDesiredHash == "" || b.Shared != nil && (b.Shared.Owner.Binding.Producer != b.Producer.Producer || b.Shared.Owner.Binding.Generation != b.Producer.Generation || b.Shared.GatewayRoot != b.Declaration.NotificationRoot || !reportRouteOwnerShape(b, b.Shared.Owner)) {
			return fmt.Errorf("shared source retirement requires retained producer and delivery ownership")
		}
	} else if b.Shared != nil || b.Producer != nil {
		return fmt.Errorf("standalone source retirement has shared ownership history")
	}

	if err := validateSetupDesired(b.Declaration); err != nil {
		return err
	}
	if err := validateSetupDesired(r.Declaration); err != nil {
		return err
	}
	if err := validateDeclarativeBackupUpdate(v.Installation, b.Declaration, r); err != nil {
		return err
	}
	if len(removed) != len(v.Sources) || len(b.SourceKeys) != len(v.Installation.Sources) || len(b.SourceEnrollments) != len(b.SourceKeys) {
		return fmt.Errorf("incomplete source retirement pins")
	}
	for _, s := range v.Installation.Sources {
		e := b.SourceEnrollments[s.Alias]
		if e.Schema != 1 || e.WorkerID != b.WorkerID || e.SourceID != s.SourceID || !safefs.ValidID(s.SourceID, "s") || e.Fingerprint == "" || e.Fingerprint != b.SourceKeys[s.Alias] {
			return fmt.Errorf("retirement predecessor enrollment differs")
		}
		if r.Declaration.Sources[s.Alias] == "" {
			want := SetupSourceRetirement{Target: SetupSourceTarget{s.Alias, s.Host, s.User, s.Port}, SourceID: s.SourceID, Fingerprint: e.Fingerprint, HelperSHA256: b.Declaration.Sources[s.Alias], Reason: r.Declaration.Desired.Backup.RetireSources[s.Alias]}
			if v.Sources[s.Alias] != want || !safefs.DigestPattern.MatchString(want.HelperSHA256) {
				return fmt.Errorf("source retirement changed its retained destination")
			}
		}
	}
	return nil
}

func checkSetupSourceRetirement(root *safefs.Directory, j WorkerSetupJournal, request *WorkerSetupRequest) error {
	if j.SourceRetirement == nil {
		return validateSourceRetirementHistory(j)
	}
	v := *j.SourceRetirement
	if err := v.validate(); err != nil {
		return err
	}
	if err := validateSourceRetirementHistory(v.Before); err != nil {
		return err
	}
	after := workerSetupRequestHash(v.Request)
	if request != nil && workerSetupRequestHash(*request) != after {
		return fmt.Errorf("source retirement is pending; resume the exact YAML disposition, endpoints and artifacts")
	}
	if j.ID != v.Before.ID || j.WorkerID != v.Before.WorkerID || j.ArchiveID != v.Before.ArchiveID || !reflect.DeepEqual(j.Shared, v.Before.Shared) || !reflect.DeepEqual(j.Producer, v.Before.Producer) || !reflect.DeepEqual(j.RetiredSources, v.Before.RetiredSources) {
		return fmt.Errorf("source retirement lost retained identity or history")
	}
	if v.Version == 1 && (!reflect.DeepEqual(j.SourceKeys, v.Before.SourceKeys) || !reflect.DeepEqual(j.SourceEnrollments, v.Before.SourceEnrollments)) {
		return fmt.Errorf("source retirement changed existing enrollment")
	}
	switch j.RequestHash {
	case after:
		if !reflect.DeepEqual(j.Declaration, v.Request.Declaration) || j.Updating && (!reflect.DeepEqual(j.PreviousDeclaration, v.Before.Declaration) || j.PreviousRequestHash != v.Before.RequestHash) || !j.Updating && j.Stage != "complete" {
			return fmt.Errorf("source retirement candidate authority differs")
		}
	case v.Before.RequestHash:
		if j.Updating || j.Stage != "complete" || !reflect.DeepEqual(j.Declaration, v.Before.Declaration) {
			return fmt.Errorf("source retirement rollback is incomplete")
		}
	default:
		return fmt.Errorf("source retirement belongs to another setup revision")
	}
	if v.Version == 2 {
		return verifyCombinedSourceKeys(root, j, v.Before, v.Request)
	}
	view := j
	view.SourceRetirement, view.Declaration = nil, v.Before.Declaration
	return verifySetupSourceKeys(root, view)
}

func validateSourceRetirementHistory(j WorkerSetupJournal) error {
	if j.Declaration == nil || j.Declaration.Desired == nil || j.Declaration.Desired.Backup == nil {
		if len(j.RetiredSources) == 0 {
			return nil
		}
		return fmt.Errorf("retirement history lost its declaration")
	}
	reasons := j.Declaration.Desired.Backup.RetireSources
	if len(reasons) != len(j.RetiredSources) {
		return fmt.Errorf("source retirement history differs from YAML dispositions")
	}
	for alias, s := range j.RetiredSources {
		if s.Target.Alias != alias || s.Reason == "" || reasons[alias] != s.Reason || !safefs.ValidID(s.SourceID, "s") || s.Fingerprint == "" || !safefs.DigestPattern.MatchString(s.HelperSHA256) || j.SourceKeys[alias] != "" || j.Declaration.Sources[alias] != "" {
			return fmt.Errorf("completed source retirement authority changed")
		}
	}
	return nil
}

// Runtime publication happens first, with schedules staged. Disposition is then
// committed to the archive before asking the operator to revoke remote access.
// A disconnect leaves a pending intent that fences activation and other edits.
func prepareSourceRetirementDisposition(home string, root *safefs.Directory, j WorkerSetupJournal) error {
	if err := checkSetupSourceRetirement(root, j, nil); err != nil {
		return err
	}
	if j.SourceRetirement == nil || j.Updating || j.RecoveryNeeded || j.Stage != "complete" || j.RequestHash != workerSetupRequestHash(j.SourceRetirement.Request) {
		return fmt.Errorf("publish remaining source policy before retirement")
	}
	if err := requireWorkerSetupOwnership(home, &j); err != nil {
		return err
	}

	for path, hash := range j.OwnedFiles {
		body, err := readBackupOwnedFile(root, path, 8<<20)
		if err != nil || sharedSetupDigest(body) != hash {
			return fmt.Errorf("retirement runtime or owned file drift")
		}
	}
	installed, err := loadInstalledWorker(home)
	if err != nil || installed.WorkerID != j.WorkerID || installed.ArchiveID != j.ArchiveID || !reflect.DeepEqual(installed.Policy, j.SourceRetirement.Request.Policy) {
		return fmt.Errorf("retirement runtime no longer matches published policy")
	}
	state, err := openInstalledBackupStateWithLease(home, false, false, true)
	if err != nil {
		return err
	}
	defer state.Close()
	archive, err := openBackupArchive(j.ArchivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	release, err := lockWorkerInstallation(state, archive)
	if err != nil {
		return err
	}
	defer release()
	dir, err := safefs.Open(state.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	lease, err := dir.Lock("notifier.lock", true, true)
	if err != nil {
		return err
	}
	defer lease.Close()
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	if control.WriterID != j.WorkerID || control.ArchiveID != j.ArchiveID || control.ConfigHash != installed.ConfigHash {
		return fmt.Errorf("source retirement archive identity differs")
	}
	changed := false
	for _, s := range j.SourceRetirement.Sources {
		if reason := control.RetiredSources[s.SourceID]; reason != "" && reason != s.Reason {
			return fmt.Errorf("source already has a different retirement disposition")
		}
		changed = changed || control.RetiredSources[s.SourceID] == ""
	}
	if changed {
		err = archive.updateControl(state, func(c *ArchiveControl) error {
			if c.RetiredSources == nil {
				c.RetiredSources = map[string]string{}
			}
			for _, s := range j.SourceRetirement.Sources {
				c.RetiredSources[s.SourceID] = s.Reason
				for key, instance := range c.Instances {
					if instance.SourceID == s.SourceID {
						instance.Retired, instance.RetirementReason = true, s.Reason
						c.Instances[key] = instance
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return state.reconcileRetiredIncidents(time.Now().UTC())
}

func commitSourceRetirement(stateRoot *safefs.Directory, j *WorkerSetupJournal, results map[string]source.SourceEnrollmentResult) error {
	if j.SourceRetirement == nil || j.Stage != "complete" || j.Updating || j.RequestHash != workerSetupRequestHash(j.SourceRetirement.Request) || len(results) != len(j.SourceRetirement.Sources) {
		return fmt.Errorf("incomplete source retirement acknowledgements")
	}
	for alias, s := range j.SourceRetirement.Sources {
		if results[alias] != (source.SourceEnrollmentResult{Schema: 1, SourceID: s.SourceID, WorkerID: j.WorkerID}) {
			return fmt.Errorf("source retirement acknowledgement differs")
		}
	}
	completed := *j
	completed.SourceKeys = maps.Clone(j.SourceKeys)
	completed.SourceEnrollments = maps.Clone(j.SourceEnrollments)
	completed.RetiredSources = maps.Clone(j.RetiredSources)
	if completed.RetiredSources == nil {
		completed.RetiredSources = map[string]SetupSourceRetirement{}
	}
	for alias, s := range j.SourceRetirement.Sources {
		completed.RetiredSources[alias] = s
		delete(completed.SourceKeys, alias)
		delete(completed.SourceEnrollments, alias)
	}
	completed.SourceRetirement = nil
	if err := commitSharedPolicyHistory(stateRoot, *j, &completed, results); err != nil {
		return err
	}
	if err := stateRoot.WriteJSON("setup.json", completed, true); err != nil {
		return err
	}
	*j = completed
	return nil
}

func finishSourceRetirementExchange(home string, root, stateRoot *safefs.Directory, j *WorkerSetupJournal) error {
	if err := prepareSourceRetirementDisposition(home, root, *j); err != nil {
		return err
	}
	if err := SendSetupFrame(WorkerSetupResponse{Schema: 1, Stage: "retiring-sources", WorkerID: j.WorkerID, ArchiveID: j.ArchiveID, RetiringSources: j.SourceRetirement.Sources}); err != nil {
		return err
	}
	raw, err := protocol.ReadBackupFrame(os.Stdin)
	if err != nil {
		return err
	}
	var step WorkerSetupStep
	if err = protocol.StrictBackupJSON(raw, &step); err != nil {
		return err
	}
	if step.Operation != "sources-retired" || step.Alias != "" || step.Enrollment != nil {
		return fmt.Errorf("expected source retirement acknowledgements")
	}
	if err = commitSourceRetirement(stateRoot, j, step.RetiredSources); err != nil {
		return err
	}
	return SendSetupFrame(WorkerSetupResponse{Schema: 1, Stage: "complete", WorkerID: j.WorkerID, ArchiveID: j.ArchiveID})
}
