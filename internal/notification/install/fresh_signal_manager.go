package install

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/dryaf/deploy/internal/safefs"
)

type userFreshSignalManager struct{ userSignalServiceManager }

func (u userFreshSignalManager) Inspect(ctx context.Context, m FreshSignalManifest) (signalServiceState, error) {
	return u.inspectUnit(ctx, freshSignalUnit, m.Spec.UnitDirectory, "/org/freedesktop/systemd1/unit/deploy_2dnotification_2dsignal_2eservice", m.executable(), m.commandArgs())
}
func (u userFreshSignalManager) Start(ctx context.Context) error {
	return u.ctl(ctx, "enable", "--now", freshSignalUnit)
}
func (u userFreshSignalManager) Stop(ctx context.Context) error {
	return u.ctl(ctx, "stop", freshSignalUnit)
}
func (u userFreshSignalManager) Disable(ctx context.Context) error {
	return u.ctl(ctx, "disable", freshSignalUnit)
}
func (u userFreshSignalManager) Verify(ctx context.Context, m FreshSignalManifest) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		err := u.verifyOnce(ctx, m)
		if err == nil {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("fresh Signal service readiness failed: %w", err)
		case <-timer.C:
		}
	}
}
func (u userFreshSignalManager) verifyOnce(ctx context.Context, m FreshSignalManifest) error {
	before, state, err := u.snapshotUnit(ctx, freshSignalUnit, m.Spec.UnitDirectory)
	if err != nil {
		return err
	}
	if !state.Active || !state.Enabled {
		return fmt.Errorf("fresh Signal service is not enabled and running")
	}
	if _, err = u.Inspect(ctx, m); err != nil {
		return err
	}
	want, err := freshSignalHealth(m)
	if err != nil {
		return err
	}
	err = checkNativeSignalHealth(ctx, m.Spec.socket(), state.PID, func(pid uint32) error { return checkManagedNativeProcess(pid, m.executable(), m.commandArgs()) }, func(raw []byte) error {
		var actual FreshSignalHealth
		if safefs.DecodeJSON(raw, &actual, 16384) != nil || actual != want {
			return fmt.Errorf("fresh Signal health pins changed")
		}
		return nil
	})
	if err != nil {
		return err
	}
	after, _, err := u.snapshotUnit(ctx, freshSignalUnit, m.Spec.UnitDirectory)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("fresh Signal service changed during health check")
	}
	return nil
}
