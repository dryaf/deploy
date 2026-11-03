package backup

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
)

func notificationLedger(t *testing.T) (*backupState, notify.Identity, notify.Producer) {
	t.Helper()
	s, err := openBackupState(filepath.Join(t.TempDir(), "state"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	id := notify.Identity{Worker: "notification-worker", Recovery: "recovery-one"}
	p := notify.Producer{ID: "backup", Epoch: "producer-one"}
	ledgerTx(t, s, func(tx *sql.Tx) error { return installNotificationProducer(tx, id, p) })
	return s, id, p
}

func ledgerTx(t *testing.T, s *backupState, fn func(*sql.Tx) error) {
	t.Helper()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
func exportedReport(p notify.Producer, id, kind, key string, revision uint64) notify.Message {
	now := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	m := notify.Message{Producer: p, ID: id, Generation: "generation-one", Subject: "backup", Kind: kind, Severity: "info", ObservedAt: now, ExpiresAt: now.Add(5 * time.Minute), Text: "Current report", Route: "operations", ReportKey: key, ReportType: "daily", ReportRevision: revision, BasisRevision: 1}
	if kind == "catchup" {
		m.OutageID = key
		m.ReportType = "delivery-catchup"
	}
	return m
}

func TestBackupSharedCatchupReplacement(t *testing.T) {
	s, id, p := notificationLedger(t)
	var first, second notify.Message
	ledgerTx(t, s, func(tx *sql.Tx) (err error) {
		first, err = freezeNotificationExport(tx, exportedReport(p, "C1", "catchup", "outage-O", 1), []notificationObligation{{"outage-O", "catchup"}})
		return
	})
	cursor := notify.Cursor{Identity: id, Producer: p, Position: 1}
	ledgerTx(t, s, func(tx *sql.Tx) error {
		return importNotificationReceipts(tx, cursor, []notify.Receipt{{Position: 1, EventID: first.ID, Sequence: first.Sequence, State: "expired"}})
	})
	path := s.directory
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	s, err = openBackupState(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ledgerTx(t, s, func(tx *sql.Tx) error {
		ready, err := notificationCatchupReady(tx, "outage-O")
		if err != nil {
			return err
		}
		if !ready {
			t.Fatal("own expired catchup blocked replacement")
		}
		second, err = freezeNotificationExport(tx, exportedReport(p, "C2", "catchup", "outage-O", 2), []notificationObligation{{"outage-O", "catchup"}})
		return err
	})
	if first.Sequence == second.Sequence || first.ID == second.ID {
		t.Fatal("frozen identity reused")
	}
	cursor.Position = 2
	receipt := notify.Receipt{Position: 2, EventID: second.ID, Sequence: second.Sequence, State: "provider_accepted", Acceptance: &notify.Acceptance{ObservedAt: second.ObservedAt.Add(time.Second), Eligible: true}}
	ledgerTx(t, s, func(tx *sql.Tx) error { return importNotificationReceipts(tx, cursor, []notify.Receipt{receipt}) })
	ledgerTx(t, s, func(tx *sql.Tx) error { return importNotificationReceipts(tx, cursor, []notify.Receipt{receipt}) })
	var disposition, cover string
	if err := s.db.QueryRow("SELECT disposition,covered_by FROM notification_obligations WHERE id='outage-O'").Scan(&disposition, &cover); err != nil {
		t.Fatal(err)
	}
	if disposition != "satisfied" || cover != "C2" {
		t.Fatal(disposition, cover)
	}
	var accepted int
	if err := s.db.QueryRow("SELECT accepted FROM notification_exports WHERE id='C1'").Scan(&accepted); err != nil || accepted != 0 {
		t.Fatal("expired predecessor fabricated acceptance", err)
	}
}

func TestBackupSharedBatchMembershipAndCancellation(t *testing.T) {
	s, id, p := notificationLedger(t)
	m := exportedReport(p, "batch-1", "report", "group", 1)
	m.ReportType = "backlog"
	m.Incidents = []notify.Incident{{Subject: "backup", ID: "A", Revision: 1}, {Subject: "backup", ID: "B", Revision: 1}}
	ledgerTx(t, s, func(tx *sql.Tx) (err error) {
		m, err = freezeNotificationExport(tx, m, []notificationObligation{{"A-failure", "alert"}, {"B-failure", "alert"}})
		return
	})
	ledgerTx(t, s, func(tx *sql.Tx) error {
		ready, err := notificationCatchupReady(tx, "O")
		if ready {
			t.Fatal("pending batch counted as delivered")
		}
		return err
	})
	cursor := notify.Cursor{Identity: id, Producer: p, Position: 1}
	ledgerTx(t, s, func(tx *sql.Tx) error {
		return importNotificationReceipts(tx, cursor, []notify.Receipt{{Position: 1, EventID: m.ID, Sequence: m.Sequence, State: "cancelled"}})
	})
	ledgerTx(t, s, func(tx *sql.Tx) error {
		return waiveNotificationObligation(tx, "A-failure", "incident A resolved; recovery is recorded separately")
	})
	next := m
	next.ID = "batch-2"
	next.ReportRevision = 2
	next.Incidents = next.Incidents[1:]
	ledgerTx(t, s, func(tx *sql.Tx) (err error) {
		next, err = freezeNotificationExport(tx, next, []notificationObligation{{"B-failure", "alert"}})
		return
	})
	cursor.Position = 2
	ledgerTx(t, s, func(tx *sql.Tx) error {
		return importNotificationReceipts(tx, cursor, []notify.Receipt{{Position: 2, EventID: next.ID, Sequence: next.Sequence, State: "provider_accepted", Acceptance: &notify.Acceptance{ObservedAt: next.ObservedAt.Add(time.Second), Eligible: true}}})
	})
	var a, b string
	if err := s.db.QueryRow("SELECT disposition FROM notification_obligations WHERE id='A-failure'").Scan(&a); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT covered_by FROM notification_obligations WHERE id='B-failure'").Scan(&b); err != nil {
		t.Fatal(err)
	}
	if a != "waived" || b != "batch-2" {
		t.Fatal(a, b)
	}
}

func TestBackupSharedRejectsWrongCoverageAndFrozenMembership(t *testing.T) {
	s, _, p := notificationLedger(t)
	m := exportedReport(p, "batch", "report", "group", 1)
	m.ReportType = "backlog"
	m.Incidents = []notify.Incident{{Subject: "backup", ID: "A", Revision: 1}}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = freezeNotificationExport(tx, m, []notificationObligation{{"daily-period", "daily"}}); err == nil {
		t.Fatal("batch covered daily period")
	}
	tx.Rollback()
	ledgerTx(t, s, func(tx *sql.Tx) (err error) {
		m, err = freezeNotificationExport(tx, m, []notificationObligation{{"A", "alert"}})
		return
	})
	tx, err = s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = freezeNotificationExport(tx, m, []notificationObligation{{"B", "alert"}}); err == nil {
		t.Fatal("changed frozen membership")
	}
}
