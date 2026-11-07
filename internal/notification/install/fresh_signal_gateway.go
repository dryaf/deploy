package install

import (
	"context"
	"fmt"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func validateFreshSignalGateway(ctx context.Context, ref FreshSignalReference, root, state, units string, c Runtime, health bool) (FreshSignalManifest, error) {
	m, err := InspectFreshSignal(ctx, ref, health)
	if err != nil {
		return m, err
	}
	return m, validateFreshSignalGatewayManifest(m, root, state, units, c)
}
func validateFreshSignalGatewayManifest(m FreshSignalManifest, root, state, units string, c Runtime) error {
	if c.Signal != nil || c.Worker.Interaction != nil {
		return fmt.Errorf("fresh Signal direct service does not grant interactive command intake")
	}
	if m.Spec.UnitDirectory != units {
		return fmt.Errorf("fresh Signal service uses a different user manager directory")
	}
	for _, p := range []string{m.Spec.Root, m.Spec.AccountState, m.Spec.Inbox.State} {
		for _, q := range []string{root, state} {
			if managedWithin(p, q) || managedWithin(q, p) {
				return fmt.Errorf("fresh Signal service overlaps gateway state")
			}
		}
	}
	expected, err := freshSignalHealth(m)
	if err != nil {
		return err
	}
	profiles := 0
	for _, p := range c.Providers {
		if p.Kind != "signal" {
			continue
		}
		profiles++
		if p.Socket != m.Spec.socket() || p.Destination != expected.Destination || p.Conversation != nil {
			return fmt.Errorf("fresh Signal profile differs from owned service")
		}
	}
	if profiles == 0 {
		return fmt.Errorf("fresh Signal service has no declared provider profile")
	}
	return nil
}
func checkFreshSignalGatewayHealth(ctx context.Context, m ManagedManifest, pid uint32) error {
	service, err := InspectFreshSignal(ctx, *m.FreshSignal, false)
	if err != nil {
		return err
	}
	// Runtime checks avoid manager commands inside the sandbox; the socket peer's
	// actual executable, UID, argv and stable process identity prove service origin.
	want, err := freshSignalHealth(service)
	if err != nil {
		return err
	}
	return checkNativeSignalHealth(ctx, service.Spec.socket(), pid, func(pid uint32) error {
		return checkManagedNativeProcess(pid, service.executable(), service.commandArgs())
	}, func(raw []byte) error {
		var actual FreshSignalHealth
		if safefs.DecodeJSON(raw, &actual, 16384) != nil || actual != want {
			return fmt.Errorf("fresh Signal gateway health pins changed")
		}
		return nil
	})
}
func serveFreshSignalGateway(ctx context.Context, d *safefs.Directory, s *worker.Store, c Runtime, m ManagedManifest, lease *safefs.Lock) error {
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkManagedDirectoryPath(d, m.Root, true); err != nil {
			return err
		}
		if err := managedLeaseMatches(d, "installation.lock", lease); err != nil {
			return err
		}
		var current ManagedManifest
		if err := d.ReadJSON("installation.json", &current); err != nil {
			return err
		}
		if protocol.Hash(current) != protocol.Hash(m) {
			return fmt.Errorf("fresh Signal gateway authority changed")
		}
		canonical, err := managedRuntime(d, m)
		if err != nil {
			return err
		}
		if protocol.Hash(canonical) != protocol.Hash(c) {
			return fmt.Errorf("fresh Signal gateway runtime changed")
		}
		if _, err = validateFreshSignalGateway(ctx, *m.FreshSignal, m.Root, m.Paths.State, m.UnitDirectory, c, false); err != nil {
			return err
		}
		return checkFreshSignalGatewayHealth(ctx, m, 0)
	}
	if err := check(ctx); err != nil {
		return err
	}
	if err := s.BindFreshSignalService(m.FreshSignal.ManifestHash); err != nil {
		return err
	}
	observe, err := s.MonitorRuntimeLoop("signal-verification")
	if err != nil {
		return err
	}
	observe(nil)
	return runRuntimeGroup(ctx, []runtimeService{
		func(ctx context.Context) error { return serveRuntime(ctx, s, c, m.Paths.Socket) },
		func(ctx context.Context) error {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			defer observe(fmt.Errorf("fresh Signal supervision stopped"))
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
				err := check(ctx)
				observe(err)
				if err != nil {
					return err
				}
			}
		},
	})
}
