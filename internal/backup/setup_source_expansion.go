package backup

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/safefs"
)

// Enrollment is external to the local file rollback. Keep its exact request and
// predecessor until installation commits, including when recovery restores the
// old runtime. A rollback must never forget a newly authorized key or allow its
// alias to be redirected to another machine. No secret values are stored here.
type setupSourceExpansion struct {
	Version      int                `json:"version"`
	Before       WorkerSetupJournal `json:"before"`
	Installation WorkerInstallation `json:"installation"`
	Request      WorkerSetupRequest `json:"request"`
}

func (v setupSourceExpansion) validate() error {
	b, r := v.Before, v.Request
	added, removed := setupSourceChanges(v.Installation, r)
	if (v.Version != 1 && v.Version != 2) || (v.Version == 1 && len(removed) > 0) || (v.Version == 2 && len(removed) == 0) || b.SourceRetirement != nil || b.SourceExpansion != nil || b.Declaration == nil || r.Declaration == nil || (b.Schema != 1 || b.Shared != nil) && (b.Schema != 2 || b.Shared == nil) || b.Stage != "complete" || !b.StateInitialized || b.Updating || b.RecoveryNeeded || len(added) == 0 {
		return fmt.Errorf("invalid source expansion authority")
	}
	if v.Installation.Policy.Notifications.Mode == "shared" {
		if b.Producer == nil || b.Producer.Validate(b.WorkerID) != nil || b.Declaration.NotificationDesiredHash == "" {
			return fmt.Errorf("shared source expansion requires its retained producer and gateway policy scope")
		}
		if b.Shared != nil && (b.Shared.Owner.Binding.Producer != b.Producer.Producer || b.Shared.Owner.Binding.Generation != b.Producer.Generation || b.Shared.GatewayRoot != b.Declaration.NotificationRoot || b.Shared.Owner.Binding.Route != b.Declaration.NotificationRoute) {
			return fmt.Errorf("shared source expansion changed its delivery owner")
		}
	} else if b.Producer != nil || b.Shared != nil {
		return fmt.Errorf("standalone source expansion has shared ownership history")
	}
	if !safefs.DigestPattern.MatchString(b.RequestHash) || !safefs.ValidID(b.ID, "n") || !safefs.ValidID(b.WorkerID, "w") || !safefs.ValidID(b.ArchiveID, "a") || b.WorkerID != v.Installation.WorkerID || b.ArchiveID != v.Installation.ArchiveID {
		return fmt.Errorf("source expansion predecessor identity differs")
	}
	if err := validateSetupDesired(b.Declaration); err != nil {
		return err
	}
	if err := validateSetupDesired(r.Declaration); err != nil {
		return err
	}
	if len(b.SourceKeys) != len(b.Declaration.Sources) || len(b.SourceEnrollments) != len(b.SourceKeys) || len(r.Declaration.Sources) != len(r.Sources) {
		return fmt.Errorf("source expansion artifact or enrollment pins are incomplete")
	}
	for _, s := range r.Sources {
		if !safefs.DigestPattern.MatchString(r.Declaration.Sources[s.Alias]) {
			return fmt.Errorf("source expansion helper pin is unavailable")
		}
	}
	for _, s := range v.Installation.Sources {
		e := b.SourceEnrollments[s.Alias]
		if e.Schema != 1 || e.WorkerID != b.WorkerID || e.SourceID != s.SourceID || !safefs.ValidID(e.SourceID, "s") || e.Fingerprint == "" || e.Fingerprint != b.SourceKeys[s.Alias] {
			return fmt.Errorf("source expansion predecessor enrollment differs")
		}
	}
	return validateDeclarativeBackupUpdate(v.Installation, b.Declaration, r)
}

// Called before publishing the update journal or returning enrollment keys.
// Retrying after rollback keeps the original intent and all external receipts.
func prepareSetupSourceExpansion(root *safefs.Directory, j *WorkerSetupJournal, before WorkerInstallation, request WorkerSetupRequest) error {
	if j.SourceExpansion != nil {
		return checkSetupSourceExpansion(root, *j, &request)
	}
	if err := verifySetupSourceKeys(root, *j); err != nil {
		return err
	}
	added, removed := setupSourceChanges(before, request)
	if len(added) == 0 {
		return nil
	}
	paths, err := declarationPaths(root.File().Name(), j.Declaration)
	if err != nil {
		return err
	}
	// The already checked retirement intent belongs to this same predecessor.
	// Source inspection projects its declaration for key verification, so inspect
	// the original source view without re-entering the retirement coordinator.
	sourceView := *j
	sourceView.SourceRetirement = nil
	_, trust, err := inspectSetupSourcePins(root, sourceView, before, paths)
	if err != nil {
		return err
	}
	for _, s := range request.Sources {
		if j.SourceKeys[s.Alias] != "" {
			for _, old := range before.Sources {
				if old.Alias == s.Alias && trust[paths.OwnedPath(old.KnownHosts)] != sharedSetupDigest([]byte(s.KnownHosts)) {
					return fmt.Errorf("existing source trust must remain unchanged during enrollment")
				}
			}
		} else {
			for _, suffix := range []string{".key", ".key.pub", ".known_hosts"} {
				f, e := openBackupOwnedFile(root, paths.OwnedPath(filepath.Join(paths.Keys(), s.Alias+suffix)), os.O_RDONLY, 0)
				if e == nil {
					f.Close()
					return fmt.Errorf("new source has pre-existing unreserved key or trust files; explicit recovery required")
				}
				if !os.IsNotExist(e) {
					return e
				}
			}
		}
	}
	v := setupSourceExpansion{Version: 1, Before: *j, Installation: before, Request: request}
	if len(removed) > 0 {
		v.Version = 2
		v.Before.SourceRetirement = nil
		if j.SourceRetirement == nil || j.SourceRetirement.Version != 2 || !reflect.DeepEqual(j.SourceRetirement.Before, v.Before) || workerSetupRequestHash(j.SourceRetirement.Request) != workerSetupRequestHash(request) {
			return fmt.Errorf("combined enrollment lost retirement intent")
		}
	}
	if err = v.validate(); err != nil {
		return err
	}
	// Deep-copy maps: subsequent key reservations must not modify the predecessor.
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var retained setupSourceExpansion
	if err = json.Unmarshal(raw, &retained); err != nil {
		return err
	}
	j.SourceExpansion = &retained
	return nil
}

