package backup

import (
	"context"
	"fmt"
	"reflect"

	"github.com/dryaf/deploy/internal/safefs"
)

func productionFreshnessScope(version int) bool { return version == 23 || version == 25 }

func freshnessScope(version int) bool { return version == 14 || productionFreshnessScope(version) }

func ReconcileDeliveryDerivedFreshnessScope(ctx context.Context, home string, r VerificationNotificationScopeChange, credentials []string, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileReportPolicy(ctx, home, r, credentials, migrate, 23)
}

// Only capture and its health/maintenance triggers are staged. Reporting retains
// its independent activity and the existing gateway delivery owner.
func freshnessCaptureUnits(j WorkerSetupJournal) ([]backupActivityUnit, error) {
	all, err := backupActivityUnits(j)
	if err != nil {
		return nil, err
	}
	var units []backupActivityUnit
	for _, unit := range all {
		switch unit.Name {
		case "deploy-backup.timer", "deploy-backup.service", "deploy-backup-health.timer", "deploy-backup-health.service", "deploy-backup-maintenance.timer", "deploy-backup-maintenance.service":
			units = append(units, unit)
		}
	}
	if len(units) != 6 {
		return nil, fmt.Errorf("freshness update requires complete capture unit ownership")
	}
	return units, nil
}

func (v *setupNotificationScope) prepareFreshnessCapture(ctx context.Context, home string, deps *providerScopeDependencies) error {
	if v.Freshness == nil || deps.capture.inspect == nil || deps.capture.stop == nil {
		return fmt.Errorf("freshness capture inspection unavailable")
	}
	units, err := freshnessCaptureUnits(v.Before)
	if err != nil {
		return err
	}
	if _, err = deps.capture.inspect(ctx, home, units); err != nil {
		return err
	}
	v.Freshness.Capture = units
	return nil
}

func (v setupNotificationScope) validateFreshnessCapture() error {
	if !productionFreshnessScope(v.Version) {
		if len(v.Freshness.Capture) != 0 {
			return fmt.Errorf("unexpected production capture intent")
		}
		return nil
	}
	units, err := freshnessCaptureUnits(v.Before)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(units, v.Freshness.Capture) {
		return fmt.Errorf("freshness capture ownership changed")
	}
	return nil
}

func (v setupNotificationScope) inspectFreshnessCapture(ctx context.Context, deps *providerScopeDependencies) error {
	if deps.capture.inspect == nil {
		return fmt.Errorf("freshness capture inspection unavailable")
	}
	stopped, err := deps.capture.inspect(ctx, v.Before.Declaration.Home, v.Freshness.Capture)
	if err != nil {
		return err
	}
	if !stopped {
		return fmt.Errorf("backup capture must remain stopped during freshness publication")
	}
	return nil
}

func (v setupNotificationScope) stageFreshnessCapture(ctx context.Context, home string, deps *providerScopeDependencies, boundary func(string) error) error {
	if deps.capture.stop == nil {
		return fmt.Errorf("freshness capture stop unavailable")
	}
	if _, err := deps.capture.inspect(ctx, home, v.Freshness.Capture); err != nil {
		return err
	}
	if err := deps.capture.stop(ctx, home, v.Freshness.Capture); err != nil {
		return err
	}
	if err := v.inspectFreshnessCapture(ctx, deps); err != nil {
		return err
	}
	return boundary("capture-stopped")
}

// Project only completed, byte-pinned freshness changes back to the original
// reporting runtime. The caller still checks the current archive control and
// completed cutover; this cannot authorize a new handoff or unrelated updates.
func reportingScopeRuntime(root *safefs.Directory, j WorkerSetupJournal, current WorkerInstallation, scope string) (WorkerInstallation, error) {
	if _, err := commandScopeOrigin(root, j, scope); err != nil {
		return current, err
	}
	for depth := 0; depth < 128; depth++ {
		if j.Declaration.NotificationDesiredHash == scope {
			return current, nil
		}
		ref := j.NotificationScopeHistory
		if ref == nil {
			return current, fmt.Errorf("reporting runtime lost scope history")
		}
		var receipt setupNotificationScopeReceipt
		if err := root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+ref.Operation+".json", &receipt); err != nil {
			return current, err
		}
		v := receipt.Intent
		if productionFreshnessScope(v.Version) {
			paths, err := declarationPaths(v.Before.Declaration.Home, v.Before.Declaration)
			if err != nil {
				return current, err
			}
			var before, after WorkerInstallation
			if err = safefs.DecodeJSON(v.Freshness.Before[paths.OwnedPath(paths.Runtime)], &before, safefs.MaxRecordBytes); err != nil {
				return current, err
			}
			if err = safefs.DecodeJSON(v.Freshness.After[paths.OwnedPath(paths.Runtime)], &after, safefs.MaxRecordBytes); err != nil {
				return current, err
			}
			if !reflect.DeepEqual(current, after) {
				return current, fmt.Errorf("reporting runtime differs from completed freshness publication")
			}
			current = before
		}
		j = v.Before
	}
	return current, fmt.Errorf("reporting runtime history exceeds review bound")
}
