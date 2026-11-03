//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/sqlite"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupRetentionHonorsKnownIntegrityFailure(t *testing.T) {
	a, s, attempt, header, data := archiveFixture(t)
	publish := func(h protocol.SnapshotHeader, data []byte) SnapshotReceipt {
		t.Helper()
		testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error {
			key := instanceKey(h.Attempt.SourceID, h.Attempt.Service)
			v := c.Instances[key]
			v.CaptureSequence = h.Attempt.CaptureSequence
			c.Instances[key] = v
			return nil
		}))
		r, err := a.receive(s, h.Attempt, "source", 1<<20, func(f *os.File) (protocol.SnapshotHeader, error) { _, err := f.Write(data); return h, err }, func(f *os.File) (protocol.SnapshotValidation, error) {
			return validateArchivedFile(context.Background(), f, false)
		})
		testutil.Must(t, err)
		return r
	}
	good := publish(header, data)
	path := filepath.Join(t.TempDir(), "bad.db")
	db, err := sql.Open("sqlite", path)
	testutil.Must(t, err)
	_, err = db.Exec(`CREATE TABLE t(a INTEGER,b INTEGER); INSERT INTO t VALUES(1,2); CREATE INDEX idx ON t(a); PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql='CREATE INDEX idx ON t(b)' WHERE name='idx'; PRAGMA writable_schema=OFF;`)
	testutil.Must(t, err)
	testutil.Must(t, db.Close())
	data, err = os.ReadFile(path)
	testutil.Must(t, err)
	header.Size = int64(len(data))
	header.SHA256 = fmtDigest(data)
	header.Attempt.CaptureSequence++
	header.Attempt.Nonce = testutil.ID(t, "n")
	header.SnapshotID = testutil.ID(t, "b")
	bad := publish(header, data) // Real quick_check passes this inconsistent index.
	file, err := os.Open(path)
	testutil.Must(t, err)
	defer file.Close()
	_, err = validateArchivedFile(context.Background(), file, true)
	var classified *sqlite.ValidationError
	if !errors.As(err, &classified) || classified.Kind != "integrity" {
		t.Fatalf("fixture did not fail integrity: %v", err)
	}
	record := ArchiveVerification{Schema: 1, ID: testutil.ID(t, "n"), ArchiveID: a.identity.ID, BackupID: bad.BackupID, SHA256: bad.Header.SHA256, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), ErrorKind: "integrity"}
	testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error {
		c.Retention = &config.RetentionConfig{KeepLast: 1}
		v := c.Instances[instanceKey(attempt.SourceID, attempt.Service)]
		v.Generation = bad.Header.Generation
		c.Instances[instanceKey(attempt.SourceID, attempt.Service)] = v
		return a.writeVerification(s, c, &record)
	}))
	assertBlocked := func() {
		t.Helper()
		_, err := a.prune(s, ArchiveFilter{}, true, time.Now().UTC())
		if err == nil || !strings.Contains(err.Error(), "known integrity failure") {
			t.Fatalf("expected known-corruption guard, got %v", err)
		}
		for _, r := range []SnapshotReceipt{good, bad} {
			_, err := os.Stat(filepath.Join(a.dir.File().Name(), receiptPath(r), "database.db"))
			testutil.Must(t, err)
		}
	}
	assertBlocked()
	// Missing local acknowledgement must not erase the archive's failure evidence.
	_, err = s.db.Exec("DELETE FROM meta WHERE key=?", "verification/"+bad.BackupID)
	testutil.Must(t, err)
	assertBlocked()
	// Conversely, a failed archive checkpoint must not hide local failure evidence.
	testutil.Must(t, s.rememberVerification(record))
	testutil.Must(t, os.Remove(filepath.Join(a.dir.File().Name(), "control/verification", record.ID+".json")))
	assertBlocked()
	if err = a.deleteRetainedBundle(s, good, ArchiveFilter{}, time.Now().UTC()); err == nil || !strings.Contains(err.Error(), "known integrity failure") {
		t.Fatalf("final deletion guard bypassed: %v", err)
	}
}

