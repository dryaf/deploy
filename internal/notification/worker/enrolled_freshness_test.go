package worker

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"testing"
	"time"
)

func TestEnrolledFreshnessPreservesObservationsAcrossResume(t *testing.T) {
	for _, boundary := range []string{"intent", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			path, id, before := enrolledLimitsFixture(t)
			s, err := OpenManagedVerification(path, before, id)
			must(t, err)
			observed := time.Now().UTC().Add(-24 * time.Hour)
			snapshot := protocol.Snapshot{Producer: before.Producers[0].Producer, Sequence: 1, BasisRevision: 1, ConfigRevision: "original", EvaluatedAt: observed}
			raw, err := json.Marshal(snapshot)
			must(t, err)
			_, err = s.db.Exec("INSERT INTO status VALUES(?,?,?,?,?,?,?)", snapshot.Producer.ID, 1, 1, protocol.Hash(snapshot), string(raw), observed.UnixNano(), "original-session")
			must(t, err)
			must(t, s.Close())
			after := before
			after.Producers = append([]Enrollment(nil), before.Producers...)
			after.Producers[0].MaxSilenceSeconds = 300
			after.Producers[0].MaxEvidenceSeconds = 172800
			op, _ := safefs.NewID("n")
			stop := errors.New("lost freshness reply")
			receipt, err := migrateEnrolledPolicy(ctx, path, id, before, after, op, 2, func(at string) error {
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
						t.Fatal("pending timing update admitted runtime")
					}
				}
			}
			again, err := MigrateEnrolledFreshness(ctx, path, id, before, after, op)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(receipt) {
				t.Fatal("changed timing receipt")
			}
			budget := after
			budget.MaxMessages++
			next, _ := safefs.NewID("n")
			_, err = MigrateEnrolledLimits(ctx, path, id, after, budget, next)
			must(t, err)
			final := budget
			final.Producers = append([]Enrollment(nil), budget.Producers...)
			final.Producers[0].MaxSilenceSeconds = 180
			next, _ = safefs.NewID("n")
			_, err = MigrateEnrolledFreshness(ctx, path, id, budget, final, next)
			must(t, err)
			s, err = OpenManagedVerification(path, final, id)
			must(t, err)
			var body, session string
			var received int64
			must(t, s.db.QueryRow("SELECT body,received,session FROM status WHERE producer=?", snapshot.Producer.ID).Scan(&body, &received, &session))
			if body != string(raw) || received != observed.UnixNano() || session != "original-session" {
				t.Fatal("migration renewed prior observations")
			}
			must(t, s.Close())
			if s, err = OpenManagedVerification(path, budget, id); err == nil {
				s.Close()
				t.Fatal("prior timing accepted")
			}
		})
	}
}
func TestEnrolledFreshnessRejectsOtherProducerChanges(t *testing.T) {
	before := bootstrapConsumerConfig(bootstrapConfig())
	for _, kind := range []string{"route", "quota", "required", "health", "identity", "global"} {
		t.Run(kind, func(t *testing.T) {
			after := before
			after.Producers = append([]Enrollment(nil), before.Producers...)
			after.Producers[0].MaxSilenceSeconds++
			switch kind {
			case "route":
				after.Producers[0].Routes = nil
			case "quota":
				after.Producers[0].MaxMessages++
			case "required":
				after.Producers[0].Required = !after.Producers[0].Required
			case "health":
				after.Producers[0].PolicyHealth = !after.Producers[0].PolicyHealth
			case "identity":
				after.Producers[0].Producer.Epoch = "other"
			case "global":
				after.MaxMessages++
			}
			if err := ValidateEnrolledFreshnessChange(before, after); err == nil {
				t.Fatal("accepted unrelated change", kind)
			}
		})
	}
}
