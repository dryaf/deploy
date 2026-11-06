package backup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/helper"
	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"golang.org/x/crypto/ssh"
)

func sharedSetupFixture(t *testing.T) (string, WorkerInstallation, WorkerSetupJournal) {
	t.Helper()
	home, c, spec, deps, session := backupCutoverFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	for _, p := range []string{".config/systemd/user", ".config/containers/systemd", workerTrustDirectory, "archive"} {
		testutil.Must(t, root.Mkdir(p))
	}
	c.Image = "example.invalid/backup@sha256:" + strings.Repeat("a", 64)
	c.Policy.Storage.Path = filepath.Join(home, "archive")
	c.Policy.Storage.RequireMount = home
	for _, alias := range c.Policy.Sources {
		c.Sources = append(c.Sources, deploysource.WorkerSource{Alias: alias, Host: "fixture.invalid", User: "backup", Port: 22, SourceID: testutil.ID(t, "s"), KeyFile: filepath.Join(home, workerTrustDirectory, alias+".key"), KnownHosts: filepath.Join(home, workerTrustDirectory, alias+".known_hosts")})
	}
	testutil.Must(t, validateWorkerInstallation(c))
	a, err := enrollBackupArchive(c.Policy.Storage.Path, c.ArchiveID, c.WorkerID)
	testutil.Must(t, err)
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	control.Epoch = a.identity.Epoch
	testutil.Must(t, state.put("control", control))
	testutil.Must(t, a.commitControl(state))
	testutil.Must(t, a.Close())
	testutil.Must(t, state.put("installation", "active"))
	testutil.Must(t, state.put("min_free_bytes", int64(123)))
	var evidence backupCutoverEvidence
	testutil.Must(t, backupTransferTx(context.Background(), state, func(tx *sql.Tx) error { var e error; evidence, e = readBackupCutoverEvidence(tx); return e }))
	testutil.Must(t, state.Close())
	spec.ConfigHash, spec.EvidenceHash = notify.Hash(c), notify.Hash(evidence)
	raw, err := json.Marshal(c)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(home, recoveryFixtureConfig), raw, 0600))
	testutil.Must(t, os.WriteFile(filepath.Join(home, recoveryFixtureTrust), []byte("old host key"), 0600))
	j := WorkerSetupJournal{Schema: 1, ID: testutil.ID(t, "n"), WorkerID: c.WorkerID, ArchiveID: c.ArchiveID, ArchivePath: c.Policy.Storage.Path, StateInitialized: true, Stage: "complete", RequestHash: "old-request", Files: map[string]setupFileChange{}, OwnedFiles: map[string]string{recoveryFixtureConfig: fmtDigest(raw), recoveryFixtureTrust: fmtDigest([]byte("old host key"))}}
	j.SourceEnrollments = map[string]deploysource.SourceEnrollmentResult{}
	for _, source := range c.Sources {
		j.SourceEnrollments[source.Alias] = deploysource.SourceEnrollmentResult{Schema: 1, WorkerID: c.WorkerID, SourceID: source.SourceID}
	}
	digest := fmtDigest([]byte(recoveryFixtureHelper))
	release := ".local/lib/deploy/backup-worker/releases/" + digest
	testutil.Must(t, root.Mkdir(release))
	for _, p := range []string{release + "/deploy", ".local/lib/deploy/backup-worker/deploy"} {
		testutil.Must(t, os.WriteFile(filepath.Join(home, p), []byte(recoveryFixtureHelper), 0700))
	}
	testutil.Must(t, root.WriteJSON(".local/lib/deploy/backup-worker/artifact.json", helper.HelperArtifactManifest{Schema: 1, Role: "backup-worker", Current: digest}, false))
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", j, true))
	testutil.Must(t, runBackupCutover(context.Background(), home, c, spec, false, deps))
	for name, body := range deps.Services.(*cutoverFixtureServices).units {
		testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/systemd/user", name), []byte(body), 0600))
	}
	d, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	defer d.Close()
	profile := install.ManagedReportProfile{ManifestHash: notify.Hash("fixture-manifest"), Identity: session.m.Identity, UnitDirectory: session.m.UnitDirectory, Credentials: session.CredentialPaths()}
	testutil.Must(t, prepareSharedWorkerSetupUsing(home, root, d, &j, "update", func(r string, id notify.Identity, p notify.Producer, route string) (install.ManagedReportProfile, error) {
		if r != session.m.Root || id != session.m.Identity || p != session.p || route != "ops" {
			t.Fatal("gateway profile pins changed")
		}
		return profile, nil
	}))
	return home, c, j
}

