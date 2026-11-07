//go:build linux

package backup

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"encoding/json"
	"github.com/dryaf/deploy/internal/backup/helper"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

const recoveryFixtureConfig = ".config/deploy/backup-worker/runtime.json"

const recoveryFixtureTrust = ".config/deploy/backup-worker/ssh/source.known_hosts"

const recoveryFixtureHelper = "#!/bin/sh\nexit 0\n"

func setupRecoveryFixture(t *testing.T, home string) {
	t.Helper()
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	for _, path := range []string{".local/state/deploy/locks", ".local/state/deploy/backup-worker", ".config/deploy/backup-worker", workerTrustDirectory, ".config/systemd/user", "archive"} {
		testutil.Must(t, root.Mkdir(path))
	}
	coordinator, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, true)
	testutil.Must(t, err)
	coordinator.Close()
	a, err := enrollBackupArchive(filepath.Join(home, "archive"), testutil.ID(t, "a"), testutil.ID(t, "w"))
	testutil.Must(t, err)
	defer a.Close()
	state, err := openBackupState(filepath.Join(home, ".local/state/deploy/backup-worker/state"), true)
	testutil.Must(t, err)
	defer state.Close()
	control := ArchiveControl{Schema: 1, ArchiveID: a.identity.ID, WriterID: a.identity.WriterID, Epoch: a.identity.Epoch, Revision: 1, ConfigHash: strings.Repeat("a", 64), Instances: map[string]ExpectedInstance{}, RetiredSources: map[string]string{}, Pins: map[string]string{}}
	testutil.Must(t, state.put("control", control))
	testutil.Must(t, state.put("mode", "writer"))
	testutil.Must(t, state.put("installation", "active"))
	testutil.Must(t, state.put("min_free_bytes", int64(123)))
	testutil.Must(t, a.commitControl(state))
	testutil.Must(t, os.WriteFile(filepath.Join(home, recoveryFixtureConfig), []byte("old configuration"), 0600))
	testutil.Must(t, os.WriteFile(filepath.Join(home, recoveryFixtureTrust), []byte("old host key"), 0600))
	digest := fmtDigest([]byte(recoveryFixtureHelper))
	release := ".local/lib/deploy/backup-worker/releases/" + digest
	testutil.Must(t, root.Mkdir(release))
	for _, path := range []string{release + "/deploy", ".local/lib/deploy/backup-worker/deploy"} {
		testutil.Must(t, os.WriteFile(filepath.Join(home, path), []byte(recoveryFixtureHelper), 0700))
	}
	testutil.Must(t, root.WriteJSON(".local/lib/deploy/backup-worker/artifact.json", helper.HelperArtifactManifest{Schema: 1, Role: "backup-worker", Current: digest}, false))
	journal := WorkerSetupJournal{Schema: 1, ID: testutil.ID(t, "n"), WorkerID: a.identity.WriterID, ArchiveID: a.identity.ID, ArchivePath: a.dir.File().Name(), Stage: "prepared", RequestHash: "new-request", PreviousRequestHash: "old-request", Updating: true, Files: map[string]setupFileChange{}, OwnedFiles: map[string]string{recoveryFixtureConfig: fmtDigest([]byte("old configuration")), recoveryFixtureTrust: fmtDigest([]byte("old host key"))}}
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", journal, false))
}

// Run in a real child process so SIGKILL, rather than Go cleanup, releases the
// coordinator, schema, writer, and archive leases at the interruption boundary.
func TestBackupSetupInterruptedChild(t *testing.T) {
	home := os.Getenv("DEPLOY_BACKUP_KILL_FIXTURE")
	if home == "" {
		t.Skip("subprocess helper")
	}
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	coordinator, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	testutil.Must(t, err)
	defer coordinator.Close()
	stateRoot, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	defer stateRoot.Close()
	var journal WorkerSetupJournal
	testutil.Must(t, stateRoot.ReadJSON("setup.json", &journal))
	layout, err := declarationPaths(home, journal.Declaration)
	testutil.Must(t, err)
	state, err := openBackupStateWithLease(layout.State, false, false, true)
	testutil.Must(t, err)
	defer state.Close()
	a, err := openBackupArchive(journal.ArchivePath)
	testutil.Must(t, err)
	defer a.Close()
	testutil.Must(t, stageWorkerTrust(home, root, stateRoot, &journal, "source", []byte("new host key")))
	testutil.Must(t, prepareWorkerRecovery(root, stateRoot, &journal, state, a))
	for _, unit := range journal.EnabledUnits {
		_, err = process.LimitedOutput(exec.Command("systemctl", "--user", "disable", unit), 10*time.Second, 4096)
		testutil.Must(t, err)
	}
	testutil.Must(t, stopWorkerUnitsFor(journal.Shared != nil))
	release, err := lockWorkerInstallation(state, a)
	testutil.Must(t, err)
	defer release()
	testutil.Must(t, installWorkerTrust(root, stateRoot, &journal, []SetupSource{{Alias: "source", KnownHosts: "new host key"}}))
	testutil.Must(t, installSetupFile(root, stateRoot, &journal, layout.OwnedPath(layout.Runtime), []byte("new configuration")))
	testutil.Must(t, a.updateControl(state, func(c *ArchiveControl) error { c.ConfigHash = strings.Repeat("b", 64); return nil }))
	testutil.Must(t, state.put("min_free_bytes", int64(999)))
	testutil.Must(t, state.put("installation", "staged"))
	testutil.Must(t, helper.InstallSelfAt(home, "backup-worker", layout.Installation))
	fmt.Fprintln(os.Stdout, "READY")
	for {
		time.Sleep(time.Second)
	}
}

