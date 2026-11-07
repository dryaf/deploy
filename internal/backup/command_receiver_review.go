package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type commandReceiverReviewDependencies struct {
	reporting backupCutoverDependencies
	policy    func(context.Context, string, string, notify.Producer) (interaction.Config, error)
	helper    func(context.Context, string, WorkerInstallation, string, bool) error
}

// ReviewSharedCommandReceiver derives an initial receiver from completed backup
// reporting and the exact enrolled gateway policy. It only inspects: the caller
// must retain the returned request before staging it and admit commands separately.
func ReviewSharedCommandReceiver(ctx context.Context, home, desiredHash, manifestHash, operation string) (CommandReceiverRequest, error) {
	return reviewSharedCommandReceiver(ctx, home, desiredHash, manifestHash, operation, commandReceiverReviewDependencies{
		reporting: backupCutoverDependencies{Completed: verifyFreshCompletedReporting, CommandContinuation: install.InspectCommandDeliveryContinuation},
		policy:    install.InspectManagedCommandPolicy, helper: inspectCommandReceiverHelper,
	})
}

func reviewSharedCommandReceiver(ctx context.Context, home, desiredHash, manifestHash, operation string, deps commandReceiverReviewDependencies) (CommandReceiverRequest, error) {
	var zero CommandReceiverRequest
	if !safefs.ValidID(operation, "n") || deps.policy == nil || deps.helper == nil {
		return zero, fmt.Errorf("command receiver review requires a retained operation and inspectors")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	c, spec, err := prepareFreshSharedReportingUsing(ctx, home, desiredHash, manifestHash, deps.reporting.CommandContinuation)
	if err != nil {
		return zero, err
	}
	complete, err := inspectFreshReportingCompletion(ctx, home, c, spec, deps.reporting)
	if err != nil {
		return zero, err
	}
	if !complete {
		return zero, fmt.Errorf("connect shared reporting before reviewing commands")
	}
	policy, err := deps.policy(ctx, spec.Managed.Root, manifestHash, spec.Managed.Producer)
	if err != nil {
		return zero, err
	}
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	if err != nil {
		return zero, err
	}
	defer state.Close()
	var control ArchiveControl
	if err = state.get("control", &control); err != nil {
		return zero, err
	}
	owner, err := currentSharedBackupOwner(state)
	if err != nil {
		return zero, err
	}
	if owner == nil || owner.Binding.Producer != spec.Managed.Producer || owner.Binding.Generation != spec.Managed.Generation || owner.Binding.Route != spec.Managed.Route || owner.Binding.Archive != c.ArchiveID || control.ConfigHash != c.ConfigHash || !control.SharedRequired {
		return zero, fmt.Errorf("command receiver review changed completed reporting authority")
	}
	r := CommandReceiverRequest{Version: 1, Operation: operation, State: state.directory, Archive: c.ArchiveID, WriterEpoch: control.Epoch, OwnerHash: notify.Hash(*owner), Producer: spec.Managed.Producer, Identity: owner.Binding.Identity, Socket: owner.Socket, Policy: policy}
	for _, w := range policy.Workers {
		if w.Producer == r.Producer {
			for _, descriptor := range w.Commands {
				if descriptor.Effect != "read" {
					r.Version = 2
				}
			}
		}
	}
	if err = r.validate(); err != nil {
		return zero, err
	}
	if err = deps.helper(ctx, home, c, spec.HelperHash, r.mutations()); err != nil {
		return zero, err
	}
	if err = requireCommandReceiverOwner(state, r); err != nil {
		return zero, err
	}
	return r, ctx.Err()
}

func inspectCommandReceiverHelper(ctx context.Context, home string, c WorkerInstallation, expected string, mutations bool) error {
	root, err := safefs.Open(home)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := openBackupOwnedFile(root, backupCutoverPaths(home, c).helperFile(), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = inspectCommandReceiverArtifact(ctx, f, expected, mutations)
	return err
}

func inspectCommandReceiverArtifact(ctx context.Context, f *os.File, expected string, mutations bool) (string, error) {
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Mode().Perm()&0100 == 0 || info.Size() <= 0 || info.Size() > 256<<20 {
		return "", fmt.Errorf("installed command receiver helper is unsafe")
	}
	digest, err := safefs.DigestFile(f)
	if err != nil || digest != expected {
		return "", fmt.Errorf("installed command receiver helper changed")
	}
	cmd := exec.CommandContext(ctx, "/proc/self/fd/3", "backup-worker", "__shared-commands-probe")
	cmd.ExtraFiles = []*os.File{f}
	out, err := process.LimitedOutput(cmd, 15*time.Second, 1024)
	capability := strings.TrimSpace(string(out))
	if err != nil || capability != "backup-shared-commands-v2" && (mutations || capability != "backup-shared-commands-v1") {
		return "", fmt.Errorf("installed backup helper lacks scheduled command support; upgrade the matching helper before admission")
	}
	return capability, nil
}
