package backup

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type sharedPolicyUpdate struct {
	Helper    *sharedHelperReview `json:"helper,omitempty"`
	Version   int                 `json:"version"`
	Operation string              `json:"operation"`
	Before    WorkerSetupJournal  `json:"before"`
	Runtime   []byte              `json:"runtime"`
	After     WorkerSetupRequest  `json:"after"`
}
type sharedPolicyReference struct {
	Operation string `json:"operation"`
	Receipt   string `json:"receipt"`
}
type sharedPolicyReceipt struct {
	Helper      *helper.HelperArtifactManifest           `json:"helper,omitempty"`
	Retirement  *setupSourceRetirement                   `json:"retirement,omitempty"`
	Revocations map[string]source.SourceEnrollmentResult `json:"revocations,omitempty"`
	Expansion   *setupSourceExpansion                    `json:"expansion,omitempty"`
	Version     int                                      `json:"version"`
	Intent      sharedPolicyUpdate                       `json:"intent"`
	After       WorkerSetupJournal                       `json:"after"`
	Runtime     []byte                                   `json:"runtime"`
}

func (v sharedPolicyUpdate) validate() error {
	b := v.Before
	if (v.Version < 1 || v.Version > 8) || (!sharedHelperVersion(v.Version) && v.Helper != nil) || !safefs.ValidID(v.Operation, "n") || b.SharedPolicyUpdate != nil || b.SharedPolicyExpansion != nil || b.Stage != "complete" || b.Updating || b.RecoveryNeeded || b.NotificationScope != nil || b.Shared == nil || b.Declaration == nil || b.Declaration.Desired == nil || b.Declaration.Desired.Backup == nil || b.Producer == nil || workerSetupRequestHash(v.After) == b.RequestHash || v.After.Declaration == nil || v.After.Declaration.Desired == nil || v.After.Declaration.Desired.Backup == nil || b.SourceExpansion != nil || b.SourceRetirement != nil {
		return fmt.Errorf("shared backup policy history requires a completed predecessor")
	}
	if err := validateSetupDesired(b.Declaration); err != nil {
		return err
	}
	if err := validateSetupDesired(v.After.Declaration); err != nil {
		return err
	}
	paths, err := declarationPaths(b.Declaration.Home, b.Declaration)
	if err != nil {
		return err
	}
	if sharedSetupDigest(v.Runtime) != b.OwnedFiles[paths.OwnedPath(paths.Runtime)] {
		return fmt.Errorf("shared policy lost its original runtime")
	}
	var c WorkerInstallation
	if err = safefs.DecodeJSON(v.Runtime, &c, safefs.MaxRecordBytes); err != nil {
		return err
	}
	if c.WorkerID != b.WorkerID || c.ArchiveID != b.ArchiveID || (v.After.Operation != "apply" && v.After.Operation != "update") || v.After.Recover || v.After.Fenced || (v.Version != 6 && !sharedHelperVersion(v.Version) && v.After.Image != c.Image) || v.After.RelayImage != c.RelayImage || (!sharedHelperVersion(v.Version) && v.After.Declaration.HelperSHA256 != b.Declaration.HelperSHA256) {
		return fmt.Errorf("apply shared backup artifact changes separately")
	}
	if err = validateDeclarativeBackupUpdate(c, b.Declaration, v.After); err != nil {
		return err
	}
	if sharedHelperVersion(v.Version) {
		return validateSharedHelperUpdate(v, c)
	}
	added, removed := setupSourceChanges(c, v.After)
	if v.Version == 6 {
		declared := *v.After.Declaration.Desired.Backup
		declared.Artifacts = b.Declaration.Desired.Backup.Artifacts
		if !reflect.DeepEqual(declared, *b.Declaration.Desired.Backup) {
			return fmt.Errorf("apply backup image changes separately from other YAML policy")
		}
		if v.After.Image == c.Image || len(added) != 0 || len(removed) != 0 || sourceEndpointsDiffer(c, v.After) || !reflect.DeepEqual(c.Policy, v.After.Policy) || !reflect.DeepEqual(b.Declaration.Sources, v.After.Declaration.Sources) {
			return fmt.Errorf("apply backup image changes separately from source, helper and policy changes")
		}
		return config.PinnedWorkerImage(v.After.Image)
	}
	if v.Version == 5 {
		if len(added) == 0 || len(removed) == 0 || sourceEndpointsDiffer(c, v.After) {
			return fmt.Errorf("shared source replacement requires both additions and explicit retirements")
		}
		return nil
	}
	if v.Version == 4 {
		if len(removed) == 0 || len(added) != 0 || sourceEndpointsDiffer(c, v.After) {
			return fmt.Errorf("source retirement history requires removals with retained endpoints")
		}
		return nil // The ordinary retirement validator pins dispositions and all remaining policy.
	}
	if v.Version == 3 {
		if len(added) == 0 || len(removed) != 0 || sourceEndpointsDiffer(c, v.After) {
			return fmt.Errorf("source expansion history requires additions with retained endpoints")
		}
		return nil // validateDeclarativeBackupUpdate already pins existing helpers and policy.
	}
	if len(added) != 0 || len(removed) != 0 || !reflect.DeepEqual(b.Declaration.Sources, v.After.Declaration.Sources) {
		return fmt.Errorf("source set changes require their typed continuation")
	}
	moved := sourceEndpointsDiffer(c, v.After)
	if moved != (v.Version == 2) {
		return fmt.Errorf("source endpoint changes require their typed continuation")
	}
	if moved {
		return validateSourceEndpointMove(c, b.Declaration, v.After)
	}
	return nil
}

