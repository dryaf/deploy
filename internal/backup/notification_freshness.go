package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// Both runtime files are retained byte-for-byte. The existing installation gate
// fences even older helpers while the gateway, runtime and archive checkpoint
// converge. A completed scope receipt, rather than a new setup identity, owns
// the change. No source, timer, observation or archive data is replaced.
type setupFreshnessRuntime struct {
	Before       map[string][]byte    `json:"before"`
	After        map[string][]byte    `json:"after"`
	BeforeConfig string               `json:"before_config"`
	AfterConfig  string               `json:"after_config"`
	Capture      []backupActivityUnit `json:"capture,omitempty"`
}

func ReconcileVerificationDerivedFreshnessScope(ctx context.Context, home string, request VerificationNotificationScopeChange, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileNotificationScope(ctx, home, request, migrate, 14)
}

// Reserve only routes an exact derived-deadline request to the notification
// coordinator. It cannot publish either runtime or authorize setup before the
// shared operation completes.
func inspectCoordinatedNotificationReservation(root, stateRoot *safefs.Directory, j WorkerSetupJournal, request WorkerSetupRequest) (bool, error) {
	if j.Declaration == nil || j.Declaration.Desired == nil || request.Declaration == nil || request.Declaration.Desired == nil {
		return false, nil
	}
	if j.NotificationScope != nil {
		v := *j.NotificationScope
		if !freshnessScope(v.Version) && v.Version != 15 {
			return false, nil
		}
		if err := v.validate(); err != nil {
			return true, err
		}
		if workerSetupRequestHash(request) != workerSetupRequestHash(v.After) || !reflect.DeepEqual(request.Declaration.Desired, v.After.Declaration.Desired) {
			return true, fmt.Errorf("resume derived freshness with its exact retained YAML")
		}
		return true, checkSetupProducerReservation(stateRoot, &j)
	}
	derived := config.ValidateDerivedFreshnessDeclaration(*j.Declaration.Desired, *request.Declaration.Desired) == nil
	reportRoute := config.ValidateSharedReportRouteDeclaration(*j.Declaration.Desired, *request.Declaration.Desired) == nil
	if !derived && !reportRoute {
		return false, nil
	}
	if j.Stage != "complete" || (j.Schema != 1 && j.Schema != 2) || !derived && j.Shared != nil || j.Updating || j.RecoveryNeeded || j.SourceExpansion != nil || j.SourceRetirement != nil || j.Activity != nil && (!j.Activity.Complete || j.Activity.Disabled) {
		return true, fmt.Errorf("derived freshness requires completed staged backup setup")
	}
	if err := checkSetupProducerReservation(stateRoot, &j); err != nil {
		return true, err
	}
	if err := validateSetupDesired(request.Declaration); err != nil {
		return true, err
	}
	prior := request
	d := *request.Declaration
	d.Desired, d.DesiredHash, d.NotificationDesiredHash = j.Declaration.Desired, j.Declaration.DesiredHash, j.Declaration.NotificationDesiredHash
	prior.Declaration = &d
	if derived {
		prior.Policy.Freshness = j.Declaration.Desired.Backup.Freshness
	}
	if reportRoute {
		if request.Declaration.NotificationRoute != request.Declaration.Desired.Backup.Notifications.Route {
			return true, fmt.Errorf("report route differs from requested backup declaration")
		}
		d.NotificationRoute = j.Declaration.NotificationRoute
	}
	if workerSetupRequestHash(prior) != j.RequestHash || !reflect.DeepEqual(d, *j.Declaration) {
		return true, fmt.Errorf("derived freshness changed backup artifacts, paths or unrelated policy")
	}
	return true, nil
}

func freshnessInstallation(before WorkerInstallation, freshness string) (WorkerInstallation, error) {
	after := before
	after.Policy.Freshness = freshness
	policy, err := config.NormalizeWorker(after.Policy)
	if err != nil {
		return after, err
	}
	if !reflect.DeepEqual(policy, after.Policy) {
		return after, fmt.Errorf("freshness update changed normalized backup policy")
	}
	// Setup hashes the canonical installation before its software checks are
	// recorded. Preserve those checks, but require new live qualification for
	// this new configuration hash.
	after.ConfigHash = ""
	after.Qualification = map[string]bool{}
	after.ConfigHash = notify.Hash(after)
	after.Qualification = before.Qualification
	return after, validateWorkerInstallation(after)
}

