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
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/testutil"
)

func seedPipelineDailyHistory(t *testing.T, f *pipelineFixture, periods int) {
	t.Helper()
	current := "daily/" + f.binding.Producer.ID + "/" + f.now.Format("2006-01-02")
	ledgerTx(t, f.state, func(tx *sql.Tx) error {
		for i := periods; i >= 0; i-- {
			at := f.now.AddDate(0, 0, -i)
			id := "daily/" + f.binding.Producer.ID + "/" + at.Format("2006-01-02")
			if err := enqueueNotification(tx, notification.NotificationEvent{ID: id, Kind: "daily", Message: "Historical report"}, at); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("UPDATE outbox SET covered_by=? WHERE kind='daily' AND id<>?", current, current); err != nil {
			return err
		}
		return enqueueNotification(tx, notification.NotificationEvent{ID: "catchup/long-outage", Kind: "delivery-catchup", Message: "Historical catchup"}, f.now.Add(-time.Hour))
	})
	messages := f.freeze(t, 1, 1)
	if len(messages) != 1 || messages[0].ReportType != "daily" {
		t.Fatal("catchup overtook current daily report", messages)
	}
	f.transfer(t, f.client)
	f.accept(t, messages[0])
	f.transfer(t, f.client)
}

func assertPipelineOutage(t *testing.T, f *pipelineFixture, settled bool) {
	t.Helper()
	var disposition string
	testutil.Must(t, f.state.db.QueryRow("SELECT disposition FROM notification_obligations WHERE id='long-outage'").Scan(&disposition))
	var accepted sql.NullString
	testutil.Must(t, f.state.db.QueryRow("SELECT accepted FROM outbox WHERE id='catchup/long-outage'").Scan(&accepted))
	if (disposition == "satisfied") != settled || accepted.Valid != settled {
		t.Fatalf("outage disposition=%s, source acceptance=%v; expected settled=%v", disposition, accepted.Valid, settled)
	}
}

func pipelineMembers(t *testing.T, f *pipelineFixture, m notify.Message) map[string]string {
	t.Helper()
	rows, err := f.state.db.Query("SELECT o.id,o.kind FROM notification_members m JOIN notification_obligations o ON o.id=m.obligation WHERE m.event=?", m.ID)
	testutil.Must(t, err)
	defer rows.Close()
	members := map[string]string{}
	for rows.Next() {
		var id, kind string
		testutil.Must(t, rows.Scan(&id, &kind))
		members[id] = kind
	}
	testutil.Must(t, rows.Err())
	return members
}

func TestBackupPipelineLargeCatchupHistory(t *testing.T) {
	for _, periods := range []int{0, 127, 128, 255, 256, 300} {
		t.Run(fmt.Sprint(periods), func(t *testing.T) {
			f := newPipelineFixture(t)
			seedPipelineDailyHistory(t, f, periods)
			seen := map[string]bool{}
			pages := (periods + 1 + 127) / 128
			for page := 0; page < pages; page++ {
				messages := f.freeze(t, uint64(page+2), 1)
				if len(messages) != 1 {
					t.Fatal("expected one bounded page", messages)
				}
				m := messages[0]
				if m.OutageID != "long-outage" || m.ReportRevision != uint64(page+1) {
					t.Fatal("page changed outage or revision", m)
				}
				members := pipelineMembers(t, f, m)
				final := members["long-outage"] == "catchup"
				if len(members) > 128 || len(members) == 0 || final != (page == pages-1) {
					t.Fatal("invalid page membership", len(members), final)
				}
				if final != strings.Contains(m.Text, "Final delivery-history") {
					t.Fatal("message misstates completion", m.Text)
				}
				for id, kind := range members {
					if kind == "daily" {
						if seen[id] {
							t.Fatal("period repeated", id)
						}
						seen[id] = true
					}
				}
				assertPipelineOutage(t, f, false)
				if got := f.freeze(t, uint64(page+2), 1); len(got) != 0 {
					t.Fatal("frozen page duplicated")
				}
				f.transfer(t, f.client)
				f.accept(t, m)
				f.transfer(t, f.client)
				assertPipelineOutage(t, f, final)
			}
			if len(seen) != periods {
				t.Fatal("history was truncated", len(seen), periods)
			}
			var open, acceptedHistory int
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM notification_obligations WHERE disposition='open'").Scan(&open))
			testutil.Must(t, f.state.db.QueryRow("SELECT count(*) FROM outbox WHERE kind='daily' AND covered_by IS NOT NULL AND accepted IS NOT NULL").Scan(&acceptedHistory))
			if open != 0 || acceptedHistory != 0 {
				t.Fatal("incorrect coverage", open, acceptedHistory)
			}
			if got := f.freeze(t, uint64(pages+2), 1); len(got) != 0 {
				t.Fatal("completed history replayed")
			}
		})
	}
}

func TestBackupPipelinePartialCatchupRestartAndReceiptLoss(t *testing.T) {
	f := newPipelineFixture(t)
	seedPipelineDailyHistory(t, f, 128)
	first := f.freeze(t, 2, 1)[0]
	lost := &losePipelineResponse{api: f.client, operation: "message.enqueue"}
	if err := transferBackupNotifications(context.Background(), f.state, lost); err == nil || !lost.lost {
		t.Fatal("loss not injected", err)
	}
	testutil.Must(t, f.state.Close())
	reopened, err := openBackupState(f.state.directory, false)
	testutil.Must(t, err)
	f.state = reopened
	t.Cleanup(func() { reopened.Close() })
	f.transfer(t, f.client)
	if got := f.freeze(t, 3, 1); len(got) != 0 {
		t.Fatal("restart duplicated partial page")
	}
	f.accept(t, first)
	lost = &losePipelineResponse{api: f.client, operation: "receipt.ack", skip: 1}
	if err := transferBackupNotifications(context.Background(), f.state, lost); err == nil || !lost.lost {
		t.Fatal("ack loss not injected", err)
	}
	assertPipelineOutage(t, f, false)
	f.transfer(t, f.client)
	// A new failure between pages blocks the final recovery summary.
	f.observe(t, "a", "new failure during reconciliation", f.now)
	alerts := f.freeze(t, 4, 2)
	if len(alerts) != 1 || alerts[0].Kind != "failure" {
		t.Fatal("final catchup overtook new failure", alerts)
	}
	f.transfer(t, f.client)
	f.accept(t, alerts[0])
	f.transfer(t, f.client)
	final := f.freeze(t, 5, 2)
	if len(final) != 1 || len(pipelineMembers(t, f, final[0])) != 1 {
		t.Fatal("expected final outage-only page", final)
	}
	f.transfer(t, f.client)
	f.accept(t, final[0])
	f.transfer(t, f.client)
	assertPipelineOutage(t, f, true)
}

func TestBackupPipelinePartialCatchupIneligibleAcceptanceNeedsSamePeriods(t *testing.T) {
	f := newPipelineFixture(t)
	seedPipelineDailyHistory(t, f, 128)
	first := f.freeze(t, 2, 1)[0]
	original := pipelineMembers(t, f, first)
	f.transfer(t, f.client)
	attempt, ok, err := f.gateway.Grant(context.Background(), f.binding.Producer, first.ID)
	testutil.Must(t, err)
	if !ok {
		t.Fatal("partial page was not granted")
	}
	// Supersede while an attempt is outstanding, then record its actual late
	// acceptance. This must not advance the history cursor or close the outage.
	f.freeze(t, 3, 2)
	f.transfer(t, f.client)
	testutil.Must(t, f.gateway.Finish(context.Background(), attempt, worker.Result{Accepted: true}))
	f.transfer(t, f.client)
	assertPipelineOutage(t, f, false)
	replacements := f.freeze(t, 4, 2)
	if len(replacements) != 1 {
		t.Fatal(replacements)
	}
	replacement := replacements[0]
	members := pipelineMembers(t, f, replacement)
	if len(members) != len(original) {
		t.Fatal("replacement lost periods")
	}
	for id, kind := range original {
		if members[id] != kind {
			t.Fatal("replacement changed coverage", id)
		}
	}
	if replacement.ID == first.ID || replacement.ReportRevision != 2 || replacement.OutageID != first.OutageID {
		t.Fatal("incorrect replacement identity")
	}
	f.transfer(t, f.client)
	f.accept(t, replacement)
	f.transfer(t, f.client)
	assertPipelineOutage(t, f, false)
	final := f.freeze(t, 5, 2)
	if len(final) != 1 || len(pipelineMembers(t, f, final[0])) != 1 {
		t.Fatal("partial acceptance did not advance to final page")
	}
	f.transfer(t, f.client)
	f.accept(t, final[0])
	f.transfer(t, f.client)
	assertPipelineOutage(t, f, true)
}
