package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func administrationFixture(t *testing.T) (*Store, *time.Time, string, MessageDecision) {
	t.Helper()
	s, now, path := fixture(t)
	m := report(*now, "administrative-message", 1, 1, 1)
	must(t, s.Replace(context.Background(), snapshot(*now, 1, 1)))
	_, err := s.Enqueue(context.Background(), m)
	must(t, err)
	j, granted, err := s.Grant(context.Background(), producer, m.ID)
	must(t, err)
	if !granted {
		t.Fatal("fixture did not grant")
	}
	must(t, s.Finish(context.Background(), j, Result{Ambiguous: true}))
	v, err := s.ReviewMessage(context.Background(), producer, m.ID)
	must(t, err)
	return s, now, path, MessageDecision{Version: 1, Review: v, Action: "retry", Reason: "Operator reviewed the uncertain response", AcceptDuplicateRisk: true}
}

func TestMessageAdministrationFreshReviewCannotReviveUnsafeDelivery(t *testing.T) {
	for _, scenario := range []string{"revoked", "draining", "route", "superseded", "expired"} {
		t.Run(scenario, func(t *testing.T) {
			s, now, _, d := administrationFixture(t)
			ctx := context.Background()
			switch scenario {
			case "revoked":
				must(t, s.Revoke(ctx, producer, d.Review.Job.Message.Generation))
			case "draining":
				must(t, s.ownership(producer, func(o *ownership) error { return s.commitOwnership(o, "drain", d.Review.Job.Message.Generation) }))
			case "route":
				route := s.config.Routes[d.Review.Job.Message.Route]
				route.Revision = "new-recipient"
				s.config.Routes[d.Review.Job.Message.Route] = route
			case "superseded":
				must(t, s.Replace(ctx, snapshot(*now, 2, 2)))
			case "expired":
				*now = now.Add(6 * time.Minute)
			}
			v, err := s.ReviewMessage(ctx, producer, d.Review.Job.Message.ID)
			if scenario == "superseded" {
				if err == nil {
					t.Fatal("superseded terminal event became reviewable")
				}
				return
			}
			must(t, err)
			d.Review = v
			if _, err = s.DecideMessage(ctx, d); err == nil {
				t.Fatal("fresh review bypassed delivery fence")
			}
			// Cancelling remains possible even when delivery is forbidden.
			d.Action, d.AcceptDuplicateRisk = "cancel", false
			_, err = s.DecideMessage(ctx, d)
			must(t, err)
		})
	}
}

func TestMessageAdministrationHistoryLimitDoesNotEvictEvidence(t *testing.T) {
	s, _, path, d := administrationFixture(t)
	// This test may fsync many files on a slow filesystem. Freeze both clocks;
	// review expiry is exercised independently above.
	a := messageAdministrator{s: s, clock: func() (string, float64, error) { return d.Review.BootID, d.Review.Uptime, nil }}
	must(t, s.dir.Mkdir("administration"))
	for i := 0; i < maxMessageDecisions; i++ {
		pending := d
		id, err := safefs.NewID("n")
		must(t, err)
		pending.Review.ID = id
		if i == 0 {
			d = pending
		}
		must(t, s.dir.WriteJSON(messageIntentFile(id), pending, false))
	}
	v, err := a.review(context.Background(), producer, d.Review.Job.Message.ID)
	must(t, err)
	newDecision := d
	newDecision.Review = v
	if _, err = a.decide(context.Background(), newDecision); err == nil || !strings.Contains(err.Error(), "history limit") {
		t.Fatal("new decision exceeded history limit", err)
	}
	// An existing uncommitted decision can finish at the limit.
	_, err = a.decide(context.Background(), d)
	must(t, err)
	if _, err = os.Stat(filepath.Join(path, messageResultFile(d.Review.ID))); err != nil {
		t.Fatal(err)
	}
}

