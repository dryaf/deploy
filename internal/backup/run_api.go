package backup

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type RunRequestResult struct {
	State string `json:"state"`
}

type manualBackupDependencies struct {
	stopped func(context.Context, string, backupActivityUnit) (bool, error)
	start   func(context.Context) error
}

// RequestDeclaredBackup verifies the exact installed declaration and loaded
// Quadlet owner before asking systemd to queue a manual run. It never enables
// timers, manufactures a successful backup, or waits for capture to complete.
func RequestDeclaredBackup(ctx context.Context, home, desired, helper string, targets ...SetupSourceTarget) (RunRequestResult, error) {
	return requestDeclaredBackup(ctx, home, desired, helper, manualBackupDependencies{
		stopped: backupActivityUnitStopped,
		start: func(ctx context.Context) error {
			cmd := exec.Command("systemctl", "--user", "start", "--no-block", "deploy-backup.service")
			cmd.Stdout, cmd.Stderr = &process.Buffer{Limit: 8192}, &process.Buffer{Limit: 8192}
			if err := process.RunContext(ctx, cmd, 15*time.Second, time.Second); err != nil {
				return fmt.Errorf("manual backup start is unconfirmed; inspect status and logs before requesting another run")
			}
			return nil
		},
	}, targets...)
}

func requestDeclaredBackup(ctx context.Context, home, desired, helper string, deps manualBackupDependencies, targets ...SetupSourceTarget) (out RunRequestResult, err error) {
	err = withDeclaredWorker(ctx, home, desired, helper, func(_ *safefs.Directory, j WorkerSetupJournal, c WorkerInstallation, state *backupState, observed SetupObservation) error {
		if err := requireDeclaredSourceTargets(c, targets); err != nil {
			return err
		}
		if err := requireNoPendingDeclaredActivation(state); err != nil {
			return err
		}
		if observed.Installation != "active" && observed.Installation != "staged-awaiting-qualification" {
			return fmt.Errorf("manual backup requires an enabled, completed installation")
		}
		var mode string
		if err := state.get("mode", &mode); err != nil || mode != "writer" {
			return fmt.Errorf("manual backup requires writer mode; finish recovery before requesting a run")
		}
		paths, err := declarationPaths(home, j.Declaration)
		if err != nil {
			return err
		}
		source := paths.OwnedPath(filepath.Join(paths.QuadletDirectory, "deploy-backup.container"))
		if j.OwnedFiles[source] == "" {
			return fmt.Errorf("manual backup service has no retained Quadlet ownership")
		}
		unit := backupActivityUnit{Name: "deploy-backup.service", Source: source, Generated: true}
		stopped, err := deps.stopped(ctx, home, unit)
		if err != nil {
			return err
		}
		if !stopped {
			out.State = "running_or_busy"
			return nil
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		out.State = "start_unconfirmed"
		if err = deps.start(ctx); err != nil {
			return err
		}
		out.State = "start_requested"
		return nil
	})
	return out, err
}
