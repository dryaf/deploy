//go:build linux

package backup

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

func TestBackupReminderCancellationPreservesMixedBatchMembers(t *testing.T) {
	s, l, handlers, now, c, g, incident := backupCommandsFixture(t)
	policy := configfixture.WorkerPolicy(t)
	*now = now.Add(6 * time.Hour)
	testutil.Must(t, s.observeIncident("capture", "a", "visible failure", *now, policy.Notifications))
	for i := 0; i < 10; i++ {
		testutil.Must(t, s.observeIncident("capture", fmt.Sprintf("extra-%d", i), "other failure", *now, policy.Notifications))
	}
	testutil.Must(t, coalesceNotificationBacklog(s, *now))
	var batch string
	testutil.Must(t, s.db.QueryRow("SELECT id FROM outbox WHERE kind='batch' AND covered_by IS NULL").Scan(&batch))
	c.IssuedAt = *now
	c.Deadline = now.Add(time.Minute)
	if r := executeBackupFixture(t, l, handlers["backup.ack"], c, g, *now); r.State != "succeeded" {
		t.Fatal(r)
	}
	var covered string
	testutil.Must(t, s.db.QueryRow("SELECT covered_by FROM outbox WHERE id=?", batch).Scan(&covered))
	if covered != "command/"+c.ID {
		t.Fatal("old batch still sendable", covered)
	}
	var unrelated, suppressed int
	testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='failure' AND covered_by IS NULL AND accepted IS NULL").Scan(&unrelated))
	testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='reminder' AND json_extract(record,'$.incident_id')=? AND covered_by=?", incident, "command/"+c.ID).Scan(&suppressed))
	if unrelated != 12 || suppressed != 1 {
		t.Fatalf("unrelated=%d suppressed=%d", unrelated, suppressed)
	}
}

func TestBackupReminderMutationCannotSplitInFlightLegacyDelivery(t *testing.T) {
	s, l, handlers, now, c, g, _ := backupCommandsFixture(t)
	policy := configfixture.WorkerPolicy(t)
	called := false
	testutil.Must(t, drainNotificationOutbox(context.Background(), s, WorkerInstallation{Policy: policy}, func(context.Context, string, config.WorkerNotifications, notification.NotificationEvent) (notification.DeliveryAttempt, error) {
		if !called {
			called = true
			r := executeBackupFixture(t, l, handlers["backup.ack"], c, g, *now)
			if r.State != "failed" || !strings.Contains(r.Text, "delivery is in progress") {
				t.Fatal(r)
			}
		}
		return notification.DeliveryAttempt{Accepted: true}, nil
	}, *now))
	if !called {
		t.Fatal("no delivery")
	}
	var count int
	testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM backup_command_suppressions").Scan(&count))
	if count != 0 {
		t.Fatal("changed suppression during send")
	}
	c.ID = "C-after"
	c.RequestKey = "after"
	if r := executeBackupFixture(t, l, handlers["backup.ack"], c, g, *now); r.State != "succeeded" {
		t.Fatal("delivery guard not released", r)
	}
}

func backupCommandsFixture(t *testing.T) (*backupState, *interaction.Ledger, map[string]client.CommandHandler, *time.Time, interaction.Command, interaction.Grant, string) {
	t.Helper()
	_, state, _, _, _ := archiveFixture(t)
	policy := configfixture.WorkerPolicy(t)
	now := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	testutil.Must(t, state.observeIncident("capture", "a", "visible failure", now, policy.Notifications))
	testutil.Must(t, state.observeIncident("capture", "b", "confidential failure", now, policy.Notifications))
	p := notify.Producer{ID: "backup", Epoch: "one"}
	identity := notify.Identity{Worker: "gateway", Recovery: "one"}
	ledger, err := interaction.OpenDomainLedger(context.Background(), state.db, p, identity, true, initializeBackupCommands)
	testutil.Must(t, err)
	dest := interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100"}
	manifest := backupCommandManifest(true)
	cfg := interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "backup/storage", Producer: p, Generation: "one", Commands: manifest}}, Destinations: []interaction.Destination{dest}, Permissions: []interaction.Permission{{Actor: "actor", Conversation: "ops", Worker: p.ID, Permissions: []string{"read", "incident.manage"}, Scope: interaction.Scope{Subjects: []string{"a"}}}}}
	handlers, err := backupCommandHandlers(cfg, p, true, func() time.Time { return now })
	testutil.Must(t, err)
	var incident string
	testutil.Must(t, state.db.QueryRow("SELECT id FROM incidents WHERE instance='a'").Scan(&incident))
	c := interaction.Command{ID: "C-one", RequestKey: "one", InputHash: "one", Producer: p, Generation: "one", Actor: "actor", Destination: dest, Scope: interaction.Scope{Subjects: []string{"a"}}, IssuedAt: now, Deadline: now.Add(15 * time.Minute), Descriptor: manifest["ack"], Arguments: map[string]string{"subject": "a", "target": incident, "evidence_revision": "1", "decision_revision": "1"}}
	g := interaction.Grant{Identity: identity, ID: "grant", CommandID: c.ID, CommandHash: notify.Hash(c), ExpiresAt: now.Add(10 * time.Second)}
	return state, ledger, handlers, &now, c, g, incident
}

