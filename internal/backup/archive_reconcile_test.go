//go:build linux

package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/testutil"
	"golang.org/x/sys/unix"
)

func TestBackupReconcileAbandonedIncomingAndPreserveForeignFiles(t *testing.T) {
	a, state, attempt, _, _ := archiveFixture(t)
	id := testutil.ID(t, "b")
	if err := state.recordAttempt(id, instanceKey(attempt.SourceID, attempt.Service), "receiving", attempt); err != nil {
		t.Fatal(err)
	}
	lock, err := a.dir.Lock(".locks/"+id+".lock", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.dir.Mkdir(".incoming/" + id); err != nil {
		t.Fatal(err)
	}
	dir, err := a.dir.Sub(".incoming/" + id)
	if err != nil {
		t.Fatal(err)
	}
	file, err := dir.Open("database.db", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.Write([]byte("partial"))
	file.Close()
	if err = a.reconcileWork(state); err == nil {
		t.Fatal("live lease did not block cleanup")
	}
	lock.Close()
	foreign, err := dir.Open("unrelated", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	foreign.Close()
	if err = a.reconcileWork(state); err == nil {
		t.Fatal("foreign work file deleted")
	}
	if err = unix.Unlinkat(int(dir.File().Fd()), "unrelated", 0); err != nil {
		t.Fatal(err)
	}
	dir.Close()
	if err = a.reconcileWork(state); err != nil {
		t.Fatal(err)
	}
	if _, err = a.dir.Sub(".incoming/" + id); !os.IsNotExist(err) {
		t.Fatal("abandoned spool remains", err)
	}
}

func TestBackupReconcileFinishesPartialTrash(t *testing.T) {
	a, state, attempt, header, data := archiveFixture(t)
	receipt, err := a.receive(state, attempt, "source", 1<<20, func(f *os.File) (protocol.SnapshotHeader, error) { _, err := f.Write(data); return header, err }, func(f *os.File) (protocol.SnapshotValidation, error) {
		return validateArchivedFile(context.Background(), f, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	control, err := a.checkpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.dir.Mkdir("control/deletions"); err != nil {
		t.Fatal(err)
	}
	if err = a.dir.WriteJSON("control/deletions/"+receipt.BackupID+".json", archiveDeletion{1, a.identity.ID, control.Revision, receipt, time.Now()}, false); err != nil {
		t.Fatal(err)
	}
	parent, err := a.dir.Sub(filepath.Dir(receiptPath(receipt)))
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	trash, err := a.dir.Sub(".trash")
	if err != nil {
		t.Fatal(err)
	}
	defer trash.Close()
	if err = parent.Rename(receipt.BackupID, trash, receipt.BackupID); err != nil {
		t.Fatal(err)
	}
	dir, err := trash.Sub(receipt.BackupID)
	if err != nil {
		t.Fatal(err)
	}
	if err = unix.Unlinkat(int(dir.File().Fd()), "database.db", 0); err != nil {
		t.Fatal(err)
	}
	dir.Close()
	if err = a.reconcileWork(state); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = state.db.QueryRow("SELECT count(*) FROM bundles WHERE id=?", receipt.BackupID).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestBackupInterruptedReceiptTempReconciles(t *testing.T) {
	a, s, attempt, _, _ := archiveFixture(t)
	id := testutil.ID(t, "b")
	testutil.Must(t, s.recordAttempt(id, instanceKey(attempt.SourceID, attempt.Service), "receiving", attempt))
	lease, err := a.dir.Lock(".locks/"+id+".lock", true, true)
	testutil.Must(t, err)
	lease.Close()
	testutil.Must(t, a.dir.Mkdir(".incoming/"+id))
	// WriteJSON uses this exact name format before the atomic rename.
	testutil.Must(t, a.dir.WriteJSON(".incoming/"+id+"/."+testutil.ID(t, "n")+".json", map[string]string{"partial": "receipt"}, false))
	if err = a.reconcileWork(s); err != nil {
		t.Fatalf("ordinary interrupted receipt cannot reconcile, blocking the next backup pass: %v", err)
	}
	if _, err = os.Stat(filepath.Join(a.dir.File().Name(), ".incoming", id)); !os.IsNotExist(err) {
		t.Fatalf("interrupted transfer directory remains: %v", err)
	}
}
