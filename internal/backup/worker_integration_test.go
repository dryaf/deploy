//go:build linux && backupintegration

package backup

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	deploysource "github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

// This checks the actual systemd supervisor and production helper in the private
// rootless container. It is not a VM power-loss or complete worker qualification.
func TestRootlessBackupRecoveryIntegration(t *testing.T) {
	if os.Getenv("DEPLOY_BACKUP_INTEGRATION") != "isolated" {
		t.Skip("requires disposable backup integration harness")
	}
	home, err := os.UserHomeDir()
	testutil.Must(t, err)
	if home != "/home/podman" || os.Getuid() != 1000 {
		t.Fatal("refusing non-disposable worker account")
	}
	t.Run("maintenance-health", func(t *testing.T) { checkMaintenanceUnitHealth(t, home) })
	checkBackupCaptureLockLifetime(t, true)
	checkBackupSnapshotLockHandoff(t, true)
	setupRecoveryFixture(t, home)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	for name, body := range map[string]string{
		"deploy-backup.service": "[Service]\nType=oneshot\nExecStart=/usr/bin/true\n",
		"deploy-backup.timer":   "[Timer]\nOnActiveSec=30min\nUnit=deploy-backup.service\n[Install]\nWantedBy=timers.target\n",
	} {
		testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/systemd/user", name), []byte(body), 0600))
	}
	systemctl := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("systemctl %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	systemctl("daemon-reload")
	systemctl("enable", "--now", "deploy-backup.timer")
	stateRoot, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	defer stateRoot.Close()
	var journal WorkerSetupJournal
	testutil.Must(t, stateRoot.ReadJSON("setup.json", &journal))
	checkStagedSSHTrust(t, home, root, stateRoot, &journal)
	testutil.Must(t, installRecoverySupervisorFrom(home, root, stateRoot, &journal, "/usr/local/bin/deploy-backup-integration"))
	t.Cleanup(func() {
		exec.Command("systemctl", "--user", "disable", "--now", recoveryTimer, "deploy-backup.timer").Run()
	})
	cmd := startInterruptedSetup(t, home)
	testutil.Must(t, cmd.Process.Kill())
	if err = cmd.Wait(); err == nil {
		t.Fatal("coordinator was not killed")
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		journal = WorkerSetupJournal{}
		testutil.Must(t, stateRoot.ReadJSON("setup.json", &journal))
		if !journal.RecoveryNeeded {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if journal.RecoveryNeeded {
		out, _ := exec.Command("journalctl", "--user", "-u", "deploy-backup-recovery.service", "--no-pager", "-n", "30").CombinedOutput()
		t.Fatalf("automatic recovery did not finish: %s", out)
	}
	assertSetupRecovered(t, home)
	if systemctl("is-enabled", "deploy-backup.timer") != "enabled" || systemctl("is-active", "deploy-backup.timer") != "active" {
		t.Fatal("backup timer activity not restored")
	}
	t.Log("SIGKILL recovery restored configuration, helper, archive control, state policy, and active timer using the production helper and real user systemd")
	// The production validator also runs inside this nested rootless environment.
	out, err := exec.Command("/usr/local/bin/deploy-backup-integration", "__worker_selftest").CombinedOutput()
	if err != nil {
		t.Fatalf("isolated validator: %v: %s", err, out)
	}
	t.Logf("production isolated validator: %s", out)
	checkUninstallAfterRotation(t, home, root)
}

func checkMaintenanceUnitHealth(t *testing.T, home string) {
	t.Helper()
	units := filepath.Join(home, ".config/systemd/user")
	testutil.Must(t, os.MkdirAll(units, 0700))
	ctl := func(args ...string) {
		t.Helper()
		out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("systemctl %v: %v: %s", args, err, out)
		}
	}
	names := []string{"deploy-backup", "deploy-backup-notifier", "deploy-backup-health", "deploy-backup-maintenance"}
	t.Cleanup(func() {
		for _, name := range names {
			exec.Command("systemctl", "--user", "disable", "--now", name+".timer").Run()
			exec.Command("systemctl", "--user", "stop", name+".service").Run()
			exec.Command("systemctl", "--user", "reset-failed", name+".service").Run()
			os.Remove(filepath.Join(units, name+".timer"))
			os.Remove(filepath.Join(units, name+".service"))
		}
		exec.Command("systemctl", "--user", "daemon-reload").Run()
	})
	for _, name := range names {
		command := "/usr/bin/true"
		if name == "deploy-backup-maintenance" {
			command = "/usr/bin/false"
		}
		testutil.Must(t, os.WriteFile(filepath.Join(units, name+".service"), []byte("[Service]\nType=oneshot\nExecStart="+command+"\n"), 0600))
		testutil.Must(t, os.WriteFile(filepath.Join(units, name+".timer"), []byte("[Timer]\nOnCalendar=*-*-* 23:59:59 UTC\nUnit="+name+".service\n[Install]\nWantedBy=timers.target\n"), 0600))
	}
	ctl("daemon-reload")
	for _, name := range names {
		ctl("enable", "--now", name+".timer")
	}
	a, state, c, _ := healthyWorkerFixture(t)
	c.Policy.Storage.RequireMount = "/proc"
	c.Policy.Storage.MinFreeBytes = 1
	probe := collectWorkerHealth(c, a.dir.File().Name(), state)
	if !probe.Healthy {
		t.Fatalf("idle maintenance service is not healthy: %+v", probe)
	}
	ctl("disable", "deploy-backup-maintenance.timer")
	probe = collectWorkerHealth(c, a.dir.File().Name(), state)
	if probe.BackupHealthy || !strings.Contains(strings.Join(probe.Diagnostics, "\n"), "deploy-backup-maintenance.timer") {
		t.Fatalf("disabled maintenance timer undetected: %+v", probe)
	}
	ctl("enable", "deploy-backup-maintenance.timer")
	if err := exec.Command("systemctl", "--user", "start", "deploy-backup-maintenance.service").Run(); err == nil {
		t.Fatal("fixture maintenance service did not fail")
	}
	probe = collectWorkerHealth(c, a.dir.File().Name(), state)
	if probe.BackupHealthy || !strings.Contains(strings.Join(probe.Diagnostics, "\n"), "deploy-backup-maintenance.service") {
		t.Fatalf("failed maintenance service undetected: %+v", probe)
	}
	t.Log("real user systemd: idle maintenance healthy; disabled timer and failed service unhealthy")
}

// This fixture acknowledges source revocation locally; source enrollment and
// real revocation remain a separate integration qualification. Worker lifecycle
// requests themselves use the production entry point and real user systemd.
func checkUninstallAfterRotation(t *testing.T, home string, root *safefs.Directory) {
	t.Helper()
	var setup WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &setup))
	c := WorkerInstallation{Schema: 1, WorkerID: setup.WorkerID, ArchiveID: setup.ArchiveID, ConfigHash: strings.Repeat("a", 64), Policy: configfixture.WorkerPolicy(t)}
	for _, alias := range c.Policy.Sources {
		c.Sources = append(c.Sources, deploysource.WorkerSource{Alias: alias, Host: "127.0.0.1", User: "podman", Port: 22, KeyFile: filepath.Join(home, ".ssh/deploy-integration"), KnownHosts: filepath.Join(home, ".ssh/deploy-known-hosts"), SourceID: testutil.ID(t, "s")})
	}
	body, err := json.Marshal(c)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(home, recoveryFixtureConfig), body, 0600))
	setup.OwnedFiles[recoveryFixtureConfig] = fmtDigest(body)
	for _, name := range []string{"deploy-backup.service", "deploy-backup.timer"} {
		path := filepath.Join(".config/systemd/user", name)
		body, err := os.ReadFile(filepath.Join(home, path))
		testutil.Must(t, err)
		setup.OwnedFiles[path] = fmtDigest(body)
	}
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", setup, true))
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/lifecycle.json", workerLifecycleJournal{Schema: 1, Operation: "rotate-key", Stage: "complete", Installation: c}, false))
	request := func(r workerLifecycleRequest) workerLifecycleJournal {
		t.Helper()
		body, err := json.Marshal(r)
		testutil.Must(t, err)
		cmd := exec.Command("/usr/local/bin/deploy-backup-integration", "backup-worker", "__lifecycle")
		cmd.Stdin = bytes.NewReader(body)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("production lifecycle %s: %v: %s", r.Operation, err, out)
		}
		var journal workerLifecycleJournal
		testutil.Must(t, safefs.DecodeJSON(out, &journal, safefs.MaxRecordBytes))
		return journal
	}
	if journal := request(workerLifecycleRequest{Operation: "uninstall-prepare"}); journal.Operation != "uninstall" || journal.Stage != "prepared" {
		t.Fatalf("not prepared: %+v", journal)
	}
	if err = exec.Command("systemctl", "--user", "is-active", "--quiet", "deploy-backup.timer").Run(); err == nil {
		t.Fatal("uninstall preparation left timer active")
	}
	finish := workerLifecycleRequest{Operation: "uninstall-finish"}
	for _, source := range c.Sources {
		finish.Results = append(finish.Results, deploysource.SourceEnrollmentResult{SourceID: source.SourceID, WorkerID: c.WorkerID})
	}
	if journal := request(finish); journal.Stage != "complete" {
		t.Fatal("uninstall did not complete")
	}
	if journal := request(workerLifecycleRequest{Operation: "uninstall-prepare"}); journal.Stage != "complete" {
		t.Fatal("uninstall retry was not idempotent")
	}
	for name := range setup.OwnedFiles {
		if _, err = os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("owned file remains: %s: %v", name, err)
		}
	}
	a, err := openBackupArchive(filepath.Join(home, "archive"))
	testutil.Must(t, err)
	a.Close()
	t.Log("production lifecycle completed uninstall after a completed rotation journal, stopped timers, removed owned files, preserved the archive, and accepted an idempotent retry (fixture source acknowledgements)")
}