func TestMessageAdministrationRetryIsAuditedAndNeverReplays(t *testing.T) {
	s, now, _, d := administrationFixture(t)
	ctx := context.Background()
	if _, err := s.DecideMessage(ctx, MessageDecision{Version: 1, Review: d.Review, Action: "retry", Reason: "No duplicate consent"}); err == nil {
		t.Fatal("ambiguous retry accepted without consent")
	}
	v, err := s.DecideMessage(ctx, d)
	must(t, err)
	if v.Receipt.State != "waiting_status" || v.Receipt.Acceptance != nil || v.Receipt.Position == 0 {
		t.Fatal(v)
	}
	j, err := s.Get(ctx, producer, d.Review.Job.Message.ID)
	must(t, err)
	if j.Tries != 1 || j.Attempt != "" || j.Provider != "" || protocol.Hash(j.Message) != protocol.Hash(d.Review.Job.Message) || j.RouteRevision != d.Review.Job.RouteRevision {
		t.Fatal("retry rewrote history/destination/payload", j)
	}
	j, granted, err := s.Grant(ctx, producer, j.Message.ID)
	must(t, err)
	if !granted || j.Tries != 2 {
		t.Fatal("explicit retry not subject to ordinary grants", j, granted)
	}
	must(t, s.Finish(ctx, j, Result{Accepted: true}))
	*now = now.Add(time.Hour)
	got, err := s.DecideMessage(ctx, d)
	must(t, err)
	if protocol.Hash(got) != protocol.Hash(v) {
		t.Fatal("idempotent replay changed completion")
	}
	j, err = s.Get(ctx, producer, j.Message.ID)
	must(t, err)
	if j.State != "provider_accepted" || j.Tries != 2 {
		t.Fatal("replay reset accepted event", j)
	}
	view, err := s.MessageDecision(ctx, d.Review.ID)
	must(t, err)
	if protocol.Hash(view) != protocol.Hash(v) {
		t.Fatal("audit differs")
	}
	changed := d
	changed.Reason = "Changed instruction"
	if _, err = s.DecideMessage(ctx, changed); err == nil {
		t.Fatal("instruction ID reused")
	}
}

func TestMessageAdministrationRejectsDriftAndCannotBypassFences(t *testing.T) {
	for _, scenario := range []string{"expired_review", "wall_rewind", "boot", "monotonic", "expired_message", "payload", "config", "route", "ownership", "wrong_epoch", "receipt_head", "inflight", "queued_retry"} {
		t.Run(scenario, func(t *testing.T) {
			s, now, _, d := administrationFixture(t)
			a := messageAdministrator{s: s, clock: process.HostClock}
			switch scenario {
			case "expired_review":
				*now = now.Add(6 * time.Minute)
			case "wall_rewind":
				*now = now.Add(-3 * time.Second)
			case "boot":
				a.clock = func() (string, float64, error) { return "changed-boot", d.Review.Uptime, nil }
			case "monotonic":
				a.clock = func() (string, float64, error) { return d.Review.BootID, d.Review.Uptime + 301, nil }
			case "payload":
				d.Review.Job.Message.Text = "different"
				d.Review.JobHash = protocol.Hash(d.Review.Job)
			case "config":
				s.config.MaxMessages++
			case "route":
				route := s.config.Routes[d.Review.Job.Message.Route]
				route.Revision = "changed"
				s.config.Routes[d.Review.Job.Message.Route] = route
			case "ownership":
				must(t, s.Revoke(context.Background(), producer, d.Review.Job.Message.Generation))
			case "wrong_epoch":
				d.Review.Identity.Recovery = "other"
			case "receipt_head":
				must(t, s.transaction(context.Background(), func(tx *sql.Tx) error {
					return s.receipt(tx, producer.ID, protocol.Receipt{EventID: d.Review.Job.Message.ID, Sequence: 1, State: "ambiguous", At: *now})
				}))
			case "expired_message":
				*now = now.Add(6 * time.Minute)
				v, err := s.ReviewMessage(context.Background(), producer, d.Review.Job.Message.ID)
				must(t, err)
				d.Review = v
			case "inflight":
				_, err := s.db.Exec("UPDATE messages SET attempt='n-00000000000000000000000000000001',state='sending'")
				must(t, err)
			case "queued_retry":
				_, err := s.db.Exec("UPDATE messages SET state='queued'")
				must(t, err)
				v, err := s.ReviewMessage(context.Background(), producer, d.Review.Job.Message.ID)
				must(t, err)
				d.Review = v
			}
			if _, err := a.decide(context.Background(), d); err == nil {
				t.Fatal("unsafe decision accepted")
			}
			var n int
			must(t, s.db.QueryRow("SELECT count(*) FROM metadata WHERE key LIKE 'message-decision-v1:%'").Scan(&n))
			if n != 0 {
				t.Fatal("rejection persisted decision")
			}
		})
	}
}

