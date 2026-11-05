package install

import (
	"context"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

type admittedDeliverySender struct {
	check  func(context.Context) error
	fail   func(error)
	sender worker.Sender
}

func (a admittedDeliverySender) Send(ctx context.Context, name string, message protocol.Message) worker.Result {
	if err := a.check(ctx); err != nil {
		a.fail(err)
		return worker.Result{RetryAfter: time.Second}
	}
	return a.sender.Send(ctx, name, message)
}

func serveManagedDelivery(ctx context.Context, d *safefs.Directory, s *worker.Store, c Runtime, m ManagedManifest, installation *safefs.Lock, sender worker.Sender) error {
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkManagedDirectoryPath(d, m.Root, true); err != nil {
			return err
		}
		if err := managedLeaseMatches(d, "installation.lock", installation); err != nil {
			return err
		}
		var current ManagedManifest
		if err := d.ReadJSON("installation.json", &current); err != nil {
			return err
		}
		if protocol.Hash(current) != protocol.Hash(m) {
			return fmt.Errorf("delivery installation authority changed")
		}
		canonical, err := managedRuntime(d, m)
		if err != nil {
			return err
		}
		if protocol.Hash(canonical) != protocol.Hash(c) {
			return fmt.Errorf("delivery runtime changed")
		}
		units, err := managedUnitDirectory(m.UnitDirectory)
		if err != nil {
			return err
		}
		unit, present, err := managedUnitBytes(units)
		units.Close()
		if err != nil {
			return err
		}
		if !present || managedDigest(unit) != m.UnitHash {
			return fmt.Errorf("delivery unit changed")
		}
		if err := s.RequireManagedDeliveryOwner(deliveryOwner(m, c), m.DeliveryOwnerHash); err != nil {
			return err
		}
		return checkDeliveryAdmission(d, m, c, time.Now().UTC())
	}
	if s.VerificationOnly() || m.Mode != "delivery" {
		return fmt.Errorf("delivery runtime requires admitted delivery state")
	}
	if err := check(ctx); err != nil {
		return err
	}
	observe, err := s.MonitorRuntimeLoop("delivery-admission")
	if err != nil {
		return err
	}
	observe(nil)
	if sender == nil {
		sender = provider.Adapter{Profiles: c.Providers}
	}
	fatal := make(chan error, 1)
	fail := func(err error) {
		select {
		case fatal <- err:
		default:
		}
	}
	adapter := admittedDeliverySender{check: check, fail: fail, sender: sender}
	return runRuntimeGroup(ctx, []runtimeService{
		func(ctx context.Context) error { return s.Serve(ctx, m.Paths.Socket) },
		func(ctx context.Context) error { return s.RunDispatcher(ctx, adapter) },
		func(ctx context.Context) error {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case err := <-fatal:
					observe(err)
					return err
				case <-ticker.C:
					err := check(ctx)
					observe(err)
					if err != nil {
						return err
					}
				}
			}
		},
	})
}
