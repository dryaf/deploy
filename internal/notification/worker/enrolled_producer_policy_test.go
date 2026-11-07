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

func TestEnrolledProducerPolicyResumesWithoutReplacingIdentity(t *testing.T) {
	for _, boundary := range []string{"intent", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			path, id, before := enrolledLimitsFixture(t)
			after := before
			after.Producers = append([]Enrollment(nil), before.Producers...)
			after.Producers[0].Required = !before.Producers[0].Required
			after.Producers[0].PolicyHealth = !before.Producers[0].PolicyHealth
			after.Producers[0].InteractiveContext = true
			after.Producers[0].ContextKinds = []string{"source"}
			after.Producers[0].MaxMessages++
			op, _ := safefs.NewID("n")
			stop := errors.New("lost policy reply")
			original, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
			must(t, err)
			must(t, PreflightEnrolledProducerPolicy(context.Background(), path, id, before, after))
			receipt, err := migrateEnrolledPolicy(context.Background(), path, id, before, after, op, 4, func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if boundary != "complete" {
				if s, e := OpenManagedVerification(path, after, id); e == nil {
					s.Close()
					t.Fatal("pending policy admitted runtime")
				}
			}
			again, err := MigrateEnrolledProducerPolicy(context.Background(), path, id, before, after, op)
			must(t, err)
			if protocol.Hash(receipt) != protocol.Hash(again) {
				t.Fatal("replaced receipt")
			}
			if s, e := OpenManagedVerification(path, before, id); e == nil {
				s.Close()
				t.Fatal("old policy admitted")
			}
			s, err := OpenManagedVerification(path, after, id)
			must(t, err)
			must(t, s.RequireInactiveProducers())
			must(t, s.Close())
			retained, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
			must(t, err)
			if string(original) != string(retained) {
				t.Fatal("replaced enrollment")
			}
		})
	}
}
func TestEnrolledProducerPolicyPendingQueueRules(t *testing.T) {
	for _, kind := range []string{"quota", "required", "health", "context", "route", "smaller-quota"} {
		t.Run(kind, func(t *testing.T) {
			path, id, before := enrolledLimitsFixture(t)
			s, err := OpenManagedVerification(path, before, id)
			must(t, err)
			for seq, msg := range []string{"one", "two"} {
				_, err = s.db.Exec("INSERT INTO messages(producer,id,seq,hash,body,generation,route_revision,state) VALUES(?,?,?,?,?,?,?,?)", before.Producers[0].Producer.ID, msg, seq+1, protocol.Hash(msg), "pending body", "staged", "original", "queued")
				must(t, err)
			}
			must(t, s.Close())
			after := before
			after.Producers = append([]Enrollment(nil), before.Producers...)
			switch kind {
			case "quota":
				after.Producers[0].MaxMessages++
			case "smaller-quota":
				after.Producers[0].MaxMessages = 1
			case "required":
				after.Producers[0].Required = !before.Producers[0].Required
			case "health":
				after.Producers[0].PolicyHealth = !before.Producers[0].PolicyHealth
			case "context":
				after.Producers[0].InteractiveContext = true
				after.Producers[0].ContextKinds = []string{"source"}
			case "route":
				after.Producers[0].Routes = nil
			}
			op, _ := safefs.NewID("n")
			review := PreflightEnrolledProducerPolicy(context.Background(), path, id, before, after)
			_, err = MigrateEnrolledProducerPolicy(context.Background(), path, id, before, after, op)
			expected := before
			if kind == "quota" {
				must(t, review)
				must(t, err)
				expected = after
			} else {
				if review == nil || err == nil {
					t.Fatal("pending policy change accepted", review, err)
				}
				if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
					t.Fatal("refusal retained intent", err)
				}
			}
			s, err = OpenManagedVerification(path, expected, id)
			must(t, err)
			defer s.Close()
			var count int
			must(t, s.db.QueryRow("SELECT count(*) FROM messages WHERE body='pending body' AND route_revision='original' AND state='queued'").Scan(&count))
			if count != 2 {
				t.Fatal("rewrote pending messages")
			}
		})
	}
}
func TestEnrolledProducerPolicyRejectsIdentityChanges(t *testing.T) {
	before := bootstrapConsumerConfig(bootstrapConfig())
	for _, kind := range []string{"id", "epoch", "added", "removed", "global", "route"} {
		t.Run(kind, func(t *testing.T) {
			after := before
			after.Producers = append([]Enrollment(nil), before.Producers...)
			switch kind {
			case "id":
				after.Producers[0].Producer.ID = "another"
			case "epoch":
				after.Producers[0].Producer.Epoch = "another"
			case "added":
				p := after.Producers[0]
				p.Producer.ID = "another"
				after.Producers = append(after.Producers, p)
			case "removed":
				after.Producers = nil
			case "global":
				after.MaxMessages++
			case "route":
				after.Routes = map[string]Route{"alerts": {Revision: "changed", Primary: "other"}}
			}
			if ValidateEnrolledProducerPolicyChange(before, after) == nil {
				t.Fatal("unrelated authority change accepted")
			}
		})
	}
}
