//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil"
)

type recoveryActivationCrashInput struct {
	Home, Point string
	Config      WorkerInstallation
	Gateway     worker.Config
	Manifest    install.ManagedManifest
	Spec        backupRecoveryActivationSpec
}

func TestBackupRecoveryActivationCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_BACKUP_RECOVERY_ACTIVATION_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	testutil.Must(t, err)
	var in recoveryActivationCrashInput
	testutil.Must(t, json.Unmarshal(raw, &in))
	r := *in.Manifest.DeliveryRecovery
	s, err := worker.OpenManagedRecoveredDelivery(in.Manifest.Paths.State, in.Gateway, in.Manifest.Revision, r, notify.Hash(r.Owner), notify.Hash("fixture-activation"))
	testutil.Must(t, err)
	defer s.Close()
	session := &cutoverFixtureSession{m: in.Manifest, store: s, p: in.Spec.Managed.Producer, g: in.Spec.Managed.Generation, config: in.Gateway}
	deps := backupCutoverDependencies{Services: &recoveryReportServices{}, Managed: func(ctx context.Context, r install.ManagedProducerRequest, fn func(install.ManagedProducerSession) error) error {
		return fn(recoveryReportSession{session})
	}, Verify: func(context.Context, backupNotificationOwner) error { return nil }, Fault: func(at string) error {
		if at == in.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	err = runBackupRecoveryActivation(context.Background(), in.Home, in.Config, in.Spec, false, deps)
	t.Fatal("crash boundary missed", err)
}

func TestBackupRecoveryActivationSIGKILL(t *testing.T) {
	for _, point := range []string{"recovery-activation-intent", "recovery-activation-sql", "recovery-activation-active", "recovery-activation-file", "recovery-activation-shared"} {
		t.Run(point, func(t *testing.T) {
			f, home, spec, deps, session := backupRecoveryActivationFixture(t)
			testutil.Must(t, session.store.Close())
			input := recoveryActivationCrashInput{home, point, f.config, f.gatewayConfig, session.m, spec}
			path := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(input)
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(path, raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackupRecoveryActivationCrashHelper$", "-test.timeout=90s")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_RECOVERY_ACTIVATION_CRASH="+path)
			out, err := process.LimitedOutput(cmd, 95*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("child did not crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong child exit", cmd.ProcessState, string(out))
			}
			r := *session.m.DeliveryRecovery
			s, err := worker.OpenManagedRecoveredDelivery(f.gatewayPath, f.gatewayConfig, session.m.Revision, r, notify.Hash(r.Owner), notify.Hash("fixture-activation"))
			testutil.Must(t, err)
			defer s.Close()
			session.store = s
			testutil.Must(t, runBackupRecoveryActivation(context.Background(), home, f.config, spec, false, deps))
			testutil.Must(t, runBackupRecoveryActivation(context.Background(), home, f.config, spec, false, deps))
			testutil.Must(t, requireNotificationRecoveryInactiveTx(f.state))
		})
	}
}