func executeBackupFixture(t *testing.T, l *interaction.Ledger, h client.CommandHandler, c interaction.Command, g interaction.Grant, now time.Time) interaction.Result {
	t.Helper()
	testutil.Must(t, l.Import(context.Background(), c))
	g.CommandID = c.ID
	g.CommandHash = notify.Hash(c)
	g.ExpiresAt = now.Add(10 * time.Second)
	r, err := l.Execute(context.Background(), c, g, "", now, h.Apply)
	testutil.Must(t, err)
	return r
}

func TestBackupAcknowledgementIsAtomicAndDoesNotResolveIncident(t *testing.T) {
	s, l, handlers, now, c, g, incident := backupCommandsFixture(t)
	ctx := context.Background()
	testutil.Must(t, l.Import(ctx, c))
	_, err := l.Execute(ctx, c, g, "", *now, func(tx *sql.Tx, cmd interaction.Command) (interaction.Result, error) {
		r, e := handlers["backup.ack"].Apply(tx, cmd)
		if e != nil {
			return r, e
		}
		return r, fmt.Errorf("injected failure after domain writes")
	})
	if err == nil {
		t.Fatal("injected failure ignored")
	}
	var count int
	testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM backup_command_suppressions").Scan(&count))
	if count != 0 {
		t.Fatal("suppression survived result rollback")
	}
	r := executeBackupFixture(t, l, handlers["backup.ack"], c, g, *now)
	if r.State != "succeeded" {
		t.Fatal(r)
	}
	var until string
	testutil.Must(t, s.db.QueryRow("SELECT expires FROM backup_command_suppressions WHERE incident=?", incident).Scan(&until))
	*now = now.Add(30 * time.Minute)
	again, err := l.Execute(ctx, c, g, "", *now, handlers["backup.ack"].Apply)
	testutil.Must(t, err)
	if notify.Hash(r) != notify.Hash(again) {
		t.Fatal("replay renewed acknowledgement")
	}
	var after string
	testutil.Must(t, s.db.QueryRow("SELECT expires FROM backup_command_suppressions WHERE incident=?", incident).Scan(&after))
	if after != until {
		t.Fatal("deadline renewed")
	}
	testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM incidents WHERE resolved IS NULL").Scan(&count))
	if count != 2 {
		t.Fatal("acknowledgement cleared health")
	}
	testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='failure' AND covered_by IS NULL").Scan(&count))
	if count != 2 {
		t.Fatal("initial failures suppressed")
	}
}

func TestBackupReminderExpiryAndEscalationBypassSuppression(t *testing.T) {
	for _, escalation := range []bool{false, true} {
		t.Run(fmt.Sprint(escalation), func(t *testing.T) {
			s, l, handlers, now, c, g, incident := backupCommandsFixture(t)
			policy := configfixture.WorkerPolicy(t)
			*now = now.Add(6 * time.Hour)
			testutil.Must(t, s.observeIncident("capture", "a", "visible failure", *now, policy.Notifications))
			c.IssuedAt = *now
			c.Deadline = now.Add(time.Minute)
			r := executeBackupFixture(t, l, handlers["backup.ack"], c, g, *now)
			if r.State != "succeeded" {
				t.Fatal(r)
			}
			var count int
			testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='reminder' AND covered_by IS NULL AND json_extract(record,'$.incident_id')=?", incident).Scan(&count))
			if count != 0 {
				t.Fatal("queued reminder not cancelled")
			}
			*now = now.Add(30 * time.Minute)
			testutil.Must(t, s.observeIncident("capture", "a", "visible failure", *now, policy.Notifications))
			testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='reminder' AND covered_by IS NULL").Scan(&count))
			if count != 0 {
				t.Fatal("reminder suppression ineffective")
			}
			message := "visible failure"
			if escalation {
				message = "more severe failure"
			} else {
				*now = now.Add(31 * time.Minute)
			}
			testutil.Must(t, s.observeIncident("capture", "a", message, *now, policy.Notifications))
			testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='reminder' AND covered_by IS NULL").Scan(&count))
			if count != 1 {
				t.Fatal("expiry/escalation did not restore reminder within the same repeat slot", count)
			}
			testutil.Must(t, s.observeIncident("capture", "a", message, *now, policy.Notifications))
			testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='reminder' AND covered_by IS NULL").Scan(&count))
			if count != 1 {
				t.Fatal("repeated health evaluation duplicated reminder")
			}
		})
	}
}

func TestBackupCommandsScopeAndStaleEvidence(t *testing.T) {
	s, l, handlers, now, c, g, _ := backupCommandsFixture(t)
	read := c
	read.ID = "C-read"
	read.RequestKey = "read"
	read.Arguments = map[string]string{}
	read.Descriptor = backupCommandManifest(false)["status"]
	r := executeBackupFixture(t, l, handlers["backup.status"], read, g, *now)
	if r.State != "succeeded" || !strings.Contains(r.Text, "scope: 1") || strings.Contains(r.Text, "confidential") {
		t.Fatal("status disclosed other subject", r)
	}
	policy := configfixture.WorkerPolicy(t)
	testutil.Must(t, s.observeIncident("capture", "a", "changed evidence", *now, policy.Notifications))
	r = executeBackupFixture(t, l, handlers["backup.ack"], c, g, *now)
	if r.State != "failed" {
		t.Fatal("stale acknowledgement admitted")
	}
	var count int
	testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM backup_command_suppressions").Scan(&count))
	if count != 0 {
		t.Fatal("stale command changed reminders")
	}
}
