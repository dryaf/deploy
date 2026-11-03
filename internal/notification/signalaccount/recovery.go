package signalaccount

import (
	"context"
	"fmt"
	"os"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// RecoveryService must prove that the installed runtime and its children use
// this recovery gate, not just the older linking gate. Binding inspection must
// also validate the loaded unit, immutable executable/image and account mount.
type RecoveryService interface {
	Service
	VerifyRecoverySupport(context.Context) error
}

// RecoveryApply performs an exact, idempotent offline transition. It must use
// check around each durable/external boundary and return a digest of independently
// retained evidence. Verify rechecks that evidence without applying the transition.
type RecoveryApply func(context.Context, func() error) (string, error)
type RecoveryVerify func(context.Context, string, func() error) error

type recoveryCoordinator struct{ fault func(string) error }

func (c recoveryCoordinator) boundary(at string) error {
	if c.fault != nil {
		return c.fault(at)
	}
	return nil
}

// FenceRecovery permanently fences ordinary account consumers before stopping
// the pinned service. No method here releases that fence or restarts a receiver.
// Exact retries reconcile partial file publication; a retained result is verified
// without calling apply again. The future activation protocol owns fence release.
func FenceRecovery(ctx context.Context, path string, req RecoveryRequest, svc RecoveryService, apply RecoveryApply, verify RecoveryVerify) (RecoveryStatus, error) {
	return (recoveryCoordinator{}).run(ctx, path, req, svc, apply, verify)
}
func (c recoveryCoordinator) run(ctx context.Context, path string, req RecoveryRequest, svc RecoveryService, apply RecoveryApply, verify RecoveryVerify) (out RecoveryStatus, err error) {
	if err = req.validate(); err != nil {
		return
	}
	if svc == nil || apply == nil || verify == nil {
		return out, fmt.Errorf("Signal recovery dependencies missing")
	}
	if err = ctx.Err(); err != nil {
		return
	}
	d, err := accountDirectory(path)
	if err != nil {
		return out, err
	}
	defer d.Close()
	op, err := d.Lock("deploy-service-link.lock", true, true)
	if err != nil {
		return out, err
	}
	defer op.Close()
	// Unlike the short control lock, this serializes the entire service operation
	// against linking maintenance without holding a database transaction.
	if err = svc.VerifyRecoverySupport(ctx); err != nil {
		return out, err
	}
	directoryCheck := func() error {
		q, e := accountDirectory(path)
		if e != nil {
			return e
		}
		defer q.Close()
		if e = sameAccountDirectory(d, q); e != nil {
			return e
		}
		return sameAccountLease(d, "deploy-service-link.lock", op)
	}
	inspect := func(stopped bool) error {
		if e := ctx.Err(); e != nil {
			return e
		}
		if e := directoryCheck(); e != nil {
			return e
		}
		state, e := svc.Inspect(ctx)
		if e != nil {
			return e
		}
		if e = directoryCheck(); e != nil {
			return e
		}
		if state.Binding != req.Binding {
			return fmt.Errorf("Signal recovery service binding changed")
		}
		if stopped && state.Active {
			return fmt.Errorf("Signal receiver restarted during recovery")
		}
		return nil
	}
	if err = inspect(false); err != nil {
		return out, err
	}
	var r recoveryRecords
	func() {
		var q *safefs.Directory
		var guard *safefs.Lock
		q, guard, err = control(path, true)
		if err != nil {
			return
		}
		defer q.Close()
		defer guard.Close()
		if err = directoryCheck(); err != nil {
			return
		}
		r, err = readRecovery(q)
		if err != nil {
			return
		}
		if r.HasIntent {
			if r.Intent.Request != req {
				err = fmt.Errorf("Signal account already has another recovery target")
			}
			return
		}
		var links linkHistory
		var maintenance maintenanceHistory
		links, err = readHistory(q)
		if err != nil {
			return
		}
		maintenance, err = readMaintenance(q)
		if err != nil {
			return
		}
		if err = maintenanceLinkEvidence(maintenance, links); err != nil {
			return
		}
		if !links.runtimeAllowed() || !maintenance.runtimeAllowed() || (maintenance.last() != nil && maintenance.last().Phase != "complete") {
			err = fmt.Errorf("settle Signal linking/service maintenance before recovery")
			return
		}
		r.Intent, err = recoveryIdentity(q)
		if err != nil {
			return
		}
		r.Intent.Version, r.Intent.Request = RecoveryVersion, req
		r.Intent.LinkHash, r.Intent.MaintenanceHash = protocol.Hash(links), protocol.Hash(maintenance)
		err = writeRecord(q, recoveryIntentFile, r.Intent, false)
		if err == nil {
			r.HasIntent = true
			err = c.boundary("recovery_intent")
		}
	}()
	if err != nil {
		return r.status(), err
	}
	// Publish under the short control lease so diagnostics/startup never read a
	// mixture of two phases. No service or callback I/O occurs under this lease.
	publish := func(name string, body any, next recoveryRecords) error {
		q, guard, e := control(path, true)
		if e != nil {
			return e
		}
		defer q.Close()
		defer guard.Close()
		if e = directoryCheck(); e != nil {
			return e
		}
		if e = sameAccountDirectory(d, q); e != nil {
			return e
		}
		actual, e := readRecovery(q)
		if e != nil {
			return e
		}
		if actual != r {
			return fmt.Errorf("Signal recovery authority changed before publication")
		}
		if e = writeRecord(q, name, body, false); e != nil {
			return e
		}
		r = next
		return nil
	}
	// Exact intent alone already fences all ordinary starts; the independent copy
	// makes subsequent intent loss detectable before any service/callback effects.
	if !r.HasFence {
		next := r
		next.HasFence = true
		if err = publish(recoveryFenceFile, r.Intent, next); err != nil {
			return r.status(), err
		}
		if err = c.boundary("recovery_fence"); err != nil {
			return r.status(), err
		}
	}
	if r.Complete {
		if err = inspect(true); err != nil {
			return r.status(), err
		}
	} else {
		if err = inspect(false); err != nil {
			return r.status(), err
		}
		if err = svc.Stop(ctx); err != nil {
			return r.status(), err
		}
		if err = c.boundary("recovery_stop"); err != nil {
			return r.status(), err
		}
	}
	if err = inspect(true); err != nil {
		return r.status(), err
	}
	if err = svc.VerifyRecoverySupport(ctx); err != nil {
		return r.status(), err
	}
	lock, err := d.Lock("deploy-account.lock", true, false)
	if err != nil {
		return r.status(), fmt.Errorf("Signal account/CLI child still busy: %w", err)
	}
	defer lock.Close()
	check := func() error {
		if e := inspect(true); e != nil {
			return e
		}
		q, guard, e := control(path, false)
		if e != nil {
			return e
		}
		defer q.Close()
		defer guard.Close()
		if e = sameAccountDirectory(d, q); e != nil {
			return e
		}
		if e = sameAccountLease(d, "deploy-service-link.lock", op); e != nil {
			return e
		}
		if e = sameAccountLease(d, "deploy-account.lock", lock); e != nil {
			return e
		}
		actual, e := readRecovery(q)
		if e != nil {
			return e
		}
		if actual != r {
			return fmt.Errorf("Signal recovery authority changed")
		}
		return nil
	}
	if err = check(); err != nil {
		return r.status(), err
	}
	if !r.HasResult {
		result, e := apply(ctx, check)
		if e != nil {
			return r.status(), e
		}
		if err = c.boundary("recovery_applied"); err != nil {
			return r.status(), err
		}
		if !safefs.DigestPattern.MatchString(result) {
			return r.status(), fmt.Errorf("Signal recovery result digest required")
		}
		if err = check(); err != nil {
			return r.status(), err
		}
		if err = verify(ctx, result, check); err != nil {
			return r.status(), err
		}
		if err = check(); err != nil {
			return r.status(), err
		}
		next := r
		next.Result = recoveryResult{protocol.Hash(r.Intent), result}
		next.HasResult = true
		if err = publish(recoveryResultFile, next.Result, next); err != nil {
			return r.status(), err
		}
		if err = c.boundary("recovery_result"); err != nil {
			return r.status(), err
		}
	} else {
		if err = verify(ctx, r.Result.Result, check); err != nil {
			return r.status(), err
		}
	}
	if err = check(); err != nil {
		return r.status(), err
	}
	if !r.Complete {
		next := r
		next.Complete = true
		if err = publish(recoveryCompleteFile, r.Result, next); err != nil {
			return r.status(), err
		}
		if err = c.boundary("recovery_complete"); err != nil {
			return r.status(), err
		}
	}
	return r.status(), check()
}
func sameAccountDirectory(a, b *safefs.Directory) error {
	x, e := a.File().Stat()
	if e != nil {
		return e
	}
	y, e := b.File().Stat()
	if e != nil {
		return e
	}
	if !os.SameFile(x, y) {
		return fmt.Errorf("Signal account directory replaced")
	}
	return nil
}
func sameAccountLease(d *safefs.Directory, name string, lease *safefs.Lock) error {
	f, e := d.Open(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return e
	}
	defer f.Close()
	x, e := f.Stat()
	if e != nil {
		return e
	}
	y, e := lease.File().Stat()
	if e != nil {
		return e
	}
	if !os.SameFile(x, y) {
		return fmt.Errorf("Signal coordination lease replaced")
	}
	return nil
}