func TestBackupRetentionBlocksSuspectClock(t *testing.T) {
	a, s, attempt, header, data := archiveFixture(t)
	var good SnapshotReceipt
	for i := 0; i < 2; i++ {
		header.Attempt.CaptureSequence = uint64(i + 1)
		header.Attempt.Nonce = testutil.ID(t, "n")
		header.SnapshotID = testutil.ID(t, "b")
		testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error {
			key := instanceKey(attempt.SourceID, attempt.Service)
			v := c.Instances[key]
			v.CaptureSequence = header.Attempt.CaptureSequence
			c.Instances[key] = v
			return nil
		}))
		r, err := a.receive(s, header.Attempt, "source", 1<<20, func(f *os.File) (protocol.SnapshotHeader, error) { _, err := f.Write(data); return header, err }, func(f *os.File) (protocol.SnapshotValidation, error) {
			return validateArchivedFile(context.Background(), f, false)
		})
		testutil.Must(t, err)
		if i == 0 {
			good = r
		}
	}
	testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error {
		c.Retention = &config.RetentionConfig{KeepLast: 1, MaxAge: "24h"}
		key := instanceKey(attempt.SourceID, attempt.Service)
		v := c.Instances[key]
		v.Generation = header.Generation
		c.Instances[key] = v
		return nil
	}))
	testutil.Must(t, s.put("clock_suspect", true))
	// The host detected a forward wall-clock jump and requires explicit ack-clock.
	_, err := a.prune(s, ArchiveFilter{}, true, time.Now().Add(48*time.Hour))
	if err == nil || !strings.Contains(err.Error(), "clock uncertainty") {
		t.Fatalf("clock guard missing: %v", err)
	}
	if err = a.deleteRetainedBundle(s, good, ArchiveFilter{}, time.Now().Add(48*time.Hour)); err == nil || !strings.Contains(err.Error(), "clock uncertainty") {
		t.Fatalf("final clock guard bypassed: %v", err)
	}
	// Read-only preview stays available during reconciliation.
	_, err = a.prune(s, ArchiveFilter{}, false, time.Now().UTC())
	testutil.Must(t, err)
	testutil.Must(t, s.put("clock_suspect", false))
	_, err = a.prune(s, ArchiveFilter{}, true, time.Now().UTC())
	testutil.Must(t, err)
	_, err = os.Stat(filepath.Join(a.dir.File().Name(), receiptPath(good), "database.db"))
	testutil.Must(t, err)
	// After acknowledgement, normal expiration still works and preserves the minimum.
	result, err := a.prune(s, ArchiveFilter{}, true, time.Now().Add(48*time.Hour))
	testutil.Must(t, err)
	deleted := 0
	for _, row := range result.Candidates {
		if row.Deleted {
			deleted++
		}
	}
	if deleted != 1 {
		t.Fatalf("normal retention deleted %d backups, wanted 1", deleted)
	}
}

func TestBackupClockGuardPreservesDiagnosticMaintenance(t *testing.T) {
	a, s, c, r := healthyWorkerFixture(t)
	testutil.Must(t, s.put("clock_suspect", true))
	testutil.Must(t, s.put("paused_until", time.Now().Add(2*time.Hour)))
	testutil.Must(t, maintenanceAdmission(c, s, 25*time.Minute, time.Now().UTC()))
	key := instanceKey(r.Header.Attempt.SourceID, r.Header.Attempt.Service)
	testutil.Must(t, s.put("maintenance/full/"+key+"/"+r.Header.Generation, maintenanceAttempt{At: time.Now().UTC(), Passed: true, BackupID: r.BackupID}))
	c.Policy.RestoreChecks = nil
	c.Policy.Retention = &config.RetentionConfig{KeepLast: 1}
	testutil.Must(t, runScheduledMaintenance(context.Background(), c, s, a))
	var deferred string
	testutil.Must(t, s.get("maintenance_deferred", &deferred))
	if !strings.Contains(deferred, "clock uncertainty") {
		t.Fatalf("scheduled retention not deferred: %s", deferred)
	}
}

func TestBackupRetentionProtectsCountAgeAndPins(t *testing.T) {
	a, state, attempt, header, _ := archiveFixture(t)
	var control ArchiveControl
	if err := state.get("control", &control); err != nil {
		t.Fatal(err)
	}
	instance := control.Instances[instanceKey(attempt.SourceID, attempt.Service)]
	instance.Generation = header.Generation
	control.Instances[instanceKey(attempt.SourceID, attempt.Service)] = instance
	control.Retention = &config.RetentionConfig{KeepLast: 2, MaxAge: "24h"}
	now := time.Now().UTC()
	var receipts []SnapshotReceipt
	for i := 0; i < 5; i++ {
		h := header
		h.Attempt.CaptureSequence = uint64(i + 1)
		r := SnapshotReceipt{Schema: 1, ArchiveID: a.identity.ID, BackupID: testutil.ID(t, "b"), WriterID: a.identity.WriterID, WriterEpoch: 1, SourceAlias: "vps-main", Header: h, ReceivedAt: now.Add(-time.Duration(5-i) * 24 * time.Hour)}
		receipts = append(receipts, r)
	}
	receipts[2].ReceivedAt = now.Add(-time.Hour)
	control.Pins[receipts[0].BackupID] = "full-integrity"
	result, err := retentionPreview(control, receipts, ArchiveFilter{}, now)
	if err != nil {
		t.Fatal(err)
	}
	deleted := []string{}
	for _, r := range result.Candidates {
		if r.Delete {
			deleted = append(deleted, r.Receipt.BackupID)
		}
	}
	if len(deleted) != 1 || deleted[0] != receipts[1].BackupID {
		t.Fatal("unsafe count/age/pin decision", deleted)
	}
	control.Retention = nil
	result, err = retentionPreview(control, receipts, ArchiveFilter{}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range result.Candidates {
		if r.Delete {
			t.Fatal("absent retention policy allowed deletion")
		}
	}
}
