//go:build linux

package backup

import (
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

type remigrationCrashInput struct {
	Config   WorkerInstallation
	Spec     backupRemigrationSpec
	Manifest install.ManagedManifest
	Runtime  worker.Config
}

func TestBackupRemigrationCrashChild(t *testing.T) {
	home := os.Getenv("DEPLOY_REMIGRATION_HOME")
	if home == "" {
		t.Skip("subprocess fixture")
	}
	d, err := safefs.Open(home)
	testutil.Must(t, err)
	defer d.Close()
	var input remigrationCrashInput
	testutil.Must(t, d.ReadJSON("remigration-crash.json", &input))
	gateway, err := worker.Open(input.Manifest.Paths.State, input.Runtime, false)
	testutil.Must(t, err)
	defer gateway.Close()
	session := &cutoverFixtureSession{m: input.Manifest, store: gateway, p: input.Spec.Cutover.Managed.Producer, g: input.Spec.Cutover.Managed.Generation}
	deps := backupCutoverDependencies{Services: &cutoverFixtureServices{}, Managed: func(ctx context.Context, r install.ManagedProducerRequest, fn func(install.ManagedProducerSession) error) error {
		return fn(session)
	}, Fault: func(at string) error {
		if at == os.Getenv("DEPLOY_REMIGRATION_BOUNDARY") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	err = runBackupRemigration(context.Background(), home, input.Config, input.Spec, false, deps)
	t.Fatal("crash boundary not reached", err)
}

func TestBackupRemigrationSIGKILL(t *testing.T) {
	for _, point := range []string{"remigration-intent", "remigration-sql", "remigration-staged", "remigration-active", "remigration-file", "remigration-shared"} {
		t.Run(point, func(t *testing.T) {
			home, c, spec, deps, session, _ := remigrationFixture(t, "accepted")
			testutil.Must(t, safefs.WritePrivateJSON(filepath.Join(home, "remigration-crash.json"), remigrationCrashInput{c, spec, session.m, session.config}))
			testutil.Must(t, session.store.Close())
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupRemigrationCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_REMIGRATION_HOME="+home, "DEPLOY_REMIGRATION_BOUNDARY="+point)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("child did not die at boundary", err, string(out))
			}
			session.store, err = worker.Open(session.m.Paths.State, session.config, false)
			testutil.Must(t, err)
			defer session.store.Close()
			testutil.Must(t, runBackupRemigration(context.Background(), home, c, spec, false, deps))
			testutil.Must(t, runBackupRemigration(context.Background(), home, c, spec, true, deps))
		})
	}
}
