package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const setupAdoptionFile = ".local/state/deploy/backup-worker/declaration-adoption.json"

type SetupAdoptionReference struct {
	Operation string `json:"operation"`
	Intent    string `json:"intent"`
}
type setupAdoptionIntent struct {
	Version    int                `json:"version"`
	Operation  string             `json:"operation"`
	Before     WorkerSetupJournal `json:"before"`
	Request    WorkerSetupRequest `json:"request"`
	SourceKeys map[string]string  `json:"source_keys"`
	Trust      map[string]string  `json:"trust"`
}

func workerSetupRequestHash(request WorkerSetupRequest) string {
	request.Operation = ""
	if request.Declaration != nil {
		declaration := *request.Declaration
		// Retain compatibility with journals predating desired snapshots.
		declaration.Desired = nil
		request.Declaration = &declaration
	}
	raw, _ := json.Marshal(request)
	return sharedSetupDigest(raw)
}

func (v setupAdoptionIntent) validate() error {
	j, r := v.Before, v.Request
	if v.Version != 1 || !safefs.ValidID(v.Operation, "n") || j.Adoption != nil || j.SourceRetirement != nil || j.NotificationScope != nil || j.NotificationScopeHistory != nil || len(j.RetiredSources) != 0 || j.Declaration != nil || j.Schema != 1 || j.Stage != "complete" || !j.StateInitialized || j.Updating || j.RecoveryNeeded || j.Activity != nil || j.Shared != nil || j.Producer != nil || !safefs.ValidID(j.ID, "n") || !safefs.ValidID(j.WorkerID, "w") || !safefs.ValidID(j.ArchiveID, "a") || !safefs.DigestPattern.MatchString(j.RequestHash) {
		return fmt.Errorf("invalid standalone backup adoption predecessor")
	}
	if r.Schema != 1 || r.Operation != "adopt" || r.Recover || r.Fenced || r.Declaration == nil || r.Declaration.Version != 1 || r.Declaration.Paths != nil || r.Declaration.Desired == nil || r.Declaration.NotificationRoot != "" || r.Declaration.NotificationRoute != "" || r.Declaration.NotificationDesiredHash != "" || r.Policy.Notifications.Mode == "shared" || r.Policy.Notifications.Signal != nil {
		return fmt.Errorf("backup adoption requires unchanged legacy standalone policy and layout")
	}
	if err := validateSetupDesired(r.Declaration); err != nil {
		return err
	}
	if len(r.Declaration.Desired.Backup.RetireSources) != 0 {
		return fmt.Errorf("adopt unchanged policy before requesting source retirement")
	}
	if err := config.PinnedWorkerImage(r.Image); err != nil {
		return err
	}
	if !config.UserPattern.MatchString(r.Declaration.User) || (r.Declaration.Architecture != "amd64" && r.Declaration.Architecture != "arm64") {
		return fmt.Errorf("invalid backup adoption target")
	}
	if !safefs.DigestPattern.MatchString(r.Declaration.HelperSHA256) || r.Declaration.UID <= 0 || !config.WorkerAbsolutePath(r.Declaration.Home) || len(v.SourceKeys) != len(r.Sources) || len(v.Trust) != len(r.Sources) || len(r.Declaration.Sources) != len(r.Sources) {
		return fmt.Errorf("backup adoption inputs incomplete")
	}
	seen := map[string]bool{}
	for _, s := range r.Sources {
		if seen[s.Alias] || !config.ValidInventoryName(s.Alias) || v.SourceKeys[s.Alias] == "" || v.SourceKeys[s.Alias] != j.SourceEnrollments[s.Alias].Fingerprint || !safefs.DigestPattern.MatchString(r.Declaration.Sources[s.Alias]) {
			return fmt.Errorf("backup adoption source pins changed")
		}
		seen[s.Alias] = true
		path := filepath.Join(workerTrustDirectory, s.Alias+".known_hosts")
		if v.Trust[path] != sharedSetupDigest([]byte(s.KnownHosts)) {
			return fmt.Errorf("backup adoption trust pin changed")
		}
	}
	return nil
}