func checkSetupSourceExpansion(root *safefs.Directory, j WorkerSetupJournal, request *WorkerSetupRequest) error {
	if j.SourceExpansion == nil {
		return nil
	}
	v := *j.SourceExpansion
	if err := v.validate(); err != nil {
		return err
	}
	if v.Version == 2 && (j.SourceRetirement == nil || j.SourceRetirement.Version != 2 || !reflect.DeepEqual(j.SourceRetirement.Before, v.Before) || workerSetupRequestHash(j.SourceRetirement.Request) != workerSetupRequestHash(v.Request)) {
		return fmt.Errorf("combined enrollment changed retirement authority")
	}
	after := workerSetupRequestHash(v.Request)
	if request != nil && workerSetupRequestHash(*request) != after {
		return fmt.Errorf("source enrollment is pending; resume the exact declaration, artifacts and source trust originally applied")
	}
	if j.ID != v.Before.ID || j.WorkerID != v.Before.WorkerID || j.ArchiveID != v.Before.ArchiveID || !reflect.DeepEqual(j.Producer, v.Before.Producer) || !reflect.DeepEqual(j.Shared, v.Before.Shared) {
		return fmt.Errorf("source expansion installation identity changed")
	}
	switch j.RequestHash {
	case after:
		if !j.Updating || !reflect.DeepEqual(j.Declaration, v.Request.Declaration) || !reflect.DeepEqual(j.PreviousDeclaration, v.Before.Declaration) || j.PreviousRequestHash != v.Before.RequestHash {
			return fmt.Errorf("source expansion update lost its predecessor")
		}
	case v.Before.RequestHash:
		if j.Updating || !reflect.DeepEqual(j.Declaration, v.Before.Declaration) || j.Stage != "complete" {
			return fmt.Errorf("source expansion rollback is incomplete")
		}
	default:
		return fmt.Errorf("source expansion belongs to another setup revision")
	}
	if v.Version == 2 {
		return verifyCombinedSourceKeys(root, j, v.Before, v.Request)
	}
	for alias, pin := range v.Before.SourceKeys {
		if j.SourceKeys[alias] != pin || j.SourceEnrollments[alias] != v.Before.SourceEnrollments[alias] {
			return fmt.Errorf("source expansion changed an existing enrollment")
		}
	}
	view := j
	d := *v.Request.Declaration
	d.Sources = map[string]string{}
	view.Declaration = &d
	view.SourceEnrollments = maps.Clone(j.SourceEnrollments)
	for alias, pin := range j.SourceKeys {
		if v.Request.Declaration.Sources[alias] == "" || pin == "" {
			return fmt.Errorf("source expansion has an unreserved source key")
		}
		d.Sources[alias] = v.Request.Declaration.Sources[alias]
		if e, ok := view.SourceEnrollments[alias]; ok {
			if e.Schema != 1 || e.WorkerID != j.WorkerID || !safefs.ValidID(e.SourceID, "s") || e.Fingerprint != pin {
				return fmt.Errorf("source expansion enrollment identity differs")
			}
		} else {
			// A reserved key can precede the remote enrollment reply. Verify the
			// private key now without claiming that the source accepted it.
			view.SourceEnrollments[alias] = source.SourceEnrollmentResult{Fingerprint: pin}
		}
	}
	for alias := range j.SourceEnrollments {
		if j.SourceKeys[alias] == "" {
			return fmt.Errorf("source expansion enrollment has no reserved key")
		}
	}
	return verifySetupSourceKeys(root, view)
}

func recordSetupSourceEnrollment(stateRoot *safefs.Directory, j *WorkerSetupJournal, alias string, e source.SourceEnrollmentResult) error {
	if j.Declaration != nil {
		if j.Declaration.Sources[alias] == "" || j.SourceKeys[alias] == "" || j.SourceKeys[alias] != e.Fingerprint || e.Schema != 1 || e.WorkerID != j.WorkerID || !safefs.ValidID(e.SourceID, "s") {
			return fmt.Errorf("source enrollment differs from its reserved identity")
		}
		if old, ok := j.SourceEnrollments[alias]; ok && old != e {
			return fmt.Errorf("source enrollment changed; explicit source recovery required")
		}
	}
	if j.SourceEnrollments == nil {
		j.SourceEnrollments = map[string]source.SourceEnrollmentResult{}
	}
	j.SourceEnrollments[alias] = e
	return stateRoot.WriteJSON("setup.json", j, true)
}
