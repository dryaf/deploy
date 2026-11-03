//go:build linux

package backup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupAcceptsSmallSQLitePages(t *testing.T) {
	a, state, attempt, header, _ := archiveFixture(t)
	path := filepath.Join(t.TempDir(), "small.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("PRAGMA page_size=512; CREATE TABLE item(id INTEGER PRIMARY KEY); INSERT INTO item VALUES(1)")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) >= 4096 || len(data) < 512 {
		t.Fatalf("small-page fixture: size=%d error=%v", len(data), err)
	}
	header.Size, header.SHA256 = int64(len(data)), fmtDigest(data)
	if err = protocol.ValidateSnapshotHeader(header, attempt, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err = a.receive(state, attempt, "source", 1<<20, func(f *os.File) (protocol.SnapshotHeader, error) {
		_, err := f.Write(data)
		return header, err
	}, func(f *os.File) (protocol.SnapshotValidation, error) {
		return validateArchivedFile(context.Background(), f, false)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupArchivePublicationAndIndependentPull(t *testing.T) {
	a, state, attempt, header, data := archiveFixture(t)
	receipt, err := a.receive(state, attempt, "vps-main", 1<<20, func(file *os.File) (protocol.SnapshotHeader, error) { _, err := file.Write(data); return header, err }, fixtureValidator)
	if err != nil {
		t.Fatal(err)
	}
	list, err := a.scan(100)
	if err != nil || !list.Complete || len(list.Entries) != 1 || list.Incoming != 0 {
		t.Fatal(list, err)
	}
	output := filepath.Join(t.TempDir(), "restore")
	result, err := a.pull(receipt, output, fixtureValidator)
	if err != nil || result != output {
		t.Fatal(result, err)
	}
	if testutil.DatabaseValue(t, filepath.Join(output, "database.db")) != "archived" {
		t.Fatal("wrong data")
	}
	if _, err = a.pull(receipt, output, fixtureValidator); err == nil {
		t.Fatal("existing output replaced")
	}
	// Source and state are unnecessary for historical list/pull.
	if err = state.Close(); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(t.TempDir(), "restore")
	if _, err = a.pull(receipt, second, fixtureValidator); err != nil {
		t.Fatal("read depended on state/source", err)
	}
	if err = a.dir.WriteJSON("unused.json", map[string]string{"x": "y"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err = a.selectReceipt(receipt.BackupID, false, ArchiveFilter{}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupArchiveRejectsCorruptionAndPathAliases(t *testing.T) {
	a, state, attempt, header, data := archiveFixture(t)
	corrupted := append([]byte{}, data...)
	corrupted[200] ^= 1
	if _, err := a.receive(state, attempt, "vps-main", 1<<20, func(f *os.File) (protocol.SnapshotHeader, error) { _, err := f.Write(corrupted); return header, err }, fixtureValidator); err == nil {
		t.Fatal("corrupt data published")
	}
	list, err := a.scan(100)
	if err != nil || len(list.Entries) != 0 || list.Incoming != 0 {
		t.Fatal(list, err)
	}
	target := filepath.Join(t.TempDir(), "foreign")
	if err = os.WriteFile(target, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(target, filepath.Join(a.dir.File().Name(), "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err = a.dir.Open("alias", os.O_RDONLY, 0); err == nil {
		t.Fatal("symlink followed")
	}
}

func TestBackupControlRejectsStaleCheckpoint(t *testing.T) {
	a, state, _, _, _ := archiveFixture(t)
	var old ArchiveControl
	if err := state.get("control", &old); err != nil {
		t.Fatal(err)
	}
	if err := a.updateControl(state, func(c *ArchiveControl) error { c.RetiredSources["source"] = "retired"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := state.put("control", old); err != nil {
		t.Fatal(err)
	}
	if err := a.commitControl(state); err == nil {
		t.Fatal("stale control overwrote newer checkpoint")
	}
	actual, err := a.checkpoint()
	if err != nil || actual.RetiredSources["source"] != "retired" {
		t.Fatal(actual, err)
	}
}