func TestMessageAdministrationCancelPreservesUnsuccessfulReceiptAndConsumerFloors(t *testing.T) {
	s, _, _, d := administrationFixture(t)
	d.Action = "cancel"
	d.AcceptDuplicateRisk = false
	v, err := s.DecideMessage(context.Background(), d)
	must(t, err)
	if v.Receipt.State != "cancelled" || v.Receipt.Acceptance != nil {
		t.Fatal(v)
	}
	j, err := s.Get(context.Background(), producer, d.Review.Job.Message.ID)
	must(t, err)
	if j.State != "cancelled" || j.Tries != 1 || protocol.Hash(j.Message) != protocol.Hash(d.Review.Job.Message) {
		t.Fatal(j)
	}
	page, err := s.InspectReceipts(context.Background(), producer, 0, 128)
	must(t, err)
	if page.AcknowledgedThrough != 0 || page.ListedThrough != 0 {
		t.Fatal("administration advanced consumer receipt floors")
	}
	if _, err = s.ReviewMessage(context.Background(), producer, j.Message.ID); err == nil {
		t.Fatal("accepted terminal review")
	}
}

func TestMessageAdministrationPendingIntentRequiresExplicitFreshReplay(t *testing.T) {
	s, now, path, d := administrationFixture(t)
	ctx := context.Background()
	injected := errors.New("interrupted")
	a := messageAdministrator{s: s, clock: process.HostClock, fault: func(point string) error {
		if point == "administration_intent" {
			return injected
		}
		return nil
	}}
	if _, err := a.decide(ctx, d); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	c := s.config
	must(t, s.Close())
	s, err := Open(path, c, false)
	must(t, err)
	defer s.Close()
	s.now = func() time.Time { return *now }
	j, err := s.Get(ctx, producer, d.Review.Job.Message.ID)
	must(t, err)
	if j.State != "ambiguous" || j.Tries != 1 {
		t.Fatal("startup applied file intent", j)
	}
	_, err = s.DecideMessage(ctx, d)
	must(t, err)
}

func TestMessageAdministrationStartupChecksIndependentEvidence(t *testing.T) {
	for _, damage := range []string{"missing_result", "missing_intent", "lost_sql", "changed_receipt", "changed_result"} {
		t.Run(damage, func(t *testing.T) {
			s, _, path, d := administrationFixture(t)
			_, err := s.DecideMessage(context.Background(), d)
			must(t, err)
			switch damage {
			case "missing_result":
				must(t, os.Remove(filepath.Join(path, messageResultFile(d.Review.ID))))
			case "missing_intent":
				must(t, os.Remove(filepath.Join(path, messageIntentFile(d.Review.ID))))
			case "lost_sql":
				_, err = s.db.Exec("DELETE FROM metadata WHERE key=?", messageDecisionKey(d.Review.ID))
				must(t, err)
			case "changed_receipt":
				_, err = s.db.Exec("UPDATE receipts SET body='{}'")
				must(t, err)
			case "changed_result":
				must(t, os.WriteFile(filepath.Join(path, messageResultFile(d.Review.ID)), []byte("{}"), 0600))
			}
			c := s.config
			must(t, s.Close())
			reopened, err := Open(path, c, false)
			if damage == "missing_result" {
				must(t, err)
				defer reopened.Close()
				if _, err = os.Stat(filepath.Join(path, messageResultFile(d.Review.ID))); err != nil {
					t.Fatal("completion not repaired", err)
				}
			} else if err == nil {
				reopened.Close()
				t.Fatal("damaged administrative evidence accepted")
			}
		})
	}
}

func TestMessageAdministrationStartupRejectsForeignPendingIntent(t *testing.T) {
	s, _, path, d := administrationFixture(t)
	must(t, s.dir.Mkdir("administration"))
	d.Review.Identity.Worker = "another-worker"
	must(t, s.dir.WriteJSON(messageIntentFile(d.Review.ID), d, false))
	c := s.config
	must(t, s.Close())
	if reopened, err := Open(path, c, false); err == nil {
		reopened.Close()
		t.Fatal("pending audit from another worker was accepted")
	}
}