func TestBackupSharedKeyRotation(t *testing.T) {
	home, c, _ := sharedSetupFixture(t)
	source := c.Sources[0]
	_, private, err := ed25519.GenerateKey(rand.Reader)
	testutil.Must(t, err)
	block, err := ssh.MarshalPrivateKey(private, "fixture")
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(source.KeyFile, pem.EncodeToMemory(block), 0600))
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$BACKUP_RECOVERY_CALLS\"\ncase \"$*\" in *is-enabled*) echo enabled;; esac\n"
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("BACKUP_RECOVERY_CALLS", log)
	qualified := 0
	fail := errors.New("synthetic source requalification failed")
	qualify := func(s deploysource.WorkerSource, _ config.WorkerTimeouts) error {
		if s != source {
			t.Fatal("rotation changed source")
		}
		qualified++
		if qualified == 1 {
			return fail
		}
		return nil
	}
	prepare := workerLifecycleRequest{Operation: "rotate-key-prepare", Source: source.Alias}
	testutil.Must(t, workerLifecycleUsing(home, prepare, qualify))
	var j workerLifecycleJournal
	testutil.Must(t, root.ReadJSON(".local/state/deploy/backup-worker/lifecycle.json", &j))
	if j.NotificationOwner == nil || j.Schema != 2 || j.Stage != "prepared" {
		t.Fatal("rotation did not pin shared owner")
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(j.PublicKey))
	testutil.Must(t, err)
	finish := workerLifecycleRequest{Operation: "rotate-key-finish", Source: source.Alias, Results: []deploysource.SourceEnrollmentResult{{SourceID: source.SourceID, WorkerID: c.WorkerID, Fingerprint: ssh.FingerprintSHA256(key)}}}
	if err = workerLifecycleUsing(home, finish, qualify); !errors.Is(err, fail) {
		t.Fatal(err)
	}
	// Resume after the private key changed but before source requalification and
	// owned-file publication. The same prepared public key remains canonical.
	testutil.Must(t, workerLifecycleUsing(home, finish, qualify))
	testutil.Must(t, workerLifecycleUsing(home, finish, qualify))
	var setup WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &setup))
	raw, err := os.ReadFile(source.KeyFile)
	testutil.Must(t, err)
	relative, err := filepath.Rel(home, source.KeyFile)
	testutil.Must(t, err)
	if setup.OwnedFiles[relative] != sharedSetupDigest(raw) {
		t.Fatal("rotation left stale setup ownership hash")
	}
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	owner, err := currentSharedBackupOwner(state)
	testutil.Must(t, err)
	if owner == nil || *owner != *j.NotificationOwner {
		t.Fatal("rotation changed notification owner")
	}
	if requireLegacyNotificationOwner(state) == nil {
		t.Fatal("rotation unfenced legacy sends")
	}
	calls, err := os.ReadFile(log)
	testutil.Must(t, err)
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.Contains(line, " start ") && strings.Contains(line, "notifier") {
			t.Fatal("rotation started legacy sender")
		}
	}
}

func TestBackupSharedQualifiedActivation(t *testing.T) {
	home, c, j := sharedSetupFixture(t)
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	c.Policy.Monitoring.BackupPingURLFile = filepath.Join(home, "backup-monitor")
	c.Policy.Monitoring.DailyDeliveryPingURLFile = filepath.Join(home, "report-monitor")
	testutil.Must(t, os.WriteFile(c.Policy.Monitoring.BackupPingURLFile, []byte("https://hc-ping.com/11111111-1111-4111-8111-111111111111"), 0600))
	testutil.Must(t, os.WriteFile(c.Policy.Monitoring.DailyDeliveryPingURLFile, []byte("https://hc-ping.com/22222222-2222-4222-8222-222222222222"), 0600))
	c.Qualification = map[string]bool{"source_restrictions": true, "rootless_runtime": true}
	proof := WorkerQualification{Schema: 1, ConfigHash: c.ConfigHash, WorkerID: c.WorkerID, ArchiveID: c.ArchiveID, CheckedAt: time.Now().UTC(), Checks: map[string]QualificationCheck{}}
	for _, name := range []string{"storage_crash_recovery", "notification_receipt", "missed_worker_route", "daily_delivery_route", "isolated_application_restore"} {
		proof.Checks[name] = QualificationCheck{Passed: true, Evidence: "synthetic qualification fixture"}
	}
	testutil.Must(t, state.put("qualification", proof))
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$BACKUP_RECOVERY_CALLS\"\n"), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("BACKUP_RECOVERY_CALLS", log)
	verify := func(h string, _ WorkerInstallation, _ *backupState, o backupNotificationOwner) error {
		if h != home || o != j.Shared.Owner {
			t.Fatal("activation owner changed")
		}
		return nil
	}
	qualify := func(deploysource.WorkerSource, config.WorkerTimeouts) error { return nil }
	if activateWorkerUsing(c, state, verify, qualify) == nil {
		t.Fatal("activation skipped successful backup gate")
	}
	testutil.Must(t, state.put("run", WorkerRun{Complete: true, ConfigHash: c.ConfigHash, FinishedAt: time.Now().UTC()}))
	testutil.Must(t, activateWorkerUsing(c, state, verify, qualify))
	calls, err := os.ReadFile(log)
	testutil.Must(t, err)
	if !strings.Contains(string(calls), "deploy-backup-report.timer") || !strings.Contains(string(calls), "deploy-backup-report.path") || strings.Contains(string(calls), "notifier") || strings.Contains(string(calls), "signal") {
		t.Fatal("activation selected wrong notification owner", string(calls))
	}
	if requireLegacyNotificationOwner(state) == nil {
		t.Fatal("activation removed legacy fence")
	}
}