func prepareSharedPolicyHistory(root *safefs.Directory, j *WorkerSetupJournal, request WorkerSetupRequest) error {
	if j.Shared == nil {
		return nil
	}
	if j.SharedPolicyUpdate != nil {
		v := j.SharedPolicyUpdate
		if !sharedHelperVersion(v.Version) || j.Updating || j.RequestHash != v.Before.RequestHash || workerSetupRequestHash(request) != workerSetupRequestHash(v.After) {
			return fmt.Errorf("resume the exact pending shared helper upgrade")
		}
		if err := checkSharedPolicyUpdate(*j); err != nil {
			return err
		}
		review, err := reviewSharedHelper(root, *j, request.Declaration.HelperSHA256)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(review, v.Helper) {
			return fmt.Errorf("rolled-back helper differs from retained upgrade")
		}
		return nil
	}
	if j.NotificationScopeHistory == nil && j.SharedPolicyHistory == nil {
		before, err := loadInstalledWorker(root.File().Name())
		if err != nil {
			return err
		}
		if request.Image == before.Image && request.Declaration.HelperSHA256 == j.Declaration.HelperSHA256 {
			return nil
		}
	}
	if j.SourceExpansion != nil {
		// Remote authorization survives local rollback. Restore the same
		// history intent retained by the expansion, not a new predecessor
		// containing partially enrolled keys.
		if err := checkSetupSourceExpansion(root, *j, &request); err != nil {
			return err
		}
		v := j.SourceExpansion.Before.SharedPolicyUpdate
		if v == nil || (v.Version != 3 && v.Version != 5) || v.validate() != nil || workerSetupRequestHash(v.After) != workerSetupRequestHash(request) || !reflect.DeepEqual(v.Before.SharedPolicyHistory, j.SharedPolicyHistory) || !reflect.DeepEqual(v.Before.NotificationScopeHistory, j.NotificationScopeHistory) {
			return fmt.Errorf("pending source expansion lost its shared history predecessor")
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &j.SharedPolicyUpdate)
	}
	if j.SourceRetirement != nil {
		if err := checkSetupSourceRetirement(root, *j, &request); err != nil {
			return err
		}
		v := j.SourceRetirement.Before.SharedPolicyUpdate
		if v == nil || v.Version != 4 || v.validate() != nil || workerSetupRequestHash(v.After) != workerSetupRequestHash(request) || !reflect.DeepEqual(v.Before.SharedPolicyHistory, j.SharedPolicyHistory) || !reflect.DeepEqual(v.Before.NotificationScopeHistory, j.NotificationScopeHistory) {
			return fmt.Errorf("pending source retirement lost its shared history predecessor")
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &j.SharedPolicyUpdate)
	}
	paths, err := declarationPaths(j.Declaration.Home, j.Declaration)
	if err != nil {
		return err
	}
	raw, err := readBackupOwnedFile(root, paths.OwnedPath(paths.Runtime), safefs.MaxRecordBytes)
	if err != nil {
		return err
	}
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	var before WorkerInstallation
	if err = safefs.DecodeJSON(raw, &before, safefs.MaxRecordBytes); err != nil {
		return err
	}
	version := 1
	if sourceEndpointsDiffer(before, request) {
		version = 2
	}
	if added, _ := setupSourceChanges(before, request); len(added) > 0 {
		version = 3
	}
	if added, removed := setupSourceChanges(before, request); len(removed) > 0 {
		version = 4
		if len(added) > 0 {
			version = 5
		}
	}
	if request.Image != before.Image {
		version = 6
	}
	if request.Declaration.HelperSHA256 != j.Declaration.HelperSHA256 {
		version = 7
	}
	v := sharedPolicyUpdate{Version: version, Operation: id, Before: *j, Runtime: raw, After: request}
	if version == 7 {
		v.Helper, err = reviewSharedHelper(root, *j, request.Declaration.HelperSHA256)
		if err != nil {
			return err
		}
		if v.Helper.Commands != nil {
			v.Version = 8
		}
	}
	// Copy maps and declarations before the existing update transaction mutates
	// its working journal. The intent remains an immutable predecessor.
	encoded, err := json.Marshal(v)
	if err != nil {
		return err
	}
	v = sharedPolicyUpdate{}
	if err = json.Unmarshal(encoded, &v); err != nil {
		return err
	}
	if err = v.validate(); err != nil {
		return err
	}
	j.SharedPolicyUpdate = &v
	return nil
}