func startInterruptedSetup(t *testing.T, home string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestBackupSetupInterruptedChild$")
	cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_KILL_FIXTURE="+home)
	pipe, err := cmd.StdoutPipe()
	testutil.Must(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	testutil.Must(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	scanner := bufio.NewScanner(pipe)
	if !scanner.Scan() || scanner.Text() != "READY" {
		output := scanner.Text()
		for scanner.Scan() {
			output += "\n" + scanner.Text()
		}
		cmd.Wait()
		t.Fatalf("child did not reach interruption boundary: %s %s %v", output, stderr.String(), scanner.Err())
	}
	return cmd
}

func assertSetupRecovered(t *testing.T, home string) {
	t.Helper()
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var journal WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &journal))
	if journal.RecoveryNeeded || journal.Updating || journal.Stage != "complete" || journal.RequestHash != "old-request" {
		t.Fatalf("incomplete recovery: %+v", journal)
	}
	layout, err := declarationPaths(home, journal.Declaration)
	testutil.Must(t, err)
	trust, err := os.ReadFile(filepath.Join(layout.Keys(), "source.known_hosts"))
	testutil.Must(t, err)
	if string(trust) != "old host key" {
		t.Fatal("source trust not restored")
	}
	body, err := os.ReadFile(layout.Runtime)
	testutil.Must(t, err)
	if string(body) != "old configuration" {
		t.Fatal("configuration not restored")
	}
	body, err = os.ReadFile(layout.Helper())
	testutil.Must(t, err)
	if string(body) != recoveryFixtureHelper {
		t.Fatal("helper not restored")
	}
	state, err := openBackupState(layout.State, false)
	testutil.Must(t, err)
	defer state.Close()
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	if control.ConfigHash != strings.Repeat("a", 64) {
		t.Fatal("control not restored")
	}
	var minimum int64
	testutil.Must(t, state.get("min_free_bytes", &minimum))
	if minimum != 123 {
		t.Fatal("storage policy not restored")
	}
	var installation string
	testutil.Must(t, state.get("installation", &installation))
	if installation != "active" {
		t.Fatal("installation state not restored")
	}
}

func TestBackupSetupSIGKILLRecovery(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(fmt.Sprint("custom=", custom), func(t *testing.T) { testBackupSetupSIGKILLRecovery(t, custom) })
	}
}

func testBackupSetupSIGKILLRecovery(t *testing.T, custom bool) {
	home := t.TempDir()
	setupRecoveryFixture(t, home)
	// Carry declarative authority through the real interrupted update, including
	// a failed service restart before rollback completion can be acknowledged.
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	var before WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &before))
	before.PreviousDeclaration = &SetupDeclaration{Version: 1, DesiredHash: strings.Repeat("c", 64), Home: home}
	before.Declaration = &SetupDeclaration{Version: 1, DesiredHash: strings.Repeat("d", 64), Home: home}
	if custom {
		base := t.TempDir()
		overrides := config.WorkerPaths{Config: filepath.Join(base, "config"), State: filepath.Join(base, "state"), Installation: filepath.Join(base, "artifacts"), UnitDirectory: filepath.Join(base, "units")}
		p, e := ResolveInstallationPaths(home, "", "", "", overrides)
		testutil.Must(t, e)
		for old, next := range map[string]string{filepath.Join(home, ".config/deploy/backup-worker"): p.Config, filepath.Join(home, ".local/state/deploy/backup-worker/state"): p.State, filepath.Join(home, ".local/lib/deploy/backup-worker"): p.Installation, filepath.Join(home, ".config/systemd/user"): p.UnitDirectory} {
			testutil.Must(t, os.Rename(old, next))
		}
		before.OwnedFiles = map[string]string{p.Runtime: fmtDigest([]byte("old configuration")), filepath.Join(p.Keys(), "source.known_hosts"): fmtDigest([]byte("old host key"))}
		for i, d := range []*SetupDeclaration{before.PreviousDeclaration, before.Declaration} {
			d.Version, d.Paths = 2, &p
			d.Desired = &config.DesiredWorkers{Backup: &config.BackupDeclaration{Paths: overrides, Schedule: []string{"daily", "hourly"}[i]}}
			d.DesiredHash, e = d.Desired.Fingerprint()
			testutil.Must(t, e)
		}
	}
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", before, true))
	root.Close()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$BACKUP_RECOVERY_CALLS"
