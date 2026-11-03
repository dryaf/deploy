//go:build linux

package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupRegressionLatestCannotForgetMissingSnapshot(t *testing.T) {
	a, s, attempt, header, data := archiveFixture(t)
	old := publishSnapshotFixture(t, a, s, header, data)
	header.Attempt.CaptureSequence++
	header.Attempt.Nonce = testutil.ID(t, "n")
	header.Attempt.RequestedAt = time.Now().UTC()
	header.SnapshotID = testutil.ID(t, "b")
	newest := publishSnapshotFixture(t, a, s, header, data)
	testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error {
		key := instanceKey(attempt.SourceID, attempt.Service)
		v := c.Instances[key]
		v.Generation = header.Generation
		v.LastSuccess = header.Attempt.RequestedAt
		c.Instances[key] = v
		return nil
	}))
	testutil.Must(t, os.RemoveAll(filepath.Join(a.dir.File().Name(), receiptPath(newest))))
	for _, stage := range []string{"before-reconciliation", "after-reconciliation", "catalog-upgrade"} {
		if stage == "catalog-upgrade" {
			testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error { c.LatestSnapshots = nil; return nil }))
		}
		if stage != "before-reconciliation" {
			testutil.Must(t, a.reconcileWork(s))
		}
		// This reader never receives the local state handle.
		reader, err := openBackupArchive(a.dir.File().Name())
		testutil.Must(t, err)
		for _, generation := range []string{"", header.Generation} {
			for _, source := range []string{attempt.SourceID, "source"} {
				_, err := reader.selectReceipt("", true, ArchiveFilter{Source: source, Instance: attempt.Service, Generation: generation})
				if err == nil {
					t.Fatalf("%s silently substituted an older backup", stage)
				}
			}
		}
		selected, err := reader.selectReceipt(old.BackupID, false, ArchiveFilter{})
		testutil.Must(t, err)
		_, err = reader.pull(selected, filepath.Join(t.TempDir(), "download"), fixtureValidator)
		testutil.Must(t, err)
		reader.Close()
	}
	// A successful newer publication resolves the uncertainty.
	header.Attempt.CaptureSequence++
	header.Attempt.Nonce = testutil.ID(t, "n")
	header.SnapshotID = testutil.ID(t, "b")
	replacement := publishSnapshotFixture(t, a, s, header, data)
	selected, err := a.selectReceipt("", true, ArchiveFilter{Source: attempt.SourceID, Instance: attempt.Service})
	testutil.Must(t, err)
	if selected.BackupID != replacement.BackupID {
		t.Fatal("new successful publication did not become latest")
	}

}

func TestBackupLatestInterruptedPublication(t *testing.T) {
	a, s, attempt, header, data := archiveFixture(t)
	old := publishSnapshotFixture(t, a, s, header, data)
	header.Attempt.CaptureSequence++
	header.Attempt.Nonce = testutil.ID(t, "n")
	header.SnapshotID = testutil.ID(t, "b")
	testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error {
		key := instanceKey(attempt.SourceID, attempt.Service)
		v := c.Instances[key]
		v.CaptureSequence = header.Attempt.CaptureSequence
		v.Generation = header.Generation
		c.Instances[key] = v
		return nil
	}))
	var gate *safefs.Lock
	_, err := a.receive(s, header.Attempt, "source", 1<<20, func(f *os.File) (protocol.SnapshotHeader, error) { _, e := f.Write(data); return header, e }, func(f *os.File) (protocol.SnapshotValidation, error) {
		var e error
		gate, e = a.dir.Lock(".locks/index.lock", true, false)
		testutil.Must(t, e)
		return fixtureValidator(f)
	})
	if gate != nil {
		gate.Close()
	}
	if err == nil {
		t.Fatal("publication ignored conflicting index lease")
	}
	testutil.Must(t, a.reconcileWork(s))
	if _, err = a.selectReceipt("", true, ArchiveFilter{Source: attempt.SourceID, Instance: attempt.Service}); err == nil {
		t.Fatal("uncertain publication silently selected an older backup")
	}
	if _, err = a.selectReceipt(old.BackupID, false, ArchiveFilter{}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupLatestReconstructedCatalogCannotInventEvidence(t *testing.T) {
	a, s, attempt, header, data := archiveFixture(t)
	r := publishSnapshotFixture(t, a, s, header, data)
	testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error { c.LatestSnapshots = nil; return nil }))
	control, err := a.checkpoint()
	testutil.Must(t, err)
	testutil.Must(t, s.put("recovered_control", control))
	testutil.Must(t, a.reconcileWork(s))
	if _, err = a.selectReceipt("", true, ArchiveFilter{Source: attempt.SourceID, Instance: attempt.Service, Generation: header.Generation}); err == nil {
		t.Fatal("reconstructed legacy catalog manufactured latest certainty")
	}
	_, err = a.selectReceipt(r.BackupID, false, ArchiveFilter{})
	testutil.Must(t, err)
}