func checkSharedPolicyUpdate(j WorkerSetupJournal) error {
	v := j.SharedPolicyUpdate
	if v == nil {
		if j.SharedPolicyExpansion != nil {
			return fmt.Errorf("shared replacement lost its pending history intent")
		}
		return nil
	}
	if err := v.validate(); err != nil {
		return err
	}
	expectedHash, expectedDeclaration := workerSetupRequestHash(v.After), v.After.Declaration
	pending := j.Updating && j.PreviousRequestHash == v.Before.RequestHash && reflect.DeepEqual(j.PreviousDeclaration, v.Before.Declaration)
	if (v.Version == 4 || v.Version == 5) && !j.Updating && j.Stage == "complete" && j.SourceRetirement != nil && j.PreviousRequestHash == "" && j.PreviousDeclaration == nil {
		pending = true // Local publication completed; remote revocations are still pending.
		if v.Version == 5 {
			expansion := j.SharedPolicyExpansion
			if expansion == nil || expansion.Version != 2 || expansion.validate() != nil || workerSetupRequestHash(expansion.Request) != workerSetupRequestHash(v.After) || notify.Hash(expansion.Before.SharedPolicyUpdate) != notify.Hash(v) {
				return fmt.Errorf("shared replacement lost its published enrollment intent")
			}
		}
	}
	if sharedHelperVersion(v.Version) && !j.Updating && !j.RecoveryNeeded && j.Stage == "complete" && j.RequestHash == v.Before.RequestHash && j.PreviousRequestHash == "" && j.PreviousDeclaration == nil {
		pending = true
		expectedHash, expectedDeclaration = v.Before.RequestHash, v.Before.Declaration
		if !reflect.DeepEqual(j.SourceKeys, v.Before.SourceKeys) || !reflect.DeepEqual(j.SourceEnrollments, v.Before.SourceEnrollments) {
			return fmt.Errorf("rolled-back helper upgrade changed sources")
		}
	}
	if !pending || j.ID != v.Before.ID || j.WorkerID != v.Before.WorkerID || j.ArchiveID != v.Before.ArchiveID || j.RequestHash != expectedHash || !reflect.DeepEqual(j.Declaration, expectedDeclaration) || !reflect.DeepEqual(j.Shared, v.Before.Shared) || !reflect.DeepEqual(j.Producer, v.Before.Producer) || !reflect.DeepEqual(j.SharedPolicyHistory, v.Before.SharedPolicyHistory) || !reflect.DeepEqual(j.NotificationScopeHistory, v.Before.NotificationScopeHistory) {
		return fmt.Errorf("pending shared backup update differs from its retained intent")
	}
	return nil
}

