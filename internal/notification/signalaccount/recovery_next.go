package signalaccount

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// The installed service must understand later-epoch fencing. The caller first
// retires/stops that service; neither this operation nor a retry starts it.
type NextRecoveryService interface {
	RecoveryService
	VerifyNextRecoverySupport(context.Context) error
}

type nextRecoveryIntent struct {
	Version int
	Before  recoveryRecords
	After   recoveryIntent
}

func nextRecoveryName(parent RecoveryStatus) string {
	return "deploy-recovery-next-" + protocol.Hash(parent) + ".json"
}
func ancestorRecoveryName(hash string) string { return "deploy-recovery-history/" + hash + ".json" }

func FenceNextRecovery(ctx context.Context, path string, parent RecoveryStatus, req RecoveryRequest, svc NextRecoveryService, apply RecoveryApply, verify RecoveryVerify) (RecoveryStatus, error) {
	return (recoveryCoordinator{}).runNext(ctx, path, parent, req, svc, apply, verify)
}

func (c recoveryCoordinator) runNext(ctx context.Context, path string, parent RecoveryStatus, req RecoveryRequest, svc NextRecoveryService, apply RecoveryApply, verify RecoveryVerify) (RecoveryStatus, error) {
	if req.validate() != nil || parent.Phase != "complete" || parent.RuntimeAllowed || !safefs.DigestPattern.MatchString(parent.ResultHash) || parent.Operation == req.Operation || svc == nil || apply == nil || verify == nil {
		return RecoveryStatus{}, fmt.Errorf("next Signal recovery requires exact completed parent and service")
	}
	if err := svc.VerifyNextRecoverySupport(ctx); err != nil {
		return RecoveryStatus{}, err
	}
	inspect := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := svc.Inspect(ctx)
		if err != nil {
			return err
		}
		if state.Active || state.Binding != req.Binding {
			return fmt.Errorf("stop the pinned Signal service before next recovery")
		}
		return nil
	}
	if err := inspect(); err != nil {
		return RecoveryStatus{}, err
	}
	if err := c.prepareNext(ctx, path, parent, req, inspect); err != nil {
		return RecoveryStatus{}, err
	}
	return c.runExpectedParent(ctx, path, req, svc, apply, verify, &parent)
}

func (c recoveryCoordinator) prepareNext(ctx context.Context, path string, parent RecoveryStatus, req RecoveryRequest, inspect func() error) error {
	d, err := accountDirectory(path)
	if err != nil {
		return err
	}
	defer d.Close()
	op, err := d.Lock("deploy-service-link.lock", true, false)
	if err != nil {
		return err
	}
	defer op.Close()
	account, err := d.Lock("deploy-account.lock", true, false)
	if err != nil {
		return err
	}
	defer account.Close()
	// No service/manager calls occur under the short control lease.
	if err = inspect(); err != nil {
		return err
	}
	guard, err := d.Lock("deploy-account-control.lock", true, false)
	if err != nil {
		return err
	}
	defer guard.Close()
	actual, err := accountDirectory(path)
	if err != nil {
		return err
	}
	err = sameAccountDirectory(d, actual)
	actual.Close()
	if err != nil {
		return err
	}
	for name, lease := range map[string]*safefs.Lock{"deploy-service-link.lock": op, "deploy-account.lock": account, "deploy-account-control.lock": guard} {
		if err = sameAccountLease(d, name, lease); err != nil {
			return err
		}
	}
	var intent nextRecoveryIntent
	exists, err := readRecord(d, nextRecoveryName(parent), &intent)
	if err != nil {
		return err
	}
	if !exists {
		before, err := readRecovery(d)
		if err != nil {
			return err
		}
		if before.status() != parent || !before.Complete {
			return fmt.Errorf("Signal recovery parent changed")
		}
		after := before.Intent
		after.Version, after.Request, after.Ancestor = 2, req, protocol.Hash(before)
		intent = nextRecoveryIntent{1, before, after}
		if err = d.Mkdir("deploy-recovery-history"); err != nil {
			return err
		}
		var retained recoveryRecords
		found, err := readRecord(d, ancestorRecoveryName(after.Ancestor), &retained)
		if err != nil {
			return err
		}
		if found && retained != before {
			return fmt.Errorf("Signal ancestor archive changed")
		}
		if !found {
			if err = writeRecord(d, ancestorRecoveryName(after.Ancestor), before, false); err != nil {
				return err
			}
		}
		if err = writeRecord(d, nextRecoveryName(parent), intent, false); err != nil {
			return err
		}
		if err = c.boundary("next_recovery_intent"); err != nil {
			return err
		}
	}
	if intent.Version != 1 || intent.Before.status() != parent || intent.After.Request != req || intent.After.Version != 2 || intent.After.Ancestor != protocol.Hash(intent.Before) {
		return fmt.Errorf("next Signal recovery intent changed")
	}
	if err = verifyRecoveryAncestors(d, intent.After); err != nil {
		return err
	}
	var current recoveryIntent
	found, err := readRecord(d, recoveryIntentFile, &current)
	if err != nil {
		return err
	}
	if !found || current != intent.Before.Intent && current != intent.After {
		return fmt.Errorf("Signal recovery root changed")
	}
	if current == intent.After {
		// Once the new fence exists, the ordinary coordinator owns every later
		// result. Never remove new completion records on a preparation retry.
		var fence recoveryIntent
		present, e := readRecord(d, recoveryFenceFile, &fence)
		if e != nil {
			return e
		}
		if present && fence == intent.After {
			_, e = readRecovery(d)
			return e
		}
	} else {
		if err = writeRecord(d, recoveryIntentFile, intent.After, true); err != nil {
			return err
		}
	}
	if err = c.boundary("next_recovery_root"); err != nil {
		return err
	}
	for _, name := range []string{recoveryCompleteFile, recoveryResultFile, recoveryFenceFile} {
		if err = ctx.Err(); err != nil {
			return err
		}
		if name == recoveryFenceFile {
			var old recoveryIntent
			found, err = readRecord(d, name, &old)
			if err == nil && found && old != intent.Before.Intent {
				return fmt.Errorf("old Signal fence changed")
			}
		} else {
			var old recoveryResult
			found, err = readRecord(d, name, &old)
			if err == nil && found && old != intent.Before.Result {
				return fmt.Errorf("old Signal result changed")
			}
		}
		if err != nil {
			return err
		}
		if found {
			if err = unix.Unlinkat(int(d.File().Fd()), name, 0); err != nil {
				return err
			}
			if err = d.File().Sync(); err != nil {
				return err
			}
		}
		if err = c.boundary("next_recovery_retire_" + name); err != nil {
			return err
		}
	}
	_, err = readRecovery(d)
	return err
}

