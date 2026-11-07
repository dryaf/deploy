package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func setupObservationFixture(t *testing.T, aliases ...string) (string, *backupState, WorkerSetupJournal) {
	t.Helper()
	home := t.TempDir()
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	for _, path := range []string{"archive", ".config/deploy/backup-worker", ".local/state/deploy/backup-worker", ".local/state/deploy/locks", ".local/lib/deploy/backup-worker"} {
		testutil.Must(t, root.Mkdir(path))
	}
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, true)
	testutil.Must(t, err)
	lock.Close()
	w, a := testutil.ID(t, "w"), testutil.ID(t, "a")
	archive, err := enrollBackupArchive(filepath.Join(home, "archive"), a, w)
	testutil.Must(t, err)
	defer archive.Close()
	state, err := openBackupState(filepath.Join(home, ".local/state/deploy/backup-worker/state"), true)
	testutil.Must(t, err)
	t.Cleanup(func() { state.Close() })
	p := configfixture.WorkerPolicy(t)
	if len(aliases) != 0 {
		p.Sources = aliases
	}
	p.Notifications = config.WorkerNotifications{Mode: "disabled"}
	p.Monitoring = config.WorkerMonitoring{}
	p.Storage = config.WorkerStorage{Mode: "home", Path: filepath.Join(home, "archive"), MinFreeBytes: 1}
	p, err = config.NormalizeWorker(p)
	testutil.Must(t, err)
	c := WorkerInstallation{Schema: 1, WorkerID: w, ArchiveID: a, ConfigHash: strings.Repeat("a", 64), Policy: p, Qualification: map[string]bool{"source_restrictions": true, "rootless_runtime": true}}
	for _, alias := range p.Sources {
		c.Sources = append(c.Sources, source.WorkerSource{Alias: alias, Host: "example.com", User: "app", Port: 22, KeyFile: "/private/key", KnownHosts: "/private/known", SourceID: testutil.ID(t, "s")})
	}
	control := ArchiveControl{Schema: 1, ReportingDisabled: true, ArchiveID: a, WriterID: w, Epoch: 1, Revision: 1, ConfigHash: c.ConfigHash, Instances: map[string]ExpectedInstance{}, RetiredSources: map[string]string{}, Pins: map[string]string{}}
	testutil.Must(t, state.put("control", control))
	testutil.Must(t, state.put("mode", "writer"))
	testutil.Must(t, state.put("installation", "staged-awaiting-qualification"))
	testutil.Must(t, archive.commitControl(state))
	body, err := json.Marshal(c)
	testutil.Must(t, err)
	const runtimePath = ".config/deploy/backup-worker/runtime.json"
	testutil.Must(t, os.WriteFile(filepath.Join(home, runtimePath), body, 0600))
	helper := []byte("fixture retained helper bytes")
	testutil.Must(t, os.WriteFile(filepath.Join(home, ".local/lib/deploy/backup-worker/deploy"), helper, 0700))
	testutil.Must(t, root.WriteJSON(".local/lib/deploy/backup-worker/artifact.json", helperManifest(fmtDigest(helper)), false))
	j := WorkerSetupJournal{Schema: 1, ID: testutil.ID(t, "n"), WorkerID: w, ArchiveID: a, Stage: "complete", StateInitialized: true, RequestHash: strings.Repeat("b", 64), OwnedFiles: map[string]string{runtimePath: fmtDigest(body)}, Declaration: &SetupDeclaration{Version: 1, UID: os.Getuid(), Home: home, DesiredHash: strings.Repeat("c", 64), HelperSHA256: fmtDigest(helper)}}
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", j, false))
	return home, state, j
}

func helperManifest(digest string) helper.HelperArtifactManifest {
	return helper.HelperArtifactManifest{Schema: 1, Role: "backup-worker", Current: digest}
}

