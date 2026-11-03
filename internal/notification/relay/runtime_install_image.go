package relay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func runtimePin(p interaction.SignalRecoveryCursor) inboxPin {
	return inboxPin{Identity: p.Handoff.Target.Identity, Account: p.Handoff.Target.Account, Baseline: protocol.Hash(p)}
}
func promoteRuntimeImage(ctx context.Context, d *safefs.Directory, p interaction.SignalRecoveryCursor, fault func(string) error) error {
	db, err := inboxArtifactDB(d, runtimeInstallImage, false)
	if err != nil {
		return err
	}
	defer db.Close()
	if err = verifySignalCursorCandidate(ctx, db, p.Handoff); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("CREATE TABLE signal_runtime(singleton INTEGER PRIMARY KEY CHECK(singleton=1),body TEXT NOT NULL); PRAGMA user_version=5;"); err != nil {
		return err
	}
	raw, _ := json.Marshal(p)
	if _, err = tx.Exec("INSERT INTO signal_runtime VALUES(1,?)", string(raw)); err != nil {
		return err
	}
	raw, _ = json.Marshal(runtimePin(p))
	if _, err = tx.Exec("UPDATE identity SET body=? WHERE singleton=1", string(raw)); err != nil {
		return err
	}
	if err = inboxRestoreBoundary(fault, "runtime_image_transaction"); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return db.Close()
}
func runtimeImageDigest(d *safefs.Directory) (hash string, size int64, err error) {
	f, err := inboxArtifactFile(d, runtimeInstallImage, 34<<20)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	size = info.Size()
	if err = f.Sync(); err != nil {
		return
	}
	hash, err = safefs.DigestFile(f)
	if err != nil {
		return
	}
	err = d.File().Sync()
	return
}
func verifyRuntimeImage(ctx context.Context, d *safefs.Directory, p SignalRuntimeInstallation) error {
	if err := verifyStandaloneInboxFile(d, runtimeInstallImage, p.DatabaseBytes, p.DatabaseHash); err != nil {
		return err
	}
	db, err := inboxArtifactDB(d, runtimeInstallImage, true)
	if err != nil {
		return err
	}
	defer db.Close()
	baseline, err := readInboxBaseline(db, runtimePin(p.Cursor))
	if err != nil {
		return err
	}
	if baseline == nil || protocol.Hash(*baseline) != protocol.Hash(p.Cursor) {
		return fmt.Errorf("Signal runtime image baseline changed")
	}
	var n int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'").Scan(&n); err != nil {
		return err
	}
	if n != 4 {
		return fmt.Errorf("Signal runtime image schema changed")
	}
	var ack, listed, seq uint64
	if err = db.QueryRowContext(ctx, "SELECT acked,listed,(SELECT seq FROM sqlite_sequence WHERE name='events') FROM identity WHERE singleton=1").Scan(&ack, &listed, &seq); err != nil {
		return err
	}
	if ack != 0 || listed != 0 || seq != p.Cursor.Handoff.Target.Position {
		return fmt.Errorf("Signal runtime image watermarks changed")
	}
	digest, err := manifestEventDigest(ctx, db, p.Cursor.Handoff.Summary.Manifest)
	if err != nil {
		return err
	}
	if digest != p.Cursor.Handoff.Summary.Manifest.Digest {
		return fmt.Errorf("Signal runtime image history changed")
	}
	var integrity string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("Signal runtime image integrity failure")
	}
	return nil
}
func inspectInstalledRuntime(ctx context.Context, d *safefs.Directory, p SignalRuntimeInstallation) error {
	if err := runtimeDatabaseFiles(d, "inbox.db"); err != nil {
		return err
	}
	var pin inboxPin
	if err := d.ReadJSON("identity.json", &pin); err != nil {
		return err
	}
	if pin != runtimePin(p.Cursor) {
		return fmt.Errorf("Signal installed identity changed")
	}
	var cursor interaction.SignalRecoveryCursor
	if err := d.ReadJSON(signalRuntimeFile, &cursor); err != nil {
		return err
	}
	if protocol.Hash(cursor) != protocol.Hash(p.Cursor) {
		return fmt.Errorf("Signal installed baseline changed")
	}
	db, err := inboxArtifactDB(d, "inbox.db", true)
	if err != nil {
		return err
	}
	defer db.Close()
	s := Inbox{dir: d, db: db, identity: pin.Identity, account: pin.Account, baseline: &p.Cursor}
	if err = s.checkBaseline(db); err != nil {
		return err
	}
	var integrity string
	if err = db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("Signal installed database integrity failure")
	}
	return s.verifyBaselineHistory(ctx)
}