func (v setupAdoptionIntent) after() WorkerSetupJournal {
	j := v.Before
	j.Adoption = &SetupAdoptionReference{Operation: v.Operation, Intent: notify.Hash(v)}
	j.Declaration = v.Request.Declaration
	j.RequestHash = workerSetupRequestHash(v.Request)
	j.SourceKeys = maps.Clone(v.SourceKeys)
	j.OwnedFiles = maps.Clone(j.OwnedFiles)
	for path, hash := range v.Trust {
		j.OwnedFiles[path] = hash
	}
	return j
}

// A reference persists through subsequent ordinary updates. It proves the first
// takeover without making the historical desired snapshot a policy fallback.
func inspectSetupAdoption(root *safefs.Directory, j WorkerSetupJournal) (bool, error) {
	var v setupAdoptionIntent
	err := root.ReadJSON(setupAdoptionFile, &v)
	if errors.Is(err, os.ErrNotExist) && j.Adoption == nil {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("backup adoption history unavailable; recovery required")
	}
	if err = v.validate(); err != nil {
		return false, err
	}
	if j.Adoption == nil {
		if !reflect.DeepEqual(j, v.Before) {
			return false, fmt.Errorf("pending backup adoption predecessor changed")
		}
		return true, nil
	}
	if j.Adoption.Operation != v.Operation || j.Adoption.Intent != notify.Hash(v) || j.ID != v.Before.ID || j.WorkerID != v.Before.WorkerID || j.ArchiveID != v.Before.ArchiveID {
		return false, fmt.Errorf("backup adoption receipt changed")
	}
	return false, nil
}

