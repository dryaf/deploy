package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestEnrolledRoutesResumePreservesHistory(t *testing.T) {
	for _, boundary := range []string{"intent", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			path, id, before := enrolledLimitsFixture(t)
			original, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
			must(t, err)
			s, err := OpenManagedVerification(path, before, id)
			must(t, err)
			_, err = s.db.Exec("INSERT INTO messages(producer,id,seq,hash,body,generation,route_revision,state) VALUES(?,?,?,?,?,?,?,?)", before.Producers[0].Producer.ID, "old-message", 1, protocol.Hash("old"), "retained terminal history", "staged", "original-route", "provider_accepted")
			must(t, err)
			must(t, s.Close())
			after := before
			after.Routes = map[string]Route{"alerts": {Revision: "next", Primary: "second-profile", Fallback: "first-profile"}}
			must(t, PreflightEnrolledRoutes(ctx, path, id, before, after))
			op, _ := safefs.NewID("n")
			stop := errors.New("lost route reply")
			receipt, err := migrateEnrolledPolicy(ctx, path, id, before, after, op, 3, func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if boundary != "complete" {
				for _, c := range []Config{before, after} {
					if s, e := OpenManagedVerification(path, c, id); e == nil {
						s.Close()
						t.Fatal("pending route admitted runtime")
					}
				}
			}
			again, err := MigrateEnrolledRoutes(ctx, path, id, before, after, op)
			must(t, err)
			if protocol.Hash(receipt) != protocol.Hash(again) {
				t.Fatal("changed receipt")
			}
			if s, e := OpenManagedVerification(path, before, id); e == nil {
				s.Close()
				t.Fatal("old routes admitted")
			}
			budget := after
			budget.MaxMessages++
			op, _ = safefs.NewID("n")
			_, err = MigrateEnrolledLimits(ctx, path, id, after, budget, op)
			must(t, err)
			timing := budget
			timing.Producers = append([]Enrollment(nil), budget.Producers...)
			timing.Producers[0].MaxSilenceSeconds++
			op, _ = safefs.NewID("n")
			_, err = MigrateEnrolledFreshness(ctx, path, id, budget, timing, op)
			must(t, err)
			final := timing
			final.Routes = before.Routes
			op, _ = safefs.NewID("n")
			_, err = MigrateEnrolledRoutes(ctx, path, id, timing, final, op)
			must(t, err)
			s, err = OpenManagedVerification(path, final, id)
			must(t, err)
			var body, revision, state string
			must(t, s.db.QueryRow("SELECT body,route_revision,state FROM messages WHERE id='old-message'").Scan(&body, &revision, &state))
			if body != "retained terminal history" || revision != "original-route" || state != "provider_accepted" {
				t.Fatal("rewrote historical message")
			}
			must(t, s.Close())
			retained, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
			must(t, err)
			if string(original) != string(retained) {
				t.Fatal("rewrote enrollment")
			}
		})
	}
}

func TestEnrolledRoutesRefusePendingMessagesWithoutIntent(t *testing.T) {
	for _, state := range []string{"waiting_status", "queued", "retry", "sending"} {
		t.Run(state, func(t *testing.T) {
			path, id, before := enrolledLimitsFixture(t)
			s, err := OpenManagedVerification(path, before, id)
			must(t, err)
			_, err = s.db.Exec("INSERT INTO messages(producer,id,seq,hash,body,generation,route_revision,state) VALUES(?,?,?,?,?,?,?,?)", before.Producers[0].Producer.ID, "pending", 1, protocol.Hash("pending"), "must not retarget", "staged", "original", state)
			must(t, err)
			must(t, s.Close())
			after := before
			after.Routes = map[string]Route{"alerts": {Revision: "changed", Primary: "other"}}
			op, _ := safefs.NewID("n")
			if err = PreflightEnrolledRoutes(context.Background(), path, id, before, after); err == nil {
				t.Fatal("pending message passed review")
			}
			if _, err = MigrateEnrolledRoutes(context.Background(), path, id, before, after, op); err == nil {
				t.Fatal("pending message migrated")
			}
			if err = PreflightEnrolledProviders(context.Background(), path, id, before, before); err == nil {
				t.Fatal("same-destination token change accepted pending work")
			}
			if _, err = MigrateEnrolledProviders(context.Background(), path, id, before, before, op); err == nil {
				t.Fatal("same-destination token change migrated pending work")
			}
			if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
				t.Fatal("refusal retained intent", err)
			}
			s, err = OpenManagedVerification(path, before, id)
			must(t, err)
			defer s.Close()
			var body, route, got string
			must(t, s.db.QueryRow("SELECT body,route_revision,state FROM messages WHERE id='pending'").Scan(&body, &route, &got))
			if body != "must not retarget" || route != "original" || got != state {
				t.Fatal("refusal changed queued work")
			}
		})
	}
}
