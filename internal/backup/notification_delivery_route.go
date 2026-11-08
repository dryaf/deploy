package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type reportRouteReference struct {
	Home      string `json:"home"`
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	Receipt   string `json:"receipt"`
}

func reportRouteScope(version int) bool { return version == 15 || version == 24 }

func ReconcileDeliveryReportRouteScope(ctx context.Context, home string, r VerificationNotificationScopeChange, credentials []string, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileReportPolicy(ctx, home, r, credentials, migrate, 24)
}
func ReconcileCommandDerivedFreshnessScope(ctx context.Context, home string, r VerificationNotificationScopeChange, credentials []string, migrate func(ProducerReservation) (string, error)) (string, error) {
	return reconcileReportPolicy(ctx, home, r, credentials, migrate, 25)
}

// The handoff and producer binding remain immutable. A changed selected route
// needs a captured original owner and a retained notification-scope edge.
func reportRouteOwnerShape(j WorkerSetupJournal, owner backupNotificationOwner) bool {
	return j.Declaration != nil && (owner.Binding.Route == j.Declaration.NotificationRoute || j.Shared != nil && j.Shared.Owner == owner && j.NotificationScopeHistory != nil)
}
func requireReportRouteOwner(j WorkerSetupJournal, owner backupNotificationOwner) error {
	if j.Declaration == nil {
		return fmt.Errorf("report route lost its declaration")
	}
	if owner.Binding.Route == j.Declaration.NotificationRoute {
		return nil
	}
	root, err := safefs.Open(j.Declaration.Home)
	if err != nil {
		return err
	}
	defer root.Close()
	for depth := 0; depth < 128; depth++ {
		if err = verifyNotificationScopeHistory(root, j); err != nil {
			return err
		}
		ref := j.NotificationScopeHistory
		if ref == nil {
			return fmt.Errorf("report route has no retained continuation from its original owner")
		}
		var edge setupNotificationScopeReceipt
		if err = root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+ref.Operation+".json", &edge); err != nil {
			return err
		}
		v := edge.Intent
		if v.After.Declaration.NotificationRoute != j.Declaration.NotificationRoute {
			return fmt.Errorf("report route differs from scope history")
		}
		if v.Version == 24 && (v.Reports == nil || v.Reports.Owner != owner) {
			return fmt.Errorf("report route continuation changed its original owner")
		}
		j = v.Before
		if j.Declaration.NotificationRoute == owner.Binding.Route {
			return nil
		}
	}
	return fmt.Errorf("report route history exceeds review bound")
}

// Publish only future report selection. Frozen messages, original pipeline and
// cutover records are unchanged. The enclosing installation gate excludes all
// reporters until the exact scope receipt is durable and acknowledged.
func (v setupNotificationScope) publishReportRoute(root *safefs.Directory, gateway string) error {
	if v.Version != 24 || v.validate() != nil || !safefs.DigestPattern.MatchString(gateway) {
		return fmt.Errorf("invalid report route publication")
	}
	p, err := declarationPaths(v.Before.Declaration.Home, v.Before.Declaration)
	if err != nil {
		return err
	}
	state, err := openBackupStateWithLease(p.State, false, false, true)
	if err != nil {
		return err
	}
	defer state.Close()
	if err = requireCommandScopeOwner(state, v.Before); err != nil {
		return err
	}

	receipt := setupNotificationScopeReceipt{1, v, gateway}
	want := reportRouteReference{Home: v.Before.Declaration.Home, Version: 1, Operation: v.Operation, Receipt: protocol.Hash(receipt)}
	var old reportRouteReference
	_, err = state.db.Exec("CREATE TABLE IF NOT EXISTS backup_notification_report_route(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL)")
	if err != nil {
		return err
	}
	if err = state.put("notification_report_route_required", true); err != nil {
		return err
	}
	var body string
	err = state.db.QueryRow("SELECT body FROM backup_notification_report_route WHERE singleton=1").Scan(&body)
	if err == nil {
		err = protocol.Decode([]byte(body), &old, 8192)
	}
	if err == nil {
		if old == want {
			return nil
		}
		if old.Home != v.Before.Declaration.Home || old.Version != 1 || !safefs.ValidID(old.Operation, "n") || !safefs.DigestPattern.MatchString(old.Receipt) {
			return fmt.Errorf("invalid report route predecessor")
		}
		var retained setupNotificationScopeReceipt
		if err = root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+old.Operation+".json", &retained); err != nil {
			return err
		}
		prior := retained.Intent
		if protocol.Hash(retained) != old.Receipt || prior.Version != 24 || prior.validate() != nil || prior.Reports.Owner != v.Reports.Owner || prior.After.Declaration.NotificationRoute != v.Before.Declaration.NotificationRoute {
			return fmt.Errorf("report route predecessor changed")
		}
	} else if err != sql.ErrNoRows {
		return err
	} else if v.Before.Declaration.NotificationRoute != v.Reports.Owner.Binding.Route {
		return fmt.Errorf("report route predecessor is missing")
	}

	raw, err := json.Marshal(want)
	if err != nil {
		return err
	}
	_, err = state.db.Exec("INSERT INTO backup_notification_report_route VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET body=excluded.body", string(raw))
	return err
}