func verifyRecoveryAncestors(d *safefs.Directory, current recoveryIntent) error {
	for depth := 0; current.Version == 2; depth++ {
		if depth >= 32 || !safefs.DigestPattern.MatchString(current.Ancestor) {
			return fmt.Errorf("invalid or oversized Signal ancestor chain")
		}
		var prior recoveryRecords
		found, err := readRecord(d, ancestorRecoveryName(current.Ancestor), &prior)
		if err != nil {
			return err
		}
		if !found || protocol.Hash(prior) != current.Ancestor || !prior.HasIntent || !prior.HasFence || !prior.HasResult || !prior.Complete || prior.Intent.Request.validate() != nil || prior.Result.Intent != protocol.Hash(prior.Intent) || !safefs.DigestPattern.MatchString(prior.Result.Result) {
			return fmt.Errorf("Signal ancestor proof lost or changed")
		}
		want := current
		want.Version, want.Request, want.Ancestor = prior.Intent.Version, prior.Intent.Request, prior.Intent.Ancestor
		if want != prior.Intent {
			return fmt.Errorf("Signal ancestor changed account or maintenance history")
		}
		current = prior.Intent
	}
	if current.Version != RecoveryVersion || current.Ancestor != "" {
		return fmt.Errorf("invalid original Signal recovery ancestor")
	}
	return nil
}

func refuseSupersededRecovery(d *safefs.Directory, expected RecoveryStatus) error {
	f, err := d.Open(nextRecoveryName(expected), os.O_RDONLY, 0)
	if err == nil {
		f.Close()
		return fmt.Errorf("Signal account has a pending or completed next recovery")
	}
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// VerifyRecoveryAncestor checks retained lineage without granting an account
// lease or permitting the predecessor service to run.
func VerifyRecoveryAncestor(path string, current, ancestor RecoveryStatus) error {
	return verifyRecoveryAncestor(path, current, ancestor, false)
}

// VerifyRetainedRecoveryAncestor also accepts a completed historical descendant.
// It is evidence inspection only; runtime always requires the current head.
func VerifyRetainedRecoveryAncestor(path string, current, ancestor RecoveryStatus) error {
	return verifyRecoveryAncestor(path, current, ancestor, true)
}

func verifyRecoveryAncestor(path string, current, ancestor RecoveryStatus, retained bool) error {
	d, guard, err := control(path, false)
	if err != nil {
		return err
	}
	defer d.Close()
	defer guard.Close()
	r, err := readRecovery(d)
	if err != nil {
		return err
	}
	if !r.Complete || (!retained && r.status() != current) || current == ancestor {
		return fmt.Errorf("Signal current recovery changed")
	}
	found := r.status() == current
	for depth := 0; depth < 32 && r.Intent.Version == 2; depth++ {
		var prior recoveryRecords
		ok, err := readRecord(d, ancestorRecoveryName(r.Intent.Ancestor), &prior)
		if err != nil {
			return err
		}
		if !ok || protocol.Hash(prior) != r.Intent.Ancestor {
			return fmt.Errorf("Signal ancestor disappeared")
		}
		if found && prior.status() == ancestor {
			return nil
		}
		found = found || prior.status() == current
		r = prior
	}
	return fmt.Errorf("Signal recovery is outside the retained lineage")
}