func TestDeclarativeRemovalStatusPreservesRetainedAuthority(t *testing.T) {
	home, _, j := setupObservationFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	const configPath = ".config/deploy/backup-worker/runtime.json"
	var installed WorkerInstallation
	testutil.Must(t, root.ReadJSON(configPath, &installed))
	lifecycle := workerLifecycleJournal{Schema: 1, Operation: "uninstall", Stage: "prepared", Installation: installed}
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/lifecycle.json", lifecycle, false))
	observed, err := InspectSetup(context.Background(), home)
	if err != nil || observed.Stage != "removing" {
		t.Fatal(observed, err)
	}
	j.Stage = "uninstalled"
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", j, true))
	lifecycle.Stage = "complete"
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/lifecycle.json", lifecycle, true))
	if _, err = InspectSetup(context.Background(), home); err == nil {
		t.Fatal("remaining owned files ignored")
	}
	testutil.Must(t, os.Remove(filepath.Join(home, configPath)))
	observed, err = InspectSetup(context.Background(), home)
	if err != nil || observed.Stage != "uninstalled" || observed.ArchivePath != installed.Policy.Storage.Path {
		t.Fatal(observed, err)
	}
	testutil.Must(t, os.Remove(filepath.Join(home, ".local/state/deploy/backup-worker/lifecycle.json")))
	if _, err = InspectSetup(context.Background(), home); err == nil {
		t.Fatal("missing removal receipt ignored")
	}
	if _, err = os.Stat(filepath.Join(home, configPath)); !os.IsNotExist(err) {
		t.Fatal("status recreated runtime", err)
	}
}

func TestDeclarativeBackupStatusChecksAuthorityAndSelectedEvidence(t *testing.T) {
	home, state, j := setupObservationFixture(t)
	observed, err := InspectSetup(context.Background(), home)
	if err != nil || observed.WorkerID != j.WorkerID || observed.ArchiveID != j.ArchiveID || !observed.ReportingDisabled || len(observed.PendingQualification) != 2 {
		t.Fatal(observed, err)
	}
	testutil.Must(t, state.put("qualification", WorkerQualification{Schema: 1, WorkerID: j.WorkerID, ArchiveID: j.ArchiveID, ConfigHash: strings.Repeat("a", 64), Architecture: runtime.GOARCH, CheckedAt: time.Now().UTC(), Checks: map[string]QualificationCheck{"storage_crash_recovery": {Passed: true, Evidence: "synthetic test evidence"}, "isolated_application_restore": {Passed: true, Evidence: "synthetic test evidence"}}}))
	var proof WorkerQualification
	testutil.Must(t, state.get("qualification", &proof))
	c, err := loadWorkerInstallation(filepath.Join(home, ".config/deploy/backup-worker/runtime.json"))
	testutil.Must(t, err)
	binding, err := backupQualificationBinding(j, c)
	testutil.Must(t, err)
	testutil.Must(t, state.put("qualification_binding", backupQualificationBindingRecord{1, binding, notify.Hash(proof)}))
	testutil.Must(t, state.put("installation", "active"))
	observed, err = InspectSetup(context.Background(), home)
	if err != nil || len(observed.PendingQualification) != 0 || observed.Installation != "active" {
		t.Fatal(observed, err)
	}
	// Presence of an archive directory alone is never enough.
	testutil.Must(t, os.Rename(filepath.Join(home, "archive/archive.json"), filepath.Join(home, "archive/archive.saved")))
	if _, err = InspectSetup(context.Background(), home); err == nil {
		t.Fatal("missing archive identity accepted")
	}
}

func TestDeclarativeBackupStatusRefusesDriftAndDoesNotRecreateState(t *testing.T) {
	for _, path := range []string{".local/lib/deploy/backup-worker/deploy", ".local/lib/deploy/backup-worker/artifact.json", ".config/deploy/backup-worker/runtime.json", ".local/state/deploy/backup-worker/state/worker.db", ".local/state/deploy/locks/backup-worker-setup.lock", ".local/state/deploy/backup-worker/setup.json"} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			home, state, _ := setupObservationFixture(t)
			testutil.Must(t, state.Close())
			testutil.Must(t, os.Remove(filepath.Join(home, path)))
			if _, err := InspectSetup(context.Background(), home); err == nil {
				t.Fatal("lost authority accepted", path)
			}
			if _, err := os.Lstat(filepath.Join(home, path)); !os.IsNotExist(err) {
				t.Fatal("status recreated missing authority", err)
			}
		})
	}
}
