//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

// A real shared producer/store and completed cutover, with a real local archive
// and private source keys. The provider and service manager remain offline fakes.
func sharedSourceExpansionFixture(t *testing.T, aliases ...string) (string, WorkerSetupRequest, WorkerSetupRequest, func()) {
	return sharedSourceExpansionFixtureWithNotification(t, nil, aliases...)
}
func sharedSourceExpansionFixtureWithNotification(t *testing.T, notification *config.NotificationDeclaration, aliases ...string) (string, WorkerSetupRequest, WorkerSetupRequest, func()) {
	return sharedSourceExpansionFixtureWithHook(t, notification, nil, aliases...)
}
func sharedSourceExpansionFixtureWithHook(t *testing.T, notification *config.NotificationDeclaration, hook func(string, *safefs.Directory, *safefs.Directory, *WorkerSetupJournal), aliases ...string) (string, WorkerSetupRequest, WorkerSetupRequest, func()) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("rootless source expansion")
	}
	home, c, _, manifest, deps, session := freshSharedReportingFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	dir, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	defer dir.Close()
	var j WorkerSetupJournal
	testutil.Must(t, dir.ReadJSON("setup.json", &j))
	testutil.Must(t, helper.InstallSelf(home, "backup-worker"))
	f, err := os.Open("/proc/self/exe")
	testutil.Must(t, err)
	j.Declaration.HelperSHA256, err = safefs.DigestFile(f)
	f.Close()
	testutil.Must(t, err)
	j.StateInitialized = true
	j.Files = map[string]setupFileChange{}
	c.Image = "registry.invalid/backup@sha256:" + notify.Hash("fixture image")
	c.Policy.Storage = config.WorkerStorage{Mode: "home", Path: filepath.Join(home, "archive"), MinFreeBytes: 1}
	if len(aliases) != 0 {
		c.Policy.Sources = aliases
	}
	c.Policy, err = config.NormalizeWorker(c.Policy)
	testutil.Must(t, err)
	for _, alias := range c.Policy.Sources {
		c.Sources = append(c.Sources, source.WorkerSource{Alias: alias, Host: "source.invalid", User: "app", Port: 22, SourceID: testutil.ID(t, "s")})
	}
	testutil.Must(t, root.WriteJSON(workerRuntimeConfig, c, true))
	completePolicyReviewSources(t, home, &j)
	testutil.Must(t, root.ReadJSON(workerRuntimeConfig, &c))
	c.ConfigHash = ""
	c.ConfigHash = notify.Hash(c)
	testutil.Must(t, root.WriteJSON(workerRuntimeConfig, c, true))
	raw, err := safefs.ReadFile(root, workerRuntimeConfig, 8<<20)
	testutil.Must(t, err)
	j.OwnedFiles[workerRuntimeConfig] = sharedSetupDigest(raw)
	p := c.Policy
	desired := config.DesiredWorkers{Notification: notification, Backup: &config.BackupDeclaration{Sources: p.Sources, Storage: config.DesiredStorage{Mode: p.Storage.Mode, Path: p.Storage.Path, MinFreeBytes: p.Storage.MinFreeBytes}, Schedule: p.Schedule, Timezone: p.Timezone, Freshness: p.Freshness, MaxRunTime: p.MaxRunTime, MaxSnapshotBytes: p.MaxSnapshotBytes, Timeouts: p.Timeouts, Retention: p.Retention, Notifications: config.DesiredBackupNotifications{Worker: "manager", Route: j.Declaration.NotificationRoute, WorkerNotifications: p.Notifications}, Monitoring: p.Monitoring, RestoreChecks: p.RestoreChecks}}
	j.Declaration.Desired = &desired
	j.Declaration.DesiredHash, err = desired.Fingerprint()
	testutil.Must(t, err)
	j.Declaration.NotificationDesiredHash, err = desired.NotificationFingerprint()
	testutil.Must(t, err)
	j.Declaration.User, j.Declaration.Architecture = "fixture", runtime.GOARCH
	j.Declaration.Sources, j.SourceKeys = map[string]string{}, map[string]string{}
	before := WorkerSetupRequest{Schema: 1, Operation: "apply", Declaration: j.Declaration, Policy: c.Policy, Image: c.Image, RelayImage: c.RelayImage}
	for _, s := range c.Sources {
		j.Declaration.Sources[s.Alias] = notify.Hash("source helper")
		j.SourceKeys[s.Alias] = j.SourceEnrollments[s.Alias].Fingerprint
		known, err := os.ReadFile(s.KnownHosts)
		testutil.Must(t, err)
		j.OwnedFiles[filepath.Join(workerTrustDirectory, s.Alias+".known_hosts")] = sharedSetupDigest(known)
		before.Sources = append(before.Sources, SetupSource{Alias: s.Alias, Host: s.Host, User: s.User, Port: s.Port, KnownHosts: string(known)})
	}
	if hook != nil {
		hook(home, root, dir, &j)
	}
	j.RequestHash = workerSetupRequestHash(before)
	testutil.Must(t, root.Mkdir("archive"))
	a, err := enrollBackupArchive(c.Policy.Storage.Path, c.ArchiveID, c.WorkerID)
	testutil.Must(t, err)
	state, err := openInstalledBackupState(home, false)
	testutil.Must(t, err)
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	control.ConfigHash, control.Epoch = c.ConfigHash, a.identity.Epoch
	testutil.Must(t, state.put("control", control))
	testutil.Must(t, state.put("mode", "writer"))
	testutil.Must(t, state.put("installation", "active"))
	testutil.Must(t, state.put("min_free_bytes", c.Policy.Storage.MinFreeBytes))
	testutil.Must(t, a.commitControl(state))
	a.Close()
	state.Close()
	j.HomeFilesystem, err = homeArchiveFilesystem(c.Policy.Storage.Path)
	testutil.Must(t, err)
	testutil.Must(t, dir.WriteJSON("setup.json", j, true))
	testutil.Must(t, connectSharedReporting(context.Background(), home, j.Declaration.DesiredHash, manifest, deps))
	for name, body := range deps.Services.(*cutoverFixtureServices).units {
		testutil.Must(t, root.Mkdir(".config/systemd/user"))
		testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/systemd/user", name), []byte(body), 0600))
	}
	profile := install.ManagedReportProfile{ManifestHash: manifest, Identity: session.m.Identity, UnitDirectory: session.m.UnitDirectory, Credentials: session.CredentialPaths()}
	testutil.Must(t, prepareSharedWorkerSetupUsing(home, root, dir, &j, "update", func(string, notify.Identity, notify.Producer, string) (install.ManagedReportProfile, error) {
		return profile, nil
	}))
	cutoverPath := filepath.Join(home, ".local/state/deploy/backup-worker/state", backupCutoverFile)
	cutover, err := os.ReadFile(cutoverPath)
	testutil.Must(t, err)
	owner, producer := *j.Shared, *j.Producer
	after := expandedSourceRequest(t, before)
	verify := func() {
		t.Helper()
		r, err := safefs.Open(home)
		testutil.Must(t, err)
		defer r.Close()
		var current WorkerSetupJournal
		testutil.Must(t, r.ReadJSON(backupSetupFile, &current))
		if !reflect.DeepEqual(current.Shared, &owner) || !reflect.DeepEqual(current.Producer, &producer) {
			t.Fatal("source expansion changed delivery ownership")
		}
		s, err := openInstalledBackupState(home, false)
		testutil.Must(t, err)
		testutil.Must(t, requireWorkerSetupOwner(s, &current))
		if requireLegacyNotificationOwner(s) == nil {
			t.Fatal("source expansion enabled a legacy sender")
		}
		s.Close()
		services := deps.Services.(*cutoverFixtureServices)
		services.started = false
		deps.Managed = nil
		testutil.Must(t, connectSharedReporting(context.Background(), home, after.Declaration.DesiredHash, manifest, deps))
		if services.started {
			t.Fatal("source expansion restarted gateway admission")
		}
		retained, err := os.ReadFile(cutoverPath)
		testutil.Must(t, err)
		if string(retained) != string(cutover) {
			t.Fatal("source expansion rewrote original cutover evidence")
		}
	}
	return home, before, after, verify
}

