//go:build linux

package backup

import (
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

type migrationCycleCrashInput struct {
	Config   WorkerInstallation
	Spec     backupMigrationCycleSpec
	Manifest install.ManagedManifest
	Runtime  worker.Config
}

func TestBackupMigrationCycleCrashChild(t *testing.T) {
	home := os.Getenv("DEPLOY_MIGRATION_CYCLE_HOME")
	if home == "" {
		t.Skip("subprocess fixture")
	}
	d, err := safefs.Open(home)
	testutil.Must(t, err)
	defer d.Close()
	var input migrationCycleCrashInput
	testutil.Must(t, d.ReadJSON("migration-cycle-crash.json", &input))
	gateway, err := worker.Open(input.Manifest.Paths.State, input.Runtime, false)
	testutil.Must(t, err)
	defer gateway.Close()
	session := &cutoverFixtureSession{m: input.Manifest, store: gateway, p: input.Spec.Managed.Producer, g: input.Spec.Managed.Generation}
	deps := backupCutoverDependencies{Services: &cutoverFixtureServices{}, Managed: func(ctx context.Context, r install.ManagedProducerRequest, fn func(install.ManagedProducerSession) error) error {
		return fn(session)
	}, Fault: func(at string) error {
		if at == os.Getenv("DEPLOY_MIGRATION_CYCLE_BOUNDARY") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	_, err = runBackupMigrationCycle(context.Background(), home, input.Config, input.Spec, deps)
	t.Fatal("crash boundary not reached", err)
}

func TestBackupMigrationCycleSIGKILL(t *testing.T) {
	for _, point := range []string{"cycle-captured", "cycle-intent", "cycle-revoked", "cycle-archive", "cycle-transaction", "cycle-committed"} {
		t.Run(point, func(t *testing.T) {
			home, c, previous, deps, session, _ := remigrationFixture(t, "accepted")
			testutil.Must(t, runBackupRemigration(context.Background(), home, c, previous, false, deps))
			state, err := openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			var owner backupNotificationOwner
			testutil.Must(t, state.get("notification_owner", &owner))
			testutil.Must(t, state.Close())
			spec := backupMigrationCycleSpec{Version: 1, Managed: previous.Cutover.Managed, ConfigHash: notify.Hash(c), HelperHash: previous.Cutover.HelperHash, OwnerHash: notify.Hash(owner), Reason: "synthetic process death"}
			testutil.Must(t, safefs.WritePrivateJSON(filepath.Join(home, "migration-cycle-crash.json"), migrationCycleCrashInput{c, spec, session.m, session.config}))
			testutil.Must(t, session.store.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupMigrationCycleCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_MIGRATION_CYCLE_HOME="+home, "DEPLOY_MIGRATION_CYCLE_BOUNDARY="+point)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("child did not die at boundary", err, string(out))
			}
			session.store, err = worker.Open(session.m.Paths.State, session.config, false)
			testutil.Must(t, err)
			defer session.store.Close()
			_, err = runBackupMigrationCycle(context.Background(), home, c, spec, deps)
			testutil.Must(t, err)
			_, err = runBackupMigrationCycle(context.Background(), home, c, spec, deps)
			testutil.Must(t, err)
		})
	}
}
