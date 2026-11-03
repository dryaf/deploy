//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func backupCleanupFixture(t *testing.T) (*backupLEDAdapterFixture, notify.RecoveryLEDCleanupSession, worker.RecoveryLEDBindingResolution) {
	t.Helper()
	f := newBackupLEDAdapterFixture(t)
	_, err := reconcileBackupLEDRecovery(context.Background(), f.state, f.config, f.session, f.inventory.InventoryID)
	testutil.Must(t, err)
	v := f.review(t)
	proposal := v.Resolutions["backups"]
	if !proposal.DomainResolved || len(proposal.ProposedFaultIDs) == 0 {
		t.Fatal(v.Resolutions)
	}
	testutil.Must(t, f.session.Close())
	session, err := worker.OpenRecoveryLEDCleanupSession(context.Background(), f.gatewayPath, f.gatewayConfig, "/private/staged-led.sock", f.registry)
	testutil.Must(t, err)
	t.Cleanup(func() { session.Close() })
	return f, session, proposal
}

func TestBackupLEDRecoveryCleanupUsesFreshDomain(t *testing.T) {
	for _, scenario := range []string{"healthy", "incident", "pause", "probe", "qualification", "owner", "reboot", "stale", "monotonic_stale", "receipt", "reporter_lease", "coordinator_lease"} {
		t.Run(scenario, func(t *testing.T) {
			f, session, proposal := backupCleanupFixture(t)
			ctx := context.Background()
			sequence := backupLEDSequence(t, f.state)
			writes := f.registry.writes
			clock := reportHostClock
			switch scenario {
			case "incident":
				testutil.Must(t, backupTransferTx(ctx, f.state, func(tx *sql.Tx) error {
					_, err := observeBackupIncident(tx, "capture", f.subject, "new failure", time.Now(), f.config.Policy.Notifications)
					return err
				}))
			case "pause":
				testutil.Must(t, f.state.put("paused_until", time.Now().Add(time.Hour)))
			case "probe":
				_, err := f.state.db.Exec("DELETE FROM meta WHERE key='health_probe'")
				testutil.Must(t, err)
			case "qualification":
				f.config.Qualification["rootless_runtime"] = false
			case "owner":
				testutil.Must(t, f.state.put("notification_owner", backupNotificationOwner{Schema: 1, Mode: "shared", Binding: f.binding, Socket: "/private/staged-worker.sock"}))
			case "reboot":
				clock = func() backupEvaluationClock { c := reportHostClock(); c.Boot = "other-boot"; return c }
			case "stale":
				clock = func() backupEvaluationClock {
					c := reportHostClock()
					c.Now = c.Now.Add(time.Hour)
					c.Uptime += 3600
					return c
				}
			case "monotonic_stale":
				clock = func() backupEvaluationClock { c := reportHostClock(); c.Uptime += 130; return c }
			case "receipt":
				_, err := f.state.db.Exec("UPDATE backup_led_recovery_reports SET receipt=''")
				testutil.Must(t, err)
			case "reporter_lease", "coordinator_lease":
				d, err := safefs.Open(f.state.directory)
				testutil.Must(t, err)
				defer d.Close()
				name := "notifier.lock"
				if scenario == "coordinator_lease" {
					name = "notification-recovery.lock"
				}
				l, err := d.Lock(name, true, false)
				testutil.Must(t, err)
				defer l.Close()
			}
			out, err := clearBackupRecoveryLEDFaultWith(ctx, f.state, f.config, session, f.inventory.InventoryID, "backups", proposal.ProposedFaultIDs[0], proposal.EvidenceHash, clock)
			if scenario == "healthy" {
				testutil.Must(t, err)
				if out.Outcome != "acknowledged" || f.registry.writes != writes+1 {
					t.Fatal(out)
				}
				_, err = clearBackupRecoveryLEDFault(ctx, f.state, f.config, session, f.inventory.InventoryID, "backups", proposal.ProposedFaultIDs[0], proposal.EvidenceHash)
				testutil.Must(t, err)
				if f.registry.writes != writes+1 {
					t.Fatal("replayed clear")
				}
			} else if err == nil || f.registry.writes != writes {
				t.Fatal("unsafe clear", scenario, out, err)
			}
			if backupLEDSequence(t, f.state) != sequence {
				t.Fatal("guard committed speculative evaluation")
			}
		})
	}
}

type inspectingBackupCleanupSession struct {
	notify.RecoveryLEDCleanupSession
	inspect func(context.Context, notify.RecoveryLEDEvidence) error
}

func (s inspectingBackupCleanupSession) ClearLED(ctx context.Context, request notify.RecoveryLEDClearRequest, guard notify.RecoveryLEDDomainGuard) (notify.RecoveryLEDClearResult, error) {
	return s.RecoveryLEDCleanupSession.ClearLED(ctx, request, func(ctx context.Context, p notify.RecoveryLEDEvidence) error {
		if err := s.inspect(ctx, p); err != nil {
			return err
		}
		return guard(ctx, p)
	})
}

func TestBackupLEDRecoveryCleanupRetainsLeasesAndRechecks(t *testing.T) {
	f, session, proposal := backupCleanupFixture(t)
	writes := f.registry.writes
	calls := 0
	wrapped := inspectingBackupCleanupSession{session, func(ctx context.Context, proof notify.RecoveryLEDEvidence) error {
		calls++
		d, err := safefs.Open(f.state.directory)
		if err != nil {
			return err
		}
		defer d.Close()
		for _, name := range []string{"notifier.lock", "notification-recovery.lock"} {
			l, err := d.Lock(name, true, false)
			if err == nil {
				l.Close()
				return errors.New("source lease not held")
			}
		}
		// No source DB transaction survives across a worker call or daemon I/O.
		return backupTransferTx(ctx, f.state, func(tx *sql.Tx) error {
			if calls == 2 {
				_, err := observeBackupIncident(tx, "capture", f.subject, "changed while journaling", time.Now(), f.config.Policy.Notifications)
				return err
			}
			return nil
		})
	}}
	out, err := clearBackupRecoveryLEDFault(context.Background(), f.state, f.config, wrapped, f.inventory.InventoryID, "backups", proposal.ProposedFaultIDs[0], proposal.EvidenceHash)
	if err == nil || out.Outcome != "not_sent" || f.registry.writes != writes || calls != 2 {
		t.Fatal(out, err, calls)
	}
}