func effectiveBackupReportRoute(tx *sql.Tx, binding backupNotificationBinding) (string, error) {

	var ref reportRouteReference
	var present int
	err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='backup_notification_report_route'").Scan(&present)
	if err != nil {
		return "", err
	}
	var required bool
	markerErr := backupNotificationMeta(tx, "notification_report_route_required", &required)
	if present == 0 && markerErr == sql.ErrNoRows {
		return binding.Route, nil
	}
	if markerErr != nil {
		return "", fmt.Errorf("report route initialization authority is missing: %w", markerErr)
	}
	if !required || present == 0 {
		return "", fmt.Errorf("report route SQL authority is missing")
	}

	var raw string
	err = tx.QueryRow("SELECT body FROM backup_notification_report_route WHERE singleton=1").Scan(&raw)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("report route authority is missing")
	}
	if err == nil {
		err = protocol.Decode([]byte(raw), &ref, 8192)
	}
	if err != nil {
		return "", err
	}
	if !backupRecoveryPath(ref.Home) || ref.Version != 1 || !safefs.ValidID(ref.Operation, "n") || !safefs.DigestPattern.MatchString(ref.Receipt) {
		return "", fmt.Errorf("invalid report route reference")
	}

	home := ref.Home
	root, err := safefs.Open(home)
	if err != nil {
		return "", err
	}
	defer root.Close()
	var receipt setupNotificationScopeReceipt
	if err = root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+ref.Operation+".json", &receipt); err != nil {
		return "", err
	}
	v := receipt.Intent
	if protocol.Hash(receipt) != ref.Receipt || receipt.Version != 1 || v.Version != 24 || v.validate() != nil || v.Reports.Owner.Binding != binding || v.Before.Declaration.Home != home || !safefs.DigestPattern.MatchString(receipt.Gateway) {
		return "", fmt.Errorf("report route lost original producer authority")
	}
	var current WorkerSetupJournal
	if err = root.ReadJSON(backupSetupFile, &current); err != nil {
		return "", err
	}
	if current.NotificationScope != nil || current.Stage != "complete" || current.Declaration == nil || current.Declaration.NotificationRoute != v.After.Declaration.NotificationRoute {
		return "", fmt.Errorf("report route is pending or differs from applied policy")
	}
	for depth := 0; depth < 128; depth++ {
		if err = verifyNotificationScopeHistory(root, current); err != nil {
			return "", err
		}
		ref := current.NotificationScopeHistory
		if ref == nil {
			return "", fmt.Errorf("report route has no completed scope receipt")
		}
		var edge setupNotificationScopeReceipt
		if err = root.ReadJSON(".local/state/deploy/backup-worker/notification-scope-"+ref.Operation+".json", &edge); err != nil {
			return "", err
		}
		if ref.Operation == v.Operation {
			if protocol.Hash(edge) != protocol.Hash(receipt) {
				return "", fmt.Errorf("report route SQL and scope receipt differ")
			}
			return v.After.Declaration.NotificationRoute, nil
		}
		current = edge.Intent.Before
	}
	return "", fmt.Errorf("report route history exceeds review bound")
}
