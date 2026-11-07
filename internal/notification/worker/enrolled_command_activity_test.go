package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
)

func TestEnrolledCommandActivityRetainsPolicy(t *testing.T) {
	for _, output := range []string{"absent", "disabled"} {
		for _, boundary := range []string{"intent", "committed", "complete"} {
			t.Run(output+"/"+boundary, func(t *testing.T) {
				ctx := context.Background()
				path, id, initial := enrolledLimitsFixture(t)
				if output == "disabled" {
					var led LEDOutputRequest
					path, initial, led, _ = enrolledLimitsOutputFixture(t, true)
					id = led.Identity
				}
				before := initial
				before.Interaction = bootstrapCommandConfig(initial).Interaction
				op, _ := safefs.NewID("n")
				_, err := MigrateEnrolledCommands(ctx, path, id, initial, before, op)
				must(t, err)
				// Preserve a policy revised after its first enrollment too.
				revised := revisedVerificationCommands(t, before)
				op, _ = safefs.NewID("n")
				_, err = MigrateEnrolledCommandPolicy(ctx, path, id, before, revised, op)
				must(t, err)
				before = revised
				original, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
				must(t, err)
				for _, enabled := range []bool{false, true, false, true} {
					after := before
					if enabled {
						after.Interaction = revised.Interaction
					} else {
						after.Interaction = nil
					}
					must(t, PreflightEnrolledCommandActivity(ctx, path, id, before, after))
					op, _ = safefs.NewID("n")
					stop := errors.New("lost reply")
					_, err = migrateEnrolledPolicy(ctx, path, id, before, after, op, 13, func(at string) error {
						if at == boundary {
							return stop
						}
						return nil
					})
					if !errors.Is(err, stop) {
						t.Fatal(enabled, err)
					}
					for range 2 {
						_, err = MigrateEnrolledCommandActivity(ctx, path, id, before, after, op)
						must(t, err)
					}
					s, err := OpenManagedVerification(path, after, id)
					must(t, err)
					if s.commandIntakeEnabled() || s.commandRepliesEnabled() {
						t.Fatal("toggle admitted intake")
					}
					var aliases int
					must(t, s.db.QueryRow("SELECT count(*) FROM command_aliases").Scan(&aliases))
					if aliases != 2 {
						t.Fatal("aliases lost", aliases)
					}
					must(t, s.Close())
					if enabled {
						before = after
						continue
					}
					// Restoring changed permissions must fail before retaining intent.
					raw, err := json.Marshal(revised.Interaction)
					must(t, err)
					bad := after
					must(t, json.Unmarshal(raw, &bad.Interaction))
					bad.Interaction.Permissions[0].Actor = "unreviewed"
					if PreflightEnrolledCommandActivity(ctx, path, id, after, bad) == nil {
						t.Fatal("changed restoration accepted")
					}
					// Ordinary unrelated policy can follow a completed toggle.
					before = after
					before.MaxMessages++
					op, _ = safefs.NewID("n")
					_, err = MigrateEnrolledLimits(ctx, path, id, after, before, op)
					must(t, err)
					present, err := VerificationCommandsRetained(path)
					must(t, err)
					if !present {
						t.Fatal("disabled policy forgotten")
					}
				}
				saved, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
				must(t, err)
				if string(saved) != string(original) {
					t.Fatal("producer replaced")
				}
			})
		}
	}
}

func TestEnrolledCommandActivityRequiresPriorPolicy(t *testing.T) {
	path, id, before := enrolledLimitsFixture(t)
	after := before
	after.Interaction = bootstrapCommandConfig(before).Interaction
	if PreflightEnrolledCommandActivity(context.Background(), path, id, before, after) == nil {
		t.Fatal("initial declaration treated as retained activity")
	}
	present, err := VerificationCommandsRetained(path)
	must(t, err)
	if present {
		t.Fatal("invented prior commands")
	}
}