// AdoptWorkerSetup records an explicit declaration takeover. It reads existing
// runtime/archive/source ownership and changes only the setup authority. There
// are no service calls, source enrollment, key generation or database writes.
func AdoptWorkerSetup(ctx context.Context, home string, r WorkerSetupRequest) (WorkerSetupResponse, error) {
	if r.Operation != "adopt" || !validSetupOperation(r) {
		return WorkerSetupResponse{}, fmt.Errorf("explicit declarative adoption request required")
	}
	if err := validateSetupDeclaration(r, home); err != nil {
		return WorkerSetupResponse{}, err
	}
	root, err := safefs.Open(home)
	if err != nil {
		return WorkerSetupResponse{}, err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if err != nil {
		return WorkerSetupResponse{}, err
	}
	defer lock.Close()
	return adoptWorkerSetupLocked(ctx, root, home, r, nil)
}

func adoptWorkerSetupLocked(ctx context.Context, root *safefs.Directory, home string, r WorkerSetupRequest, boundary func(string) error) (WorkerSetupResponse, error) {
	var result WorkerSetupResponse
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if r.Declaration == nil || r.Declaration.Desired == nil || r.Declaration.Desired.Backup == nil || r.Declaration.Home != home || r.Declaration.UID != os.Getuid() || !config.WorkerEnabled(r.Declaration.Desired.Backup.Enabled) {
		return result, fmt.Errorf("backup adoption requires the selected enabled declaration")
	}
	if err := validateSetupDesired(r.Declaration); err != nil {
		return result, err
	}
	var j WorkerSetupJournal
	if err := root.ReadJSON(backupSetupFile, &j); err != nil {
		return result, err
	}
	if _, err := inspectSetupAdoption(root, j); err != nil {
		return result, err
	}
	observed, err := inspectSetupForReviewLocked(ctx, root, home, true)
	if err != nil {
		return result, err
	}
	if observed.Stage != "complete" || observed.Paths == nil || observed.ActivationPending || observed.SharedReportingPending || observed.HelperSHA256 != r.Declaration.HelperSHA256 {
		return result, fmt.Errorf("adoption requires complete unchanged backup ownership and helper")
	}
	var lifecycle workerLifecycleJournal
	if e := root.ReadJSON(".local/state/deploy/backup-worker/lifecycle.json", &lifecycle); e == nil && lifecycle.Stage != "complete" {
		return result, fmt.Errorf("finish backup lifecycle before adoption")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return result, e
	}
	var c WorkerInstallation
	if err = readInstalledWorkerJSON(root, &c); err != nil {
		return result, err
	}
	keys, trust, err := inspectSetupSourcePins(root, j, c, *observed.Paths)
	if err != nil {
		return result, err
	}
	if err = matchSetupAdoptionPolicy(c, r, *observed.Paths, trust); err != nil {
		return result, err
	}
	if c.Policy.Storage.Mode == "home" {
		filesystem, e := homeArchiveFilesystem(c.Policy.Storage.Path)
		if e != nil || j.HomeFilesystem == "" || j.HomeFilesystem != filesystem {
			return result, fmt.Errorf("backup adoption cannot replace missing home archive filesystem authority")
		}
	}
	var intent setupAdoptionIntent
	err = root.ReadJSON(setupAdoptionFile, &intent)
	if errors.Is(err, os.ErrNotExist) {
		if r.Operation != "adopt" || j.Declaration != nil || j.Adoption != nil {
			return result, fmt.Errorf("explicit adopt is required for a legacy setup without declaration authority")
		}
		op, e := safefs.NewID("n")
		if e != nil {
			return result, e
		}
		intent = setupAdoptionIntent{Version: 1, Operation: op, Before: j, Request: r, SourceKeys: keys, Trust: trust}
		if err = intent.validate(); err != nil {
			return result, err
		}
		if err = root.WriteJSON(setupAdoptionFile, intent, false); err != nil {
			return result, err
		}
	} else if err != nil {
		return result, err
	}
	if err = intent.validate(); err != nil {
		return result, err
	}
	if workerSetupRequestHash(intent.Request) != workerSetupRequestHash(r) || !reflect.DeepEqual(keys, intent.SourceKeys) || !reflect.DeepEqual(trust, intent.Trust) {
		return result, fmt.Errorf("restore pinned backup adoption policy before resuming")
	}
	after := intent.after()
	if reflect.DeepEqual(j, after) {
		return WorkerSetupResponse{Schema: 1, Stage: "complete", WorkerID: j.WorkerID, ArchiveID: j.ArchiveID, Unchanged: true}, nil
	}
	if !reflect.DeepEqual(j, intent.Before) {
		return result, fmt.Errorf("backup adoption cannot replace changed setup authority")
	}
	if boundary != nil {
		if err = boundary("backup-adoption-intent"); err != nil {
			return result, err
		}
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = root.WriteJSON(backupSetupFile, after, true); err != nil {
		return result, err
	}
	if boundary != nil {
		if err = boundary("backup-adoption-published"); err != nil {
			return result, err
		}
	}
	return WorkerSetupResponse{Schema: 1, Stage: "complete", WorkerID: j.WorkerID, ArchiveID: j.ArchiveID, Unchanged: true}, nil
}

func matchSetupAdoptionPolicy(c WorkerInstallation, r WorkerSetupRequest, paths InstallationPaths, trust map[string]string) error {
	if c.Paths != nil || r.Declaration.Version != 1 || c.Image != r.Image || c.RelayImage != r.RelayImage || r.Policy.Notifications.Mode == "shared" || r.Policy.Notifications.Signal != nil {
		return fmt.Errorf("backup adoption cannot replace artifacts, shared/account ownership or paths")
	}
	selected, err := ResolveInstallationPaths(paths.Home, "", "", "", r.Declaration.Desired.Backup.Paths)
	if err != nil || !reflect.DeepEqual(selected, paths) {
		return fmt.Errorf("backup adoption cannot relocate retained files")
	}
	before, next := c.Policy, r.Policy
	if before.Storage.Mode == "" {
		before.Storage.Mode = "mount"
	}
	if next.Storage.Mode == "" {
		next.Storage.Mode = "mount"
	}
	if !reflect.DeepEqual(before, next) || len(c.Sources) != len(r.Sources) {
		return fmt.Errorf("import and save unchanged backup policy before adoption")
	}
	for _, source := range c.Sources {
		found := false
		for _, proposed := range r.Sources {
			if proposed.Alias != source.Alias {
				continue
			}
			if found || proposed.Host != source.Host || proposed.User != source.User || proposed.Port != source.Port || trust[paths.OwnedPath(source.KnownHosts)] != sharedSetupDigest([]byte(proposed.KnownHosts)) {
				return fmt.Errorf("adoption source endpoint or trust differs from installation")
			}
			found = true
		}
		if !found {
			return fmt.Errorf("adoption source is missing")
		}
	}
	return nil
}