case "$*" in
 *is-enabled*deploy-backup.timer) echo enabled;;
 *is-enabled*) exit 1;;
 *show*) echo loaded;;
 *enable*--now*) test "${BACKUP_RECOVERY_FAIL_START:-}" != 1;;
 *) exit 0;;
esac
`
	testutil.Must(t, os.WriteFile(filepath.Join(bin, "systemctl"), []byte(script), 0700))
	if custom {
		p := before.Declaration.Paths
		unitJSON, e := json.Marshal(map[string]any{"type": "as", "data": []string{p.UnitDirectory}})
		testutil.Must(t, e)
		envJSON, e := json.Marshal(map[string]any{"type": "as", "data": []string{"XDG_CONFIG_HOME=" + p.ConfigHome}})
		testutil.Must(t, e)
		script := "#!/bin/sh\ncase \"$*\" in\n *UnitPath) cat \"$BACKUP_UNIT_FACTS\";;\n *Environment) cat \"$BACKUP_ENV_FACTS\";;\n *) exit 1;;\nesac\n"
		testutil.Must(t, os.WriteFile(filepath.Join(bin, "busctl"), []byte(script), 0700))
		testutil.Must(t, os.WriteFile(filepath.Join(bin, "units.json"), unitJSON, 0600))
		testutil.Must(t, os.WriteFile(filepath.Join(bin, "env.json"), envJSON, 0600))
		t.Setenv("BACKUP_UNIT_FACTS", filepath.Join(bin, "units.json"))
		t.Setenv("BACKUP_ENV_FACTS", filepath.Join(bin, "env.json"))
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("BACKUP_RECOVERY_CALLS", log)
	cmd := startInterruptedSetup(t, home)
	if state, err := openInstalledBackupState(home, false); err == nil {
		state.Close()
		t.Fatal("state admitted during installation")
	}
	testutil.Must(t, recoverWorkerSetup(home)) // Active coordinator owns recovery.
	testutil.Must(t, cmd.Process.Kill())
	if err := cmd.Wait(); err == nil {
		t.Fatal("child was not killed")
	}
	// Restoring files is not completion: a failed timer restart must remain retryable.
	t.Setenv("BACKUP_RECOVERY_FAIL_START", "1")
	if err := recoverWorkerSetup(home); err == nil {
		t.Fatal("failed restart acknowledged as recovered")
	}
	t.Setenv("BACKUP_RECOVERY_FAIL_START", "0")
	testutil.Must(t, recoverWorkerSetup(home))
	assertSetupRecovered(t, home)
	root, err = safefs.Open(home)
	testutil.Must(t, err)
	var restored WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &restored))
	root.Close()
	if restored.Declaration == nil || restored.Declaration.DesiredHash != before.PreviousDeclaration.DesiredHash || restored.PreviousDeclaration != nil {
		t.Fatal("rollback lost or replaced YAML authority")
	}
	calls, err := os.ReadFile(log)
	testutil.Must(t, err)
	if !strings.Contains(string(calls), "enable --now deploy-backup.timer") {
		t.Fatal("previous timer was not restored")
	}
	testutil.Must(t, recoverWorkerSetup(home)) // Repeated boot/timer recovery is harmless.
}

func TestBackupSetupSchemaLeaseProtectsReaders(t *testing.T) {
	a, state, _, _, _ := archiveFixture(t)
	installer, err := openBackupStateWithLease(state.directory, false, false, true)
	testutil.Must(t, err)
	defer installer.Close()
	if release, err := lockWorkerInstallation(installer, a); err == nil {
		release()
		t.Fatal("installation ignored live state reader")
	}
	testutil.Must(t, state.Close())
	release, err := lockWorkerInstallation(installer, a)
	testutil.Must(t, err)
	if reader, err := openBackupState(state.directory, false); err == nil {
		reader.Close()
		t.Fatal("reader entered installation")
	}
	release()
	reader, err := openBackupState(state.directory, false)
	testutil.Must(t, err)
	reader.Close()
}