func TestBackupSharedSetupOwnershipAndUnits(t *testing.T) {
	home, c, j := sharedSetupFixture(t)
	testutil.Must(t, requireWorkerSetupOwnership(home, &j))
	if j.Schema != 2 || j.Shared == nil {
		t.Fatal("shared setup authority not retained")
	}
	units, err := workerSetupUnits(c, backupCutoverPaths(home, c), &j)
	testutil.Must(t, err)
	if !strings.Contains(units["deploy-backup-report.service"], j.Shared.Profile.Credentials[0]) {
		t.Fatal("gateway credential exclusion lost")
	}
	for _, name := range workerActivationUnitsFor(true) {
		if strings.Contains(name, "notifier") || strings.Contains(name, "signal") {
			t.Fatal("legacy sender would be activated", name)
		}
	}
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	if requireLegacyNotificationOwner(state) == nil {
		t.Fatal("shared setup removed legacy fence")
	}
	owner := j.Shared.Owner
	owner.Mode = "fenced"
	testutil.Must(t, state.put("notification_owner", owner))
	if requireWorkerSetupOwner(state, &j) == nil {
		t.Fatal("changed ownership accepted")
	}
}

func TestBackupSharedSetupSIGKILLRecovery(t *testing.T) {
	home, c, j := sharedSetupFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	j.Updating = true
	j.PreviousRequestHash = j.RequestHash
	j.RequestHash = "new-request"
	j.Stage = "prepared"
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", j, true))
	before, err := os.ReadFile(filepath.Join(home, recoveryFixtureConfig))
	testutil.Must(t, err)
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$BACKUP_RECOVERY_CALLS\"\ncase \"$*\" in\n *is-enabled*) echo enabled;;\n *show*) echo loaded;;\nesac\n"
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("BACKUP_RECOVERY_CALLS", log)
	child := startInterruptedSetup(t, home)
	testutil.Must(t, child.Process.Kill())
	if child.Wait() == nil {
		t.Fatal("child survived kill")
	}
	testutil.Must(t, recoverWorkerSetup(home))
	testutil.Must(t, recoverWorkerSetup(home))
	after, err := os.ReadFile(filepath.Join(home, recoveryFixtureConfig))
	testutil.Must(t, err)
	if string(after) != string(before) {
		t.Fatal("backup configuration not restored")
	}
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	testutil.Must(t, requireWorkerSetupOwner(state, &j))
	if requireLegacyNotificationOwner(state) == nil {
		t.Fatal("rollback restored legacy sender authority")
	}
	calls, err := os.ReadFile(log)
	testutil.Must(t, err)
	if !strings.Contains(string(calls), "enable --now deploy-backup-report.timer") || !strings.Contains(string(calls), "enable --now deploy-backup-report.path") {
		t.Fatal("report triggers not restored", string(calls))
	}
	for _, line := range strings.Split(string(calls), "\n") {
		if strings.Contains(line, "enable --now") && (strings.Contains(line, "notifier") || strings.Contains(line, "signal")) {
			t.Fatal("rollback enabled legacy sender", line)
		}
	}
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	if control.ConfigHash != c.ConfigHash {
		t.Fatal("archive configuration not restored")
	}
}
