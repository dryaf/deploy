package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func revisedVerificationCommands(t *testing.T, before Config) Config {
	t.Helper()
	raw, err := json.Marshal(before)
	must(t, err)
	var after Config
	must(t, json.Unmarshal(raw, &after))
	after.Interaction.Revision = "revised"
	after.Interaction.Workers[0].Alias = "backup/revised"
	after.Interaction.Workers[0].Commands["ack"] = interaction.Descriptor{ID: "backup.ack", Version: 1, Permission: "incident.manage", Effect: "mutation", Scoped: true, Help: "acknowledge"}
	d := after.Interaction.Destinations[0]
	d.ID, d.Generation, d.NativeID = "new-ops", "two", "654"
	after.Interaction.Destinations = append(after.Interaction.Destinations, d)
	p := after.Interaction.Permissions[0]
	p.Actor, p.Conversation, p.Permissions = "987", d.ID, []string{"read", "incident.manage"}
	after.Interaction.Permissions = append(after.Interaction.Permissions, p)
	must(t, after.Validate())
	return after
}

func TestEnrolledCommandPolicyBeforeAdmission(t *testing.T) {
	for _, origin := range []string{"bootstrap", "existing", "led-disabled"} {
		for _, boundary := range []string{"intent", "committed", "complete"} {
			t.Run(origin+"/"+boundary, func(t *testing.T) {
				ctx := context.Background()
				path, id, before := enrolledLimitsFixture(t)
				if origin == "bootstrap" {
					var op string
					path, op = bootstrapTarget(t)
					b, err := BootstrapGateway(ctx, path, op, bootstrapConfig())
					must(t, err)
					before, id = bootstrapCommandConfig(b.Config), b.Identity
					op, _ = safefs.NewID("n")
					_, err = EnrollBootstrap(ctx, path, id, before, op)
					must(t, err)
				} else {
					if origin == "led-disabled" {
						var output LEDOutputRequest
						path, before, output, _ = enrolledLimitsOutputFixture(t, true)
						id = output.Identity
					}
					initial := before
					before.Interaction = bootstrapCommandConfig(before).Interaction
					op, _ := safefs.NewID("n")
					_, err := MigrateEnrolledCommands(ctx, path, id, initial, before, op)
					must(t, err)
				}
				original, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
				must(t, err)
				after := revisedVerificationCommands(t, before)
				must(t, PreflightEnrolledCommandPolicy(ctx, path, id, before, after))
				op, _ := safefs.NewID("n")
				stop := errors.New("lost reply")
				_, err = migrateEnrolledPolicy(ctx, path, id, before, after, op, 12, func(at string) error {
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
							t.Fatal("pending policy admitted runtime")
						}
					}
				}
				bad := after
				bad.MaxMessages++
				if _, e := MigrateEnrolledCommandPolicy(ctx, path, id, before, bad, op); e == nil {
					t.Fatal("changed retry accepted")
				}
				for range 2 {
					_, err = MigrateEnrolledCommandPolicy(ctx, path, id, before, after, op)
					must(t, err)
				}
				if s, e := OpenManagedVerification(path, before, id); e == nil {
					s.Close()
					t.Fatal("stale policy accepted")
				}
				s, err := OpenManagedVerification(path, after, id)
				must(t, err)
				if s.commandIntakeEnabled() || s.commandRepliesEnabled() {
					t.Fatal("policy update admitted commands")
				}
				var count int
				must(t, s.db.QueryRow("SELECT count(*) FROM command_aliases").Scan(&count))
				if count != 2 {
					t.Fatal("alias history lost", count)
				}
				must(t, s.Close())
				next := after
				next.MaxMessages++
				op, _ = safefs.NewID("n")
				_, err = MigrateEnrolledLimits(ctx, path, id, after, next, op)
				must(t, err)
				// A later policy can remove the new audience and alias, retaining
				// the reservations without inventing any accepted command history.
				last := before
				last.MaxMessages = next.MaxMessages
				op, _ = safefs.NewID("n")
				_, err = MigrateEnrolledCommandPolicy(ctx, path, id, next, last, op)
				must(t, err)
				saved, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
				must(t, err)
				if string(saved) != string(original) {
					t.Fatal("producer enrollment changed")
				}
				s, err = OpenManagedVerification(path, last, id)
				must(t, err)
				_, err = s.db.Exec("DELETE FROM command_aliases WHERE alias=?", after.Interaction.Workers[0].Alias)
				must(t, err)
				must(t, s.Close())
				if s, e := OpenManagedVerification(path, last, id); e == nil {
					s.Close()
					t.Fatal("missing historical alias recreated")
				}
			})
		}
	}
}

func TestEnrolledCommandPolicyRejectsUnprovedHistory(t *testing.T) {
	for _, kind := range []string{"foreign-alias", "missing-alias", "cursor", "command-directory", "producer", "generation", "delivery-owner"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path, op := bootstrapTarget(t)
			b, err := BootstrapGateway(ctx, path, op, bootstrapConfig())
			must(t, err)
			before := bootstrapCommandConfig(b.Config)
			op, _ = safefs.NewID("n")
			_, err = EnrollBootstrap(ctx, path, b.Identity, before, op)
			must(t, err)
			after := revisedVerificationCommands(t, before)
			s, err := OpenManagedVerification(path, before, b.Identity)
			must(t, err)
			switch kind {
			case "foreign-alias":
				_, err = s.db.Exec("INSERT INTO command_aliases VALUES('foreign','backup-worker','reserved')")
			case "missing-alias":
				_, err = s.db.Exec("DELETE FROM command_aliases")
			case "cursor":
				_, err = s.db.Exec("INSERT INTO telegram_cursors VALUES('123',42)")
			case "command-directory":
				err = os.Mkdir(filepath.Join(path, "managed-commands"), 0700)
			case "producer":
				after.Interaction.Workers[0].Producer = protocol.Producer{ID: "other", Epoch: "two"}
			case "generation":
				after.Interaction.Workers[0].Generation = "other"
			case "delivery-owner":
				err = os.WriteFile(filepath.Join(path, managedDeliveryOwnerFile), []byte("{}"), 0600)
			}
			must(t, err)
			must(t, s.Close())
			if PreflightEnrolledCommandPolicy(ctx, path, b.Identity, before, after) == nil {
				t.Fatal("invalid authority accepted")
			}
			if _, e := os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(e) {
				t.Fatal("refused preflight retained intent", e)
			}
		})
	}
}