func (v *setupNotificationScope) prepareFreshness(root *safefs.Directory, c WorkerInstallation, paths InstallationPaths) error {
	after, err := freshnessInstallation(c, v.After.Policy.Freshness)
	if err != nil {
		return err
	}
	f := &setupFreshnessRuntime{Before: map[string][]byte{}, After: map[string][]byte{}, BeforeConfig: c.ConfigHash, AfterConfig: after.ConfigHash}
	for _, file := range []string{paths.Runtime, paths.ContainerRuntime()} {
		name := paths.OwnedPath(file)
		raw, err := readBackupOwnedFile(root, name, safefs.MaxRecordBytes)
		if err != nil {
			return err
		}
		f.Before[name] = raw
		var runtime WorkerInstallation
		if err = safefs.DecodeJSON(raw, &runtime, safefs.MaxRecordBytes); err != nil {
			return err
		}
		runtime.Policy.Freshness, runtime.ConfigHash = after.Policy.Freshness, after.ConfigHash
		f.After[name], err = json.Marshal(runtime)
		if err != nil {
			return err
		}
	}
	v.Freshness = f
	return nil
}

func (v setupNotificationScope) validateFreshness() error {
	f := v.Freshness
	if f == nil || len(f.Before) != 2 || len(f.After) != 2 {
		return fmt.Errorf("freshness migration requires both retained runtime files")
	}
	p, err := declarationPaths(v.Before.Declaration.Home, v.Before.Declaration)
	if err != nil {
		return err
	}
	var before WorkerInstallation
	if err = safefs.DecodeJSON(f.Before[p.OwnedPath(p.Runtime)], &before, safefs.MaxRecordBytes); err != nil {
		return err
	}
	if before.WorkerID != v.Before.WorkerID || before.ArchiveID != v.Before.ArchiveID || before.ConfigHash != f.BeforeConfig || before.Policy.Freshness != v.Before.Declaration.Desired.Backup.Freshness || !reflect.DeepEqual(before.Paths, v.Before.Declaration.Paths) {
		return fmt.Errorf("freshness predecessor lost backup identity")
	}
	after, err := freshnessInstallation(before, v.After.Policy.Freshness)
	if err != nil {
		return err
	}
	if after.ConfigHash != f.AfterConfig || after.ConfigHash == before.ConfigHash || !reflect.DeepEqual(after.Policy, v.After.Policy) {
		return fmt.Errorf("freshness candidate differs from retained backup policy")
	}
	for _, path := range []string{p.Runtime, p.ContainerRuntime()} {
		name := p.OwnedPath(path)
		raw := f.Before[name]
		if sharedSetupDigest(raw) != v.Before.OwnedFiles[name] {
			return fmt.Errorf("freshness predecessor differs from owned files")
		}
		var runtime WorkerInstallation
		if err = safefs.DecodeJSON(raw, &runtime, safefs.MaxRecordBytes); err != nil {
			return err
		}
		if runtime.ConfigHash != before.ConfigHash || runtime.WorkerID != before.WorkerID || runtime.ArchiveID != before.ArchiveID || runtime.Policy.Freshness != before.Policy.Freshness {
			return fmt.Errorf("freshness runtime identity differs")
		}
		runtime.Policy.Freshness, runtime.ConfigHash = after.Policy.Freshness, after.ConfigHash
		want, err := json.Marshal(runtime)
		if err != nil || string(want) != string(f.After[name]) {
			return fmt.Errorf("freshness candidate contains unrelated runtime edits")
		}
	}
	return v.validateFreshnessCapture()
}

func (v setupNotificationScope) freshnessFileMatches(name string, raw []byte) bool {
	if v.Freshness == nil {
		return false
	}
	b, ok := v.Freshness.Before[name]
	return ok && (string(raw) == string(b) || string(raw) == string(v.Freshness.After[name]))
}

func (v setupNotificationScope) freshnessOwnedFiles() map[string]string {
	files := maps.Clone(v.Before.OwnedFiles)
	for name, raw := range v.Freshness.After {
		files[name] = sharedSetupDigest(raw)
	}
	return files
}

func (v setupNotificationScope) lockFreshness(root *safefs.Directory) (func(), error) {
	p, err := declarationPaths(root.File().Name(), v.Before.Declaration)
	if err != nil {
		return nil, err
	}
	d, err := safefs.Open(p.State)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	// Do not wait behind a running backup. No publication has occurred and
	// ordinary resume can retry after that reader finishes.
	lease, err := d.Lock("schema.lock", true, false)
	if err != nil {
		return nil, fmt.Errorf("backup runtime is busy; resume freshness after it finishes: %w", err)
	}
	var gate installationGate
	err = d.ReadJSON("installation.pending", &gate)
	if os.IsNotExist(err) {
		err = d.WriteJSON("installation.pending", installationGate{v.Operation}, false)
	} else if err == nil && gate.Operation != v.Operation {
		err = fmt.Errorf("backup installation belongs to another pending operation")
	}
	if err != nil {
		lease.Close()
		return nil, err
	}
	return func() { lease.Close() }, nil
}

