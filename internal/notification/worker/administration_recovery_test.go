package worker

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestMessageAdministrationRecoveryArchivesLatestDecisionsWithoutReplaying(t *testing.T) {
	s, now, path, d := administrationFixture(t)
	ctx := context.Background()
	before := s.Identity()
	first, err := s.DecideMessage(ctx, d)
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	// This second decision is newer than the snapshot. Its audit must not vanish
	// even though restore reclassifies the snapshot's queued event as ambiguous.
	j, granted, err := s.Grant(ctx, producer, d.Review.Job.Message.ID)
	must(t, err)
	if !granted {
		t.Fatal("fixture grant unavailable")
	}
	must(t, s.Finish(ctx, j, Result{Ambiguous: true}))
	v, err := s.ReviewMessage(ctx, producer, j.Message.ID)
	must(t, err)
	second, err := s.DecideMessage(ctx, MessageDecision{Version: 1, Review: v, Action: "cancel", Reason: "Second explicit decision after snapshot"})
	must(t, err)
	c := s.config
	must(t, s.Close())
	journal, err := prepareSnapshotRecovery(ctx, path, artifact, c, nil)
	must(t, err)
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	must(t, err)
	must(t, verifyAdministrativeHistory(ctx, r.db, journal.After))
	var count int
	must(t, r.db.QueryRow("SELECT count(*) FROM metadata WHERE key LIKE 'message-decision-v1:%'").Scan(&count))
	if count != 0 {
		t.Fatal("old decision remains active")
	}
	must(t, r.db.QueryRow("SELECT count(*) FROM metadata WHERE key LIKE 'message-decision-archive-v1:%'").Scan(&count))
	if count != 2 {
		t.Fatal("latest audit omitted", count)
	}
	borrowed := &Store{db: r.db, dir: r.dir, identity: journal.After, config: c, now: func() time.Time { return *now }}
	a := messageAdministrator{s: borrowed}
	for _, record := range []MessageDecisionRecord{first, second} {
		got, err := a.status(ctx, record.Decision.Review.ID)
		must(t, err)
		if protocol.Hash(got) != protocol.Hash(record) || got.Decision.Review.Identity != before {
			t.Fatal("old evidence relabeled")
		}
		if _, err = borrowed.DecideMessage(ctx, record.Decision); err == nil {
			t.Fatal("old administrative instruction regained authority")
		}
	}
	var state string
	must(t, r.db.QueryRow("SELECT state FROM messages WHERE id=?", j.Message.ID).Scan(&state))
	if state != "ambiguous" {
		t.Fatal("historical decision applied during restore", state)
	}
	must(t, r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&count))
	if count != 0 {
		t.Fatal("old receipts entered new acknowledgement stream")
	}
	must(t, r.Close())
	r, err = openRecoveryReconciler(ctx, path, c, nil)
	must(t, err)
	defer r.Close()
	_, err = r.db.Exec("DELETE FROM metadata WHERE key=?", archivedMessageDecisionKey(second))
	must(t, err)
	must(t, r.Close())
	if broken, e := openRecoveryReconciler(ctx, path, c, nil); e == nil {
		broken.Close()
		t.Fatal("lost committed audit silently reconstructed")
	}
}

func TestMessageAdministrationRecoveryRejectsMissingLatestAuditDirectory(t *testing.T) {
	s, _, path, d := administrationFixture(t)
	ctx := context.Background()
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err := s.Snapshot(ctx, artifact)
	must(t, err)
	_, err = s.DecideMessage(ctx, d)
	must(t, err)
	c := s.config
	must(t, s.Close())
	_, err = prepareSnapshotRecovery(ctx, path, artifact, c, nil)
	must(t, err)
	must(t, os.RemoveAll(filepath.Join(path, "administration")))
	if r, e := openRecoveryReconciler(ctx, path, c, nil); e == nil {
		r.Close()
		t.Fatal("lost decisions newer than the snapshot were silently omitted")
	}
}

func TestAdministrativeSnapshotRejectsTamperedReceipt(t *testing.T) {
	s, _, _, d := administrationFixture(t)
	_, err := s.DecideMessage(context.Background(), d)
	must(t, err)
	must(t, s.transaction(context.Background(), func(tx *sql.Tx) error {
		_, e := tx.Exec("UPDATE receipts SET body='{}' WHERE position=(SELECT max(position) FROM receipts)")
		return e
	}))
	if _, err = s.Snapshot(context.Background(), filepath.Join(t.TempDir(), "snapshot")); err == nil {
		t.Fatal("snapshot accepted corrupted administrative provenance")
	}
}
