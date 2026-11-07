package install

import (
	"context"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
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
	effective, err := managedEffectiveRuntime(d, m, c)
	if err != nil {
		return err
	}
	var observeSignal func(error)
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
		runtime, err := managedEffectiveRuntime(d, m, canonical)
		if err != nil {
			return err
		}
		if protocol.Hash(runtime) != protocol.Hash(effective) {
			return fmt.Errorf("effective delivery runtime changed")
		}
		if m.FreshSignal != nil {
			err := checkFreshSignalGatewayHealth(ctx, m, 0)
			if observeSignal != nil {
				observeSignal(err)
			}
			if err != nil {
				return err
			}
		}
		if m.SignalVerification != nil {
			if _, err := readManagedSignalBinding(d, m); err != nil {
				return err
			}
			err := checkManagedSignalHealth(ctx, m)
			if observeSignal != nil {
				observeSignal(err)
			}
			if err != nil {
				return err
			}
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
		if err := requireDeliveryOwner(s, m, c); err != nil {
			return err
		}
		if err := s.RequireManagedCommandHistory(m.CommandHistoryHash); err != nil {
			return err
		}
		if m.Commands != nil {
			if err := requireManagedTelegramCommandHandoff(ctx, d, s, m, c); err != nil {
				return err
			}
			if _, err := readManagedCommandAdmission(d, m, c); err != nil {
				return err
			}
		}
		return checkDeliveryAdmission(d, m, c, time.Now().UTC())
	}
	if s.VerificationOnly() || m.Mode != "delivery" {
		return fmt.Errorf("delivery runtime requires admitted delivery state")
	}
	if err := check(ctx); err != nil {
		return err
	}
	if m.FreshSignal != nil {
		if err := s.BindFreshSignalService(m.FreshSignal.ManifestHash); err != nil {
			return err
		}
	}
	if m.SignalVerification != nil {
		if err := s.BindSignalVerification(m.SignalVerification.ManifestHash); err != nil {
			return err
		}
	}
	fatal := make(chan error, 1)
	fail := func(err error) {
		select {
		case fatal <- err:
		default:
		}
	}
	if sender == nil {
		adapter := provider.Adapter{Profiles: effective.Providers}
		if c.Signal != nil || m.FreshSignal != nil {
			transport, closeTransport, err := managedSignalTransport(m, check, fail)
			if err != nil {
				return err
			}
			defer closeTransport()
			adapter.SignalTransport = transport
		}
		sender = adapter
	}
	if m.Commands != nil {
		if err := s.BindManagedCommandAuthority(func(ctx context.Context) error {
			err := check(ctx)
			if err != nil {
				fail(err)
			}
			return err
		}); err != nil {
			return err
		}
	}
	if m.SignalVerification != nil || m.FreshSignal != nil {
		observeSignal, err = s.MonitorRuntimeLoop("signal-verification")
		if err != nil {
			return err
		}
		observeSignal(nil)
		defer observeSignal(fmt.Errorf("Signal supervision stopped"))
	}
	observe, err := s.MonitorRuntimeLoop("delivery-admission")
	if err != nil {
		return err
	}
	observe(nil)
	adapter := admittedDeliverySender{check: check, fail: fail, sender: sender}
	if c.LEDSocket != "" && m.RecoveryCommitHash != "" {
		api := admittedLEDClient{api: led.Client{Socket: c.LEDSocket, Budget: 2 * time.Second, Endpoint: c.LEDEndpoint}, check: check, fail: fail}
		if err := s.BindManagedLEDRecovery(api, check); err != nil {
			return err
		}
	}
	services := []runtimeService{
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
	}
	commands, err := managedCommandServices(d, s, c, m, check, fail, nil)
	if err != nil {
		return err
	}
	if c.LEDSocket != "" {
		services = append(services, managedLEDService(s, c.LEDSocket, check, fail, led.Client{Socket: c.LEDSocket, Budget: 2 * time.Second, Endpoint: c.LEDEndpoint}))
	}
	return runRuntimeGroup(ctx, append(services, commands...))
}