func (v setupNotificationScope) publishFreshness(root *safefs.Directory, boundary func(string) error) error {
	p, err := declarationPaths(root.File().Name(), v.Before.Declaration)
	if err != nil {
		return err
	}
	state, err := openBackupStateWithLease(p.State, false, false, true)
	if err != nil {
		return err
	}
	defer state.Close()
	var activity string
	if err = state.get("installation", &activity); err != nil || (activity != "staged-awaiting-qualification" && !(v.Version == 23 && activity == "active")) {
		return fmt.Errorf("freshness publication requires staged backup")
	}
	if v.Version == 23 {
		err = requireCommandScopeOwner(state, v.Before)
	} else {
		err = requirePendingSharedSetup(state, &v.Before)
	}
	if err != nil {
		return err
	}
	if v.Version == 23 && activity == "active" {
		if err = state.put("installation", "staged-awaiting-qualification"); err != nil {
			return err
		}
		if err = boundary("capture-staged"); err != nil {
			return err
		}
	}
	a, err := openBackupArchive(v.After.Policy.Storage.Path)
	if err != nil {
		return err
	}
	defer a.Close()
	lease, err := a.dir.Lock(".locks/writer.lock", true, false)
	if err != nil {
		return err
	}
	defer lease.Close()
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	if control.ConfigHash != v.Freshness.BeforeConfig && control.ConfigHash != v.Freshness.AfterConfig {
		return fmt.Errorf("archive freshness checkpoint changed outside retained operation")
	}
	if control.ConfigHash == v.Freshness.BeforeConfig {
		if err = a.updateControl(state, func(c *ArchiveControl) error { c.ConfigHash = v.Freshness.AfterConfig; return nil }); err != nil {
			return err
		}
	} else if err = a.commitControl(state); err != nil {
		return err
	}
	if err = boundary("archive-checkpoint"); err != nil {
		return err
	}
	for i, name := range []string{p.OwnedPath(p.Runtime), p.OwnedPath(p.ContainerRuntime())} {
		raw, err := readBackupOwnedFile(root, name, safefs.MaxRecordBytes)
		if err != nil || !v.freshnessFileMatches(name, raw) {
			return fmt.Errorf("freshness runtime drift during publication")
		}
		if string(raw) == string(v.Freshness.After[name]) {
			continue
		}
		d, err := openBackupOwnedParent(root, name)
		if err != nil {
			return err
		}
		err = safefs.ReplaceFile(d, filepath.Base(name), raw, v.Freshness.After[name], 0600)
		d.Close()
		if err == nil {
			err = boundary(fmt.Sprintf("runtime-%d", i))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (v setupNotificationScope) finishFreshnessGate(root *safefs.Directory) error {
	for name, raw := range v.Freshness.After {
		actual, err := readBackupOwnedFile(root, name, safefs.MaxRecordBytes)
		if err != nil || string(actual) != string(raw) {
			return fmt.Errorf("completed freshness runtime unavailable")
		}
	}
	p, err := declarationPaths(root.File().Name(), v.Before.Declaration)
	if err != nil {
		return err
	}
	d, err := safefs.Open(p.State)
	if err != nil {
		return err
	}
	defer d.Close()
	var gate installationGate
	if err = d.ReadJSON("installation.pending", &gate); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if gate.Operation != v.Operation {
		return fmt.Errorf("freshness gate belongs to another operation")
	}
	state, err := openBackupStateWithLease(p.State, false, false, true)
	if err != nil {
		return err
	}
	defer state.Close()
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	if control.ConfigHash != v.Freshness.AfterConfig {
		return fmt.Errorf("completed freshness checkpoint unavailable")
	}
	a, err := openBackupArchive(v.After.Policy.Storage.Path)
	if err != nil {
		return err
	}
	defer a.Close()
	checkpoint, err := a.checkpoint()
	if err != nil || !reflect.DeepEqual(checkpoint, control) {
		return fmt.Errorf("completed freshness archive acknowledgment unavailable")
	}
	return clearInstallationGate(p.State, v.Operation)
}