func (r sharedPolicyReceipt) validate() error {
	v, a := r.Intent, r.After
	validVersion := (v.Version == 1 || v.Version == 2) && r.Version == 1 && r.Expansion == nil && r.Retirement == nil && len(r.Revocations) == 0 || v.Version == 3 && r.Version == 2 && r.Retirement == nil && len(r.Revocations) == 0 || v.Version == 4 && r.Version == 3 && r.Expansion == nil || v.Version == 5 && r.Version == 4 || (v.Version == 6 && r.Version == 5 || v.Version == 7 && r.Version == 6 || v.Version == 8 && r.Version == 7) && r.Expansion == nil && r.Retirement == nil && len(r.Revocations) == 0
	if !validVersion || (!sharedHelperVersion(v.Version) && r.Helper != nil) || v.validate() != nil || a.SharedPolicyUpdate != nil || a.SharedPolicyExpansion != nil || a.Stage != "complete" || a.Updating || a.RecoveryNeeded || a.ID != v.Before.ID || a.WorkerID != v.Before.WorkerID || a.ArchiveID != v.Before.ArchiveID || a.RequestHash != workerSetupRequestHash(v.After) || !reflect.DeepEqual(a.Declaration, v.After.Declaration) || !reflect.DeepEqual(a.SharedPolicyHistory, v.Before.SharedPolicyHistory) || !reflect.DeepEqual(a.NotificationScopeHistory, v.Before.NotificationScopeHistory) || !reflect.DeepEqual(a.Producer, v.Before.Producer) || !reflect.DeepEqual(a.Shared, v.Before.Shared) {
		return fmt.Errorf("shared backup completion lost retained policy authority")
	}
	if v.Version == 3 || v.Version == 5 {
		if err := validateSharedExpansionReceipt(r); err != nil {
			return err
		}
	}
	if v.Version == 4 || v.Version == 5 {
		if err := validateSharedRetirementReceipt(r); err != nil {
			return err
		}
	} else if (v.Version < 3 || v.Version >= 6) && (!reflect.DeepEqual(a.SourceEnrollments, v.Before.SourceEnrollments) || !reflect.DeepEqual(a.SourceKeys, v.Before.SourceKeys)) {
		return fmt.Errorf("shared policy changed source enrollment")
	}
	if sharedHelperVersion(v.Version) {
		if err := validateSharedHelperCompletion(v, r.Helper); err != nil {
			return err
		}
	}
	paths, err := declarationPaths(a.Declaration.Home, a.Declaration)
	if err != nil {
		return err
	}
	if sharedSetupDigest(r.Runtime) != a.OwnedFiles[paths.OwnedPath(paths.Runtime)] {
		return fmt.Errorf("shared backup completion lost runtime publication")
	}
	var c WorkerInstallation
	if err = safefs.DecodeJSON(r.Runtime, &c, safefs.MaxRecordBytes); err != nil {
		return err
	}
	if c.WorkerID != a.WorkerID || c.ArchiveID != a.ArchiveID || !reflect.DeepEqual(c.Policy, v.After.Policy) || c.Image != v.After.Image || c.RelayImage != v.After.RelayImage {
		return fmt.Errorf("shared backup runtime differs from reviewed policy")
	}
	var before WorkerInstallation
	if err = safefs.DecodeJSON(v.Runtime, &before, safefs.MaxRecordBytes); err != nil {
		return err
	}
	expectedSources := slices.Clone(before.Sources)
	if v.Version == 2 {
		// Endpoint moves retain every identity, key and trust-file path. Only
		// the reviewed address changes; setup already inspected the remote
		// source and pinned its replacement known_hosts bytes before commit.
		for i := range expectedSources {
			for _, next := range v.After.Sources {
				if next.Alias == expectedSources[i].Alias {
					expectedSources[i].Host, expectedSources[i].User, expectedSources[i].Port = next.Host, next.User, next.Port
				}
			}
		}
	}
	if v.Version >= 3 && v.Version <= 5 {
		expectedSources = nil
		for _, next := range v.After.Sources {
			expectedSources = append(expectedSources, source.WorkerSource{Alias: next.Alias, Host: next.Host, User: next.User, Port: next.Port, KeyFile: filepath.Join(paths.Keys(), next.Alias+".key"), KnownHosts: filepath.Join(paths.Keys(), next.Alias+".known_hosts"), SourceID: a.SourceEnrollments[next.Alias].SourceID})
		}
	}
	if !reflect.DeepEqual(c.Sources, expectedSources) || !reflect.DeepEqual(c.Paths, before.Paths) {
		return fmt.Errorf("shared backup completion changed sources or paths")
	}
	return validateWorkerInstallation(c)
}

