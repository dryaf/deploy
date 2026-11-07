package worker

import (
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
	"path/filepath"
	"testing"
)

func TestEnrolledProvidersFencesUnchangedRoutesAcrossResume(t *testing.T) {
	testEnrolledProviders(t, false)
}
func TestEnrolledProvidersRetainUnadmittedCommands(t *testing.T) { testEnrolledProviders(t, true) }
func testEnrolledProviders(t *testing.T, commands bool) {
	for _, change := range []bool{false, true} {
		for _, boundary := range []string{"intent", "committed", "complete"} {
			t.Run(boundary+map[bool]string{false: "/token", true: "/destination"}[change], func(t *testing.T) {
				path, id, before := enrolledLimitsFixture(t)
				if commands {
					initial := before
					before.Interaction = bootstrapCommandConfig(before).Interaction
					op, _ := safefs.NewID("n")
					_, err := MigrateEnrolledCommands(context.Background(), path, id, initial, before, op)
					must(t, err)
				}
				after := before
				if change {
					after.Routes = map[string]Route{"alerts": {Revision: "new-destination", Primary: "telegram"}}
				}
				original, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
				must(t, err)
				op, _ := safefs.NewID("n")
				stop := errors.New("lost provider reply")
				must(t, PreflightEnrolledProviders(context.Background(), path, id, before, after))
				receipt, err := migrateEnrolledPolicy(context.Background(), path, id, before, after, op, 5, func(at string) error {
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
						t.Fatal("pending provider change admitted runtime")
					}
				}
				again, err := MigrateEnrolledProviders(context.Background(), path, id, before, after, op)
				must(t, err)
				if protocol.Hash(again) != protocol.Hash(receipt) {
					t.Fatal("provider receipt changed")
				}
				s, err := OpenManagedVerification(path, after, id)
				must(t, err)
				must(t, s.RequireInactiveProducers())
				must(t, s.Close())
				saved, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
				must(t, err)
				if string(saved) != string(original) {
					t.Fatal("rewrote producer enrollment")
				}
			})
		}
	}
}

func TestEnrolledProvidersRejectRetainedCommandInput(t *testing.T) {
	ctx := context.Background()
	path, id, initial := enrolledLimitsFixture(t)
	before := initial
	before.Interaction = bootstrapCommandConfig(before).Interaction
	op, _ := safefs.NewID("n")
	_, err := MigrateEnrolledCommands(ctx, path, id, initial, before, op)
	must(t, err)
	s, err := OpenManagedVerification(path, before, id)
	must(t, err)
	_, err = s.db.Exec("INSERT INTO telegram_cursors VALUES('123',42)")
	must(t, err)
	must(t, s.Close())
	if PreflightEnrolledProviders(ctx, path, id, before, before) == nil {
		t.Fatal("provider replacement ignored retained command intake")
	}
}