func expandedSourceRequest(t *testing.T, before WorkerSetupRequest) WorkerSetupRequest {
	t.Helper()
	raw, err := json.Marshal(before)
	testutil.Must(t, err)
	var after WorkerSetupRequest
	testutil.Must(t, json.Unmarshal(raw, &after))
	after.Policy.Sources = append(after.Policy.Sources, "extra")
	after.Sources = append(after.Sources, SetupSource{Alias: "extra", Host: "extra.invalid", User: "app", Port: 22, KnownHosts: before.Sources[0].KnownHosts})
	after.Declaration.Sources["extra"] = notify.Hash("extra helper")
	after.Declaration.Desired.Backup.Sources = append([]string(nil), after.Policy.Sources...)
	after.Declaration.DesiredHash, err = after.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
	return after
}

func TestSharedSourceExpansionReservationIsReadOnlyAndResumeOnly(t *testing.T) {
	home, _, request, _ := sharedSourceExpansionFixture(t)
	root, dir, keys, release := expansionRoots(t, home)
	var j WorkerSetupJournal
	testutil.Must(t, dir.ReadJSON("setup.json", &j))
	originalProducer, originalShared := *j.Producer, *j.Shared
	tree := policyReviewTree(t, home)
	reservation, err := inspectSetupUpdateReservation(root, dir, &j, request)
	testutil.Must(t, err)
	if reservation != originalProducer || j.SourceExpansion != nil || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("shared expansion preflight changed reservation or files")
	}
	testutil.Must(t, beginWorkerSetupUpdate(root, dir, &j, request))
	for _, boundary := range []string{"intent", "key", "enrolled"} {
		if boundary == "key" {
			_, err = setupSourceKey(home, keys, dir, &j, "extra")
			testutil.Must(t, err)
		}
		if boundary == "enrolled" {
			e := source.SourceEnrollmentResult{Schema: 1, WorkerID: j.WorkerID, SourceID: testutil.ID(t, "s"), Fingerprint: j.SourceKeys["extra"]}
			testutil.Must(t, recordSetupSourceEnrollment(dir, &j, "extra", e))
		}
		tree = policyReviewTree(t, home)
		reservation, err = inspectPendingSetupUpdateReservation(root, dir, &j, request)
		testutil.Must(t, err)
		if reservation != originalProducer || !reflect.DeepEqual(j.Shared, &originalShared) || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
			t.Fatal("pending shared expansion changed authority", boundary)
		}
	}
	for _, damage := range []string{"scope", "producer", "generation", "owner", "request"} {
		raw, err := json.Marshal(j)
		testutil.Must(t, err)
		var candidate WorkerSetupJournal
		testutil.Must(t, json.Unmarshal(raw, &candidate))
		proposed := request
		switch damage {
		case "scope":
			candidate.PreviousDeclaration.NotificationDesiredHash = notify.Hash("changed gateway")
		case "producer":
			candidate.Producer.Producer.Epoch = testutil.ID(t, "n")
		case "generation":
			candidate.Producer.Generation = testutil.ID(t, "g")
		case "owner":
			candidate.Shared.Profile.ManifestHash = notify.Hash("changed manifest")
		case "request":
			proposed.Policy.Schedule = "daily"
		}
		tree = policyReviewTree(t, home)
		if _, err = inspectPendingSetupUpdateReservation(root, dir, &candidate, proposed); err == nil || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
			t.Fatal("changed pending shared expansion was accepted", damage, err)
		}
	}
	release()
	if _, _, err = InspectReservedProducer(home, j.Declaration.NotificationDesiredHash, j.Declaration.NotificationRoot, j.Declaration.NotificationRoute); err == nil {
		t.Fatal("pending expansion admitted as a completed dependency")
	}
}