// Use the harness's disposable SSH daemon and credentials to prove a rejected
// candidate does not change the running installation's ability to connect.
func checkStagedSSHTrust(t *testing.T, home string, root, stateRoot *safefs.Directory, journal *WorkerSetupJournal) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(home, ".ssh/deploy-known-hosts"))
	testutil.Must(t, err)
	relative := filepath.Join(workerTrustDirectory, "ssh-proof.known_hosts")
	live := filepath.Join(home, relative)
	testutil.Must(t, os.WriteFile(live, body, 0600))
	journal.OwnedFiles[relative] = fmtDigest(body)
	testutil.Must(t, stateRoot.WriteJSON("setup.json", journal, true))
	connect := func(known string) error {
		cmd := exec.Command("ssh", "-F", "/dev/null", "-T", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "StrictHostKeyChecking=yes", "-o", "GlobalKnownHostsFile=/dev/null", "-o", "UserKnownHostsFile="+known, "-i", filepath.Join(home, ".ssh/deploy-integration"), "podman@127.0.0.1", "true")
		return process.Run(cmd, 10*time.Second, time.Second)
	}
	testutil.Must(t, connect(live))
	testutil.Must(t, stageWorkerTrust(home, root, stateRoot, journal, "ssh-proof", []byte("# deliberately untrusted candidate\n")))
	if err = connect(filepath.Join(home, stagedWorkerTrustPath(journal.ID, "ssh-proof"))); err == nil {
		t.Fatal("SSH accepted the untrusted candidate")
	}
	testutil.Must(t, connect(live))
	after, err := os.ReadFile(live)
	testutil.Must(t, err)
	if !bytesEqual(body, after) {
		t.Fatal("candidate qualification changed live SSH trust")
	}
	t.Log("real SSH rejected candidate trust while the prior live connection remained usable")
}