func commitSharedPolicyHistory(stateRoot *safefs.Directory, before WorkerSetupJournal, after *WorkerSetupJournal, revocations map[string]source.SourceEnrollmentResult) error {
	if before.SharedPolicyUpdate == nil {
		return nil
	}
	if err := checkSharedPolicyUpdate(before); err != nil {
		return err
	}
	v := *before.SharedPolicyUpdate
	if (v.Version == 4 || v.Version == 5) && revocations == nil {
		// Keep the intent pending across local publication. A completed history
		// receipt is written only after every remote revocation is acknowledged.
		if v.Version == 5 {
			root, err := safefs.Open(after.Declaration.Home)
			if err != nil {
				return err
			}
			defer root.Close()
			if before.SourceExpansion == nil {
				return fmt.Errorf("shared replacement lost pending enrollment")
			}
			if err = checkSetupSourceExpansion(root, before, &v.After); err != nil {
				return err
			}
			after.SharedPolicyExpansion = before.SourceExpansion
		}
		return nil
	}
	after.SharedPolicyUpdate = nil
	after.SharedPolicyExpansion = nil
	root, err := safefs.Open(after.Declaration.Home)
	if err != nil {
		return err
	}
	defer root.Close()
	paths, err := declarationPaths(after.Declaration.Home, after.Declaration)
	if err != nil {
		return err
	}
	raw, err := readBackupOwnedFile(root, paths.OwnedPath(paths.Runtime), safefs.MaxRecordBytes)
	if err != nil {
		return err
	}
	r := sharedPolicyReceipt{Version: 1, Intent: v, After: *after, Runtime: raw}
	if v.Version == 6 {
		r.Version = 5
	}
	if sharedHelperVersion(v.Version) {
		r.Version = 6
		if v.Version == 8 {
			r.Version = 7
		}
		r.Helper, err = captureSharedHelperCompletion(root, v)
		if err != nil {
			return err
		}
	}
	if v.Version == 3 {
		if err = checkSetupSourceExpansion(root, before, &v.After); err != nil {
			return err
		}
		r.Version, r.Expansion = 2, before.SourceExpansion
	}
	if v.Version == 4 || v.Version == 5 {
		if err = checkSetupSourceRetirement(root, before, &v.After); err != nil {
			return err
		}
		r.Version, r.Retirement, r.Revocations = 3, before.SourceRetirement, revocations
		if v.Version == 5 {
			r.Version, r.Expansion = 4, before.SharedPolicyExpansion
		}
	}
	if err = r.validate(); err != nil {
		return err
	}
	name := "shared-policy-" + v.Operation + ".json"
	var retained sharedPolicyReceipt
	err = stateRoot.ReadJSON(name, &retained)
	if os.IsNotExist(err) {
		err = stateRoot.WriteJSON(name, r, false)
	} else if err == nil && notify.Hash(retained) != notify.Hash(r) {
		err = fmt.Errorf("shared backup completion changed on retry")
	}
	if err != nil {
		return err
	}
	after.SharedPolicyHistory = &sharedPolicyReference{v.Operation, notify.Hash(r)}
	return nil
}

func readSharedPolicyHistory(root *safefs.Directory, ref *sharedPolicyReference) (r sharedPolicyReceipt, err error) {
	if ref == nil || !safefs.ValidID(ref.Operation, "n") || !safefs.DigestPattern.MatchString(ref.Receipt) {
		return r, fmt.Errorf("invalid shared policy history reference")
	}
	if err = root.ReadJSON(".local/state/deploy/backup-worker/shared-policy-"+ref.Operation+".json", &r); err != nil {
		return r, err
	}
	if r.Intent.Operation != ref.Operation || notify.Hash(r) != ref.Receipt {
		return r, fmt.Errorf("shared policy history changed")
	}
	return r, r.validate()
}

func sharedPolicyPredecessor(root *safefs.Directory, j WorkerSetupJournal) (r sharedPolicyReceipt, handled bool, err error) {
	if j.SharedPolicyHistory == nil {
		return r, false, nil
	}
	r, err = readSharedPolicyHistory(root, j.SharedPolicyHistory)
	if err != nil {
		return r, true, err
	}
	// A notification toggle may return to the same policy fingerprint. Its
	// newer scope receipt must be traversed before this backup receipt.
	if j.RequestHash != r.After.RequestHash || !reflect.DeepEqual(j.NotificationScopeHistory, r.After.NotificationScopeHistory) {
		return r, false, nil
	}
	copy := j
	copy.SharedPolicyHistory = r.After.SharedPolicyHistory
	if !reflect.DeepEqual(copy, r.After) {
		if err = verifyCommandScopeCapture(root, copy, r.After, r.After.Shared.Profile.ManifestHash); err != nil {
			return r, true, err
		}
	}
	return r, true, nil
}

