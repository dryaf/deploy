package worker

import (
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
	"path/filepath"
	"testing"
)

func TestEnrolledCommandsRetainProducerAndFenceAdmission(t *testing.T) {
	for _, output := range []string{"absent", "enabled", "disabled"} {
		for _, boundary := range []string{"intent", "committed", "complete"} {
			t.Run(output+"/"+boundary, func(t *testing.T) {
				ctx := context.Background()
				path, id, before := enrolledLimitsFixture(t)
				if output != "absent" {
					var outputRequest LEDOutputRequest
					path, before, outputRequest, _ = enrolledLimitsOutputFixture(t, output == "disabled")
					id = outputRequest.Identity
				}
				original, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
				must(t, err)
				after := before
				after.Interaction = bootstrapCommandConfig(before).Interaction
				must(t, PreflightEnrolledCommands(ctx, path, id, before, after))
				op, _ := safefs.NewID("n")
				stop := errors.New("interrupted")
				_, err = migrateEnrolledPolicy(ctx, path, id, before, after, op, 11, func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				})
				if !errors.Is(err, stop) {
					t.Fatal(err)
				}
				for range 2 {
					_, err = MigrateEnrolledCommands(ctx, path, id, before, after, op)
					must(t, err)
				}
				if stale, e := OpenManagedVerification(path, before, id); e == nil {
					stale.Close()
					t.Fatal("old command-free runtime accepted")
				}
				if unmanaged, e := Open(path, after, false); e == nil {
					unmanaged.Close()
					t.Fatal("unmanaged runtime admitted commands")
				}
				s, err := OpenManagedVerification(path, after, id)
				must(t, err)
				must(t, s.RequireInactiveProducers())
				if s.commandIntakeEnabled() || s.commandRepliesEnabled() {
					t.Fatal("enrollment enabled command intake")
				}
				var aliases int
				must(t, s.db.QueryRow("SELECT count(*) FROM command_aliases").Scan(&aliases))
				if aliases != 1 {
					t.Fatal("initial aliases not reserved", aliases)
				}
				must(t, s.Close())
				next := after
				next.MaxMessages++
				more, _ := safefs.NewID("n")
				_, err = MigrateEnrolledLimits(ctx, path, id, after, next, more)
				must(t, err)
				saved, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
				must(t, err)
				if string(original) != string(saved) {
					t.Fatal("producer enrollment rewritten")
				}
				s, err = OpenManagedVerification(path, next, id)
				must(t, err)
				_, err = s.db.Exec("DELETE FROM command_aliases")
				must(t, err)
				must(t, s.Close())
				if reopened, e := OpenManagedVerification(path, next, id); e == nil {
					reopened.Close()
					t.Fatal("missing alias authority recreated")
				}
			})
		}
	}
}
func TestEnrolledCommandsRefuseExistingAliasesAndChangedProducer(t *testing.T) {
	ctx := context.Background()
	path, id, before := enrolledLimitsFixture(t)
	after := before
	after.Interaction = bootstrapCommandConfig(before).Interaction
	bad := after
	bad.MaxMessages++
	if PreflightEnrolledCommands(ctx, path, id, before, bad) == nil {
		t.Fatal("mixed policy accepted")
	}
	s, err := OpenManagedVerification(path, before, id)
	must(t, err)
	w := after.Interaction.Workers[0]
	_, err = s.db.Exec("INSERT INTO command_aliases(alias,producer,epoch) VALUES(?,?,?)", w.Alias, w.Producer.ID, w.Producer.Epoch)
	must(t, err)
	must(t, s.Close())
	if PreflightEnrolledCommands(ctx, path, id, before, after) == nil {
		t.Fatal("foreign matching alias adopted")
	}
	if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
		t.Fatal("rejected preflight wrote intent", err)
	}
}
