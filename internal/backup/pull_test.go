//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupLocalPublicationRefusesRace(t *testing.T) {
	a, state, attempt, header, data := archiveFixture(t)
	receipt, err := a.receive(state, attempt, "vps", 1<<20, func(f *os.File) (protocol.SnapshotHeader, error) { _, err := f.Write(data); return header, err }, fixtureValidator)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "restore")
	raw, _ := json.Marshal(receipt)
	_, err = publishLocalPull(receipt, raw, output, func(f *os.File) error {
		if err := os.Mkdir(output, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(output, "user-file"), []byte("preserve"), 0600); err != nil {
			return err
		}
		_, err := f.Write(data)
		return err
	}, fixtureValidator)
	if err == nil {
		t.Fatal("concurrent output replaced")
	}
	if data, err := os.ReadFile(filepath.Join(output, "user-file")); err != nil || string(data) != "preserve" {
		t.Fatal("user output damaged", err)
	}
}

func TestBackupDownloadsRefuseKnownIntegrityFailure(t *testing.T) {
	a, s, attempt, header, data := archiveFixture(t)
	good := publishSnapshotFixture(t, a, s, header, data)
	path := filepath.Join(t.TempDir(), "bad.db")
	db, err := sql.Open("sqlite", path)
	testutil.Must(t, err)
	_, err = db.Exec(`CREATE TABLE t(a INTEGER,b INTEGER); INSERT INTO t VALUES(1,2); CREATE INDEX idx ON t(a); PRAGMA writable_schema=ON; UPDATE sqlite_schema SET sql='CREATE INDEX idx ON t(b)' WHERE name='idx'; PRAGMA writable_schema=OFF;`)
	testutil.Must(t, err)
	testutil.Must(t, db.Close())
	data, err = os.ReadFile(path)
	testutil.Must(t, err)
	header.Size, header.SHA256 = int64(len(data)), fmtDigest(data)
	header.Attempt.CaptureSequence++
	header.Attempt.Nonce = testutil.ID(t, "n")
	header.SnapshotID = testutil.ID(t, "b")
	bad := publishSnapshotFixture(t, a, s, header, data) // Real quick_check succeeds.
	f, err := os.Open(path)
	testutil.Must(t, err)
	_, err = validateArchivedFile(context.Background(), f, true)
	f.Close()
	if err == nil {
		t.Fatal("corrupt index fixture passed full integrity_check")
	}
	recordVerificationFixture(t, a, s, bad, "integrity", "")
	testutil.Must(t, a.updateControl(s, func(c *ArchiveControl) error {
		key := instanceKey(attempt.SourceID, attempt.Service)
		v := c.Instances[key]
		v.Generation = header.Generation
		c.Instances[key] = v
		return nil
	}))
	for _, later := range []string{"none", "execution", "quick_check"} {
		t.Run(later, func(t *testing.T) {
			if later == "execution" {
				recordVerificationFixture(t, a, s, bad, "execution", "")
			} else if later == "quick_check" {
				recordVerificationFixture(t, a, s, bad, "", "quick_check")
			}
			// Both explicit diagnostic selection and --latest retain the actual ID;
			// an older good snapshot must not be silently substituted.
			for _, latest := range []bool{false, true} {
				id := bad.BackupID
				if latest {
					id = ""
				}
				selected, err := a.selectReceipt(id, latest, ArchiveFilter{Source: attempt.SourceID, Instance: attempt.Service})
				testutil.Must(t, err)
				if selected.BackupID != bad.BackupID {
					t.Fatal("silently selected older snapshot")
				}
				output := filepath.Join(t.TempDir(), "download")
				_, err = a.pull(selected, output, func(*os.File) (protocol.SnapshotValidation, error) {
					t.Fatal("known failure reached download validation")
					return protocol.SnapshotValidation{}, nil
				})
				if err == nil || !strings.Contains(err.Error(), "known integrity failure") {
					t.Fatalf("pull: %v", err)
				}
				if _, err = os.Stat(output); !os.IsNotExist(err) {
					t.Fatalf("download published: %v", err)
				}
				var wire bytes.Buffer
				err = a.export(selected, testutil.ID(t, "n"), &wire)
				if err == nil || !strings.Contains(err.Error(), "known integrity failure") || wire.Len() != 0 {
					t.Fatalf("export: %v, bytes=%d", err, wire.Len())
				}
			}
		})
	}
	// A failure in another bundle must not prevent an explicit healthy pull.
	_, err = a.pull(good, filepath.Join(t.TempDir(), "good"), fixtureValidator)
	testutil.Must(t, err)
}

func TestBackupDownloadFullReverificationCanClearFailure(t *testing.T) {
	a, s, _, header, data := archiveFixture(t)
	r := publishSnapshotFixture(t, a, s, header, data)
	recordVerificationFixture(t, a, s, r, "integrity", "")
	f, err := os.Open(filepath.Join(a.dir.File().Name(), receiptPath(r), "database.db"))
	testutil.Must(t, err)
	_, err = validateArchivedFile(context.Background(), f, true)
	f.Close()
	testutil.Must(t, err)
	recordVerificationFixture(t, a, s, r, "", "integrity_check")
	_, err = a.pull(r, filepath.Join(t.TempDir(), "download"), fixtureValidator)
	testutil.Must(t, err)
	var wire bytes.Buffer
	testutil.Must(t, a.export(r, testutil.ID(t, "n"), &wire))
	if wire.Len() == 0 {
		t.Fatal("successful export produced no bytes")
	}
}

func TestBackupDownloadLeaseLeavesControlAvailable(t *testing.T) {
	a, s, _, header, data := archiveFixture(t)
	r := publishSnapshotFixture(t, a, s, header, data)
	_, err := a.pull(r, filepath.Join(t.TempDir(), "download"), func(f *os.File) (protocol.SnapshotValidation, error) {
		control, err := a.dir.Lock(".locks/control.lock", true, false)
		testutil.Must(t, err)
		control.Close()
		// A slow download excludes evidence-changing verification of this bundle,
		// while another reader and unrelated control mutations remain possible.
		other, shared, err := a.openBundle(r)
		testutil.Must(t, err)
		other.Close()
		shared.Close()
		dir, exclusive, err := a.openBundleLease(r, true)
		if err == nil {
			dir.Close()
			exclusive.Close()
			t.Fatal("download did not exclude exclusive verification")
		}
		return fixtureValidator(f)
	})
	testutil.Must(t, err)
	// Failure evidence is persisted before verification releases its bundle lease.
	f, err := os.OpenFile(filepath.Join(a.dir.File().Name(), receiptPath(r), "database.db"), os.O_WRONLY, 0)
	testutil.Must(t, err)
	_, err = f.WriteAt([]byte("bad!"), 0)
	testutil.Must(t, err)
	f.Close()
	record, err := a.verify(s, r, true)
	if err == nil || record.ErrorKind != "checksum" {
		t.Fatalf("verification failure not recorded: %+v %v", record, err)
	}
	var wire bytes.Buffer
	err = a.export(r, testutil.ID(t, "n"), &wire)
	if err == nil || !strings.Contains(err.Error(), "known checksum failure") || wire.Len() != 0 {
		t.Fatalf("failed verification exported: %v, bytes=%d", err, wire.Len())
	}
}