// A completed addition proves the exact external enrollment intent, every old
// key, and all new acknowledgements. The expansion is retained in history even
// after the active setup journal clears its pending operation.
func validateSharedExpansionReceipt(r sharedPolicyReceipt) error {
	v, a, expansion := r.Intent, r.After, r.Expansion
	expectedVersion := 1
	if v.Version == 5 {
		expectedVersion = 2
	}
	if expansion == nil || expansion.Version != expectedVersion || expansion.validate() != nil || workerSetupRequestHash(expansion.Request) != workerSetupRequestHash(v.After) || a.SourceExpansion != nil || a.SourceRetirement != nil {
		return fmt.Errorf("shared source addition lost its enrollment intent")
	}
	before := expansion.Before
	if before.SharedPolicyUpdate == nil || notify.Hash(before.SharedPolicyUpdate) != notify.Hash(v) {
		return fmt.Errorf("source enrollment changed its shared history intent")
	}
	before.SharedPolicyUpdate = nil
	if !reflect.DeepEqual(before, v.Before) || (v.Version == 3 && !reflect.DeepEqual(a.RetiredSources, before.RetiredSources)) || len(a.SourceKeys) != len(v.After.Sources) || len(a.SourceEnrollments) != len(a.SourceKeys) {
		return fmt.Errorf("source enrollment lost its predecessor or complete key set")
	}
	for alias, fingerprint := range before.SourceKeys {
		if v.Version == 5 && v.After.Declaration.Sources[alias] == "" {
			continue
		}
		if a.SourceKeys[alias] != fingerprint || a.SourceEnrollments[alias] != before.SourceEnrollments[alias] {
			return fmt.Errorf("source addition changed an existing key or identity")
		}
	}
	for _, target := range v.After.Sources {
		e, ok := a.SourceEnrollments[target.Alias]
		if !ok || e.Schema != 1 || e.WorkerID != a.WorkerID || !safefs.ValidID(e.SourceID, "s") || e.Fingerprint == "" || e.Fingerprint != a.SourceKeys[target.Alias] {
			return fmt.Errorf("source addition lacks its retained enrollment acknowledgement")
		}
	}
	return nil
}

func validateSharedRetirementReceipt(r sharedPolicyReceipt) error {
	v, a, retirement := r.Intent, r.After, r.Retirement
	expectedVersion := 1
	if v.Version == 5 {
		expectedVersion = 2
	}
	if retirement == nil || retirement.Version != expectedVersion || retirement.validate() != nil || workerSetupRequestHash(retirement.Request) != workerSetupRequestHash(v.After) || a.SourceExpansion != nil || a.SourceRetirement != nil || len(r.Revocations) != len(retirement.Sources) {
		return fmt.Errorf("shared source retirement lost its disposition or acknowledgements")
	}
	before := retirement.Before
	if before.SharedPolicyUpdate == nil || notify.Hash(before.SharedPolicyUpdate) != notify.Hash(v) {
		return fmt.Errorf("source retirement changed its shared history intent")
	}
	before.SharedPolicyUpdate = nil
	if !reflect.DeepEqual(before, v.Before) {
		return fmt.Errorf("source retirement lost its predecessor")
	}
	keys, enrollments, retired := maps.Clone(before.SourceKeys), maps.Clone(before.SourceEnrollments), maps.Clone(before.RetiredSources)
	if retired == nil {
		retired = map[string]SetupSourceRetirement{}
	}
	for alias, disposition := range retirement.Sources {
		if r.Revocations[alias] != (source.SourceEnrollmentResult{Schema: 1, SourceID: disposition.SourceID, WorkerID: a.WorkerID}) {
			return fmt.Errorf("source retirement acknowledgement changed")
		}
		delete(keys, alias)
		delete(enrollments, alias)
		retired[alias] = disposition
	}
	if v.Version == 5 {
		for _, target := range v.After.Sources {
			if before.SourceKeys[target.Alias] == "" {
				keys[target.Alias], enrollments[target.Alias] = a.SourceKeys[target.Alias], a.SourceEnrollments[target.Alias]
			}
		}
	}
	if !reflect.DeepEqual(a.SourceKeys, keys) || !reflect.DeepEqual(a.SourceEnrollments, enrollments) || !reflect.DeepEqual(a.RetiredSources, retired) {
		return fmt.Errorf("source retirement lost retained keys or dispositions")
	}
	return validateSourceRetirementHistory(a)
}
