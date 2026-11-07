package backup

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

// Command admission does not change the report owner or credential exclusions.
// This read-only continuation requires the original cutover to be complete;
// missing completion can never be interpreted as permission to repeat it.
func inspectFreshCommandContinuation(ctx context.Context, home string, c WorkerInstallation, retained backupCutoverIntent, currentHash string, inspect func(context.Context, string, string, string) (install.ManagedManifest, error)) error {
	if inspect == nil {
		return fmt.Errorf("command delivery continuation inspection unavailable")
	}
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	if err != nil {
		return err
	}
	defer state.Close()
	var complete string
	if err = state.get("notification_cutover_complete", &complete); err != nil || complete != notify.Hash(retained) {
		return fmt.Errorf("command continuation requires the retained completed reporting cutover")
	}
	owner, err := currentSharedBackupOwner(state)
	if err != nil {
		return err
	}
	spec := retained.Spec
	if owner == nil || owner.CutoverHash != complete || owner.Binding.Producer != spec.Managed.Producer || owner.Binding.Generation != spec.Managed.Generation || owner.Binding.Route != spec.Managed.Route || owner.Binding.Archive != c.ArchiveID {
		return fmt.Errorf("command continuation lost reporting ownership")
	}
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return err
	}
	if control.ArchiveID != c.ArchiveID || control.WriterID != c.WorkerID || control.ConfigHash != c.ConfigHash || !control.SharedRequired || control.ReportingDisabled {
		return fmt.Errorf("command continuation changed backup authority")
	}
	m, err := inspect(ctx, spec.Managed.Root, currentHash, spec.Managed.ManifestHash)
	if err != nil {
		return err
	}
	if notify.Hash(m) != currentHash || m.Root != spec.Managed.Root || m.Identity != owner.Binding.Identity || m.Mode != "delivery" {
		return fmt.Errorf("command continuation returned different gateway authority")
	}
	return nil
}
