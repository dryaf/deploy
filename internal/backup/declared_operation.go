package backup

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

// The setup lease spans authority validation and mutation. A concurrent apply,
// key rotation, removal or reporting cutover cannot retarget an operation.
func withDeclaredWorker(ctx context.Context, home, desired, helper string, fn func(*safefs.Directory, WorkerSetupJournal, WorkerInstallation, *backupState, SetupObservation) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !backupRecoveryPath(home) || !safefs.DigestPattern.MatchString(desired) || !safefs.DigestPattern.MatchString(helper) {
		return fmt.Errorf("declared backup operation requires exact target and artifact pins")
	}
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	observed, err := inspectSetupLocked(ctx, root, home)
	if err != nil {
		return err
	}
	if observed.Stage == "source_replacement_pending" {
		return ErrSourceReplacementPending
	}
	if observed.Stage == "source_retirement_pending" {
		return ErrSourceRetirementPending
	}
	if observed.Stage != "complete" || observed.DesiredHash != desired {
		return fmt.Errorf("finish applying the unchanged declaration before this operation")
	}
	var j WorkerSetupJournal
	if err = root.ReadJSON(".local/state/deploy/backup-worker/setup.json", &j); err != nil {
		return err
	}
	if j.Declaration == nil || j.Declaration.HelperSHA256 != helper {
		return fmt.Errorf("backup helper differs from the completed installation")
	}
	var lifecycle workerLifecycleJournal
	if e := root.ReadJSON(".local/state/deploy/backup-worker/lifecycle.json", &lifecycle); e == nil && lifecycle.Stage != "complete" {
		return fmt.Errorf("finish the pending backup lifecycle before this operation")
	} else if e != nil && !os.IsNotExist(e) {
		return e
	}
	var c WorkerInstallation
	if err = readInstalledWorkerJSON(root, &c); err != nil {
		return err
	}
	if err = config.PinnedWorkerImage(c.Image); err != nil {
		return fmt.Errorf("installed worker image pin is unavailable")
	}
	state, err := openInstalledBackupState(home, false)
	if err != nil {
		return err
	}
	defer state.Close()
	if err = requireBackupActivityReady(state); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return fn(root, j, c, state, observed)
}
