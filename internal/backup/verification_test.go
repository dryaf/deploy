//go:build linux

package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/testutil"
	"golang.org/x/sys/unix"
)

func TestBackupFailedVerificationPersistsWithoutPin(t *testing.T) {
	a, state, attempt, header, data := archiveFixture(t)
	receipt, err := a.receive(state, attempt, "source", 1<<20, func(f *os.File) (protocol.SnapshotHeader, error) { _, err := f.Write(data); return header, err }, func(f *os.File) (protocol.SnapshotValidation, error) {
		return validateArchivedFile(context.Background(), f, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	file, err := a.dir.Open(receiptPath(receipt)+"/database.db", unix.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("bad!"), 0); err != nil {
		t.Fatal(err)
	}
	file.Close()
	record, err := a.verify(state, receipt, true)
	if err == nil || record.ErrorKind != "checksum" {
		t.Fatal(record, err)
	}
	var stored ArchiveVerification
	if err = a.dir.ReadJSON("control/verification/"+record.ID+".json", &stored); err != nil || stored.ErrorKind != "checksum" {
		t.Fatal(stored, err)
	}
	control, err := a.checkpoint()
	if err != nil || len(control.Pins) != 0 {
		t.Fatal("failed check granted a pin", err)
	}
}

func TestBackupReviewVerificationRecoveryAndOrdering(t *testing.T) {
	a, s, c, r := healthyWorkerFixture(t)
	record := ArchiveVerification{Schema: 1, ID: testutil.ID(t, "n"), ArchiveID: a.identity.ID, BackupID: r.BackupID, SHA256: r.Header.SHA256, ControlRevision: 100, StartedAt: time.Now().UTC(), ErrorKind: "integrity"}
	testutil.Must(t, a.dir.WriteJSON("control/verification/"+record.ID+".json", record, false))
	// No local acknowledgement survived. The archive must restore the failure.
	testutil.Must(t, a.syncVerificationStatuses(s))
	summary, healthy, err := evaluateWorkerHealth(c, s, time.Now().UTC())
	testutil.Must(t, err)
	if healthy || !strings.Contains(summary, "known-corrupt") {
		t.Fatal(summary)
	}
	for i, kind := range []string{"execution", "checksum", ""} {
		later := record
		later.ID = testutil.ID(t, "n")
		later.ControlRevision += uint64(i + 1)
		later.ErrorKind = kind
		later.Validation = protocol.SnapshotValidation{Check: "quick_check", Result: "ok"}
		testutil.Must(t, s.rememberVerification(later))
	}
	var status BackupVerificationStatus
	testutil.Must(t, s.get("verification/"+r.BackupID, &status))
	if status.Definitive == nil || status.Definitive.ErrorKind != "integrity" {
		t.Fatalf("lost integrity failure: %+v", status)
	}
	// Completion of an older check cannot clear a newer failure.
	stale := record
	stale.ID = testutil.ID(t, "n")
	stale.ControlRevision--
	stale.ErrorKind = ""
	stale.Validation = protocol.SnapshotValidation{Check: "integrity_check", Result: "ok"}
	testutil.Must(t, s.rememberVerification(stale))
	_, healthy, err = evaluateWorkerHealth(c, s, time.Now().UTC())
	testutil.Must(t, err)
	if healthy {
		t.Fatal("stale result cleared failure")
	}
	full := stale
	full.ID = testutil.ID(t, "n")
	full.ControlRevision = 104
	testutil.Must(t, s.rememberVerification(full))
	_, healthy, err = evaluateWorkerHealth(c, s, time.Now().UTC())
	testutil.Must(t, err)
	if !healthy {
		t.Fatal("successful full recheck did not clear failure")
	}
	// Historical receipts stay unchanged, while listing exposes subsequent evidence.
	list, err := a.scan(100)
	testutil.Must(t, err)
	if !list.Complete || list.Verification[r.BackupID].Definitive.ErrorKind != "integrity" || list.Entries[0].Validation.Result != "ok" {
		t.Fatalf("bad historical listing: %+v", list)
	}
	filtered, err := a.list(ArchiveFilter{Instance: "other-service"}, 1, "")
	testutil.Must(t, err)
	if len(filtered.Entries) != 0 || len(filtered.Verification) != 0 {
		t.Fatal("filtered listing exposed unrelated verification history")
	}
}

func TestBackupReviewCorruptionFailsClosedWhenCheckpointBusy(t *testing.T) {
	a, s, c, r := healthyWorkerFixture(t)
	file, err := a.dir.Open(filepath.Join(receiptPath(r), "database.db"), unix.O_WRONLY, 0)
	testutil.Must(t, err)
	_, err = file.WriteAt([]byte("bad"), 100)
	testutil.Must(t, err)
	testutil.Must(t, file.Sync())
	file.Close()
	lock, err := a.dir.Lock(".locks/control.lock", true, false)
	testutil.Must(t, err)
	defer lock.Close()
	if _, err = a.verify(s, r, true); err == nil {
		t.Fatal("corruption accepted")
	}
	summary, healthy, err := evaluateWorkerHealth(c, s, time.Now().UTC())
	testutil.Must(t, err)
	if healthy || !strings.Contains(summary, "known-corrupt") {
		t.Fatalf("failed checkpoint left corruption green: %s", summary)
	}
}
