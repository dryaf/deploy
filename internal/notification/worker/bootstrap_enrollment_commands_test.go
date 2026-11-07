package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func bootstrapCommandConfig(c Config) Config {
	c = bootstrapConsumerConfig(c)
	p := c.Producers[0].Producer
	d := interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}
	c.Interaction = &interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "backup/manager", Producer: p, Generation: "one", Commands: map[string]interaction.Descriptor{"status": {ID: "backup.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "status"}}}}, Destinations: []interaction.Destination{d}, Permissions: []interaction.Permission{{Actor: "789", Conversation: d.ID, Worker: p.ID, Permissions: []string{"read"}, Scope: interaction.Scope{WholeWorker: true}}}}
	return c
}

func TestBootstrapCommandEnrollmentBoundaries(t *testing.T) {
	for _, ledState := range []string{"absent", "enabled", "disabled"} {
		for _, boundary := range []string{"intent", "producers", "owner/backup-worker", "complete"} {
			t.Run(ledState+"/"+boundary, func(t *testing.T) {
				ctx := context.Background()
				path, bootstrapOp := bootstrapTarget(t)
				before := bootstrapConfig()
				var id protocol.Identity
				var output LEDOutputRequest
				if ledState == "absent" {
					b, err := BootstrapGateway(ctx, path, bootstrapOp, before)
					must(t, err)
					id = b.Identity
				} else {
					path, before, output, _ = bootstrapLimitsOutputFixture(t, ledState == "disabled")
					id = output.Identity
				}
				after := bootstrapCommandConfig(before)
				must(t, PreflightBootstrapEnrollment(ctx, path, id, before, after, output.Operation, output.Disabled))
				op, _ := safefs.NewID("n")
				stop := errors.New("lost command enrollment reply")
				r, err := enrollBootstrap(ctx, path, id, after, op, func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				})
				if !errors.Is(err, stop) || r.Version != 3 || r.Identity != id {
					t.Fatal("missed boundary", r, err)
				}
				changed := bootstrapCommandConfig(before)
				changed.Interaction.Workers[0].Alias = "backup/other"
				if _, err = EnrollBootstrap(ctx, path, id, changed, op); err == nil {
					t.Fatal("retargeted alias")
				}
				for range 2 {
					again, e := EnrollBootstrap(ctx, path, id, after, op)
					must(t, e)
					if again != r {
						t.Fatal("receipt changed")
					}
				}
				s, err := OpenManagedVerification(path, after, id)
				must(t, err)
				must(t, s.RequireInactiveProducers())
				must(t, s.RequireLEDOutput(ctx, output.Operation, output.Disabled))
				d := after.Interaction.Destinations[0]
				in := interaction.Inbound{Provider: d.Provider, Account: d.Account, NativeID: "first", Actor: "789", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "backup/manager status"}
				if _, err = s.AdmitCommand(ctx, in); err == nil || s.commandIntakeEnabled() || s.commandRepliesEnabled() {
					t.Fatal("enrollment admitted commands")
				}
				var count int
				must(t, s.db.QueryRow("SELECT count(*) FROM command_aliases").Scan(&count))
				if count != 1 {
					t.Fatal("aliases not reserved")
				}
				must(t, s.Close())
				if ordinary, e := Open(path, after, false); e == nil {
					ordinary.Close()
					t.Fatal("unmanaged open bypassed admission")
				}
				required, err := BootstrapEnrollmentCommandsRequired(path)
				must(t, err)
				if !required {
					t.Fatal("helper downgrade allowed")
				}
			})
		}
	}
}

func TestBootstrapCommandEnrollmentRejectsHistoryBeforeIntent(t *testing.T) {
	for _, statement := range []string{"INSERT INTO telegram_cursors VALUES('123',42)", "INSERT INTO command_aliases VALUES('foreign','other','epoch')"} {
		t.Run(statement, func(t *testing.T) {
			ctx := context.Background()
			path, op := bootstrapTarget(t)
			before := bootstrapConfig()
			b, err := BootstrapGateway(ctx, path, op, before)
			must(t, err)
			s, err := OpenBootstrapVerification(path, before, b.Identity, "")
			must(t, err)
			_, err = s.db.Exec(statement)
			must(t, err)
			must(t, s.Close())
			after := bootstrapCommandConfig(before)
			if PreflightBootstrapEnrollment(ctx, path, b.Identity, before, after, "", false) == nil {
				t.Fatal("preflight accepted history")
			}
			enroll, _ := safefs.NewID("n")
			if _, err = EnrollBootstrap(ctx, path, b.Identity, after, enroll); err == nil {
				t.Fatal("enrollment accepted history")
			}
			if _, err = os.Stat(filepath.Join(path, "bootstrap-enrollment.json")); !os.IsNotExist(err) {
				t.Fatal("published invalid intent", err)
			}
		})
	}
}

func TestBootstrapCommandEnrollmentRequiresDurableAuthority(t *testing.T) {
	for _, damage := range []string{"alias", "receipt", "downgrade"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			path, op := bootstrapTarget(t)
			before := bootstrapConfig()
			b, err := BootstrapGateway(ctx, path, op, before)
			must(t, err)
			after := bootstrapCommandConfig(before)
			op, _ = safefs.NewID("n")
			_, err = EnrollBootstrap(ctx, path, b.Identity, after, op)
			must(t, err)
			d, err := safefs.Open(path)
			must(t, err)
			defer d.Close()
			switch damage {
			case "alias":
				s, err := OpenManagedVerification(path, after, b.Identity)
				must(t, err)
				_, err = s.db.Exec("DELETE FROM command_aliases")
				must(t, err)
				must(t, s.Close())
			case "receipt":
				must(t, os.Remove(filepath.Join(path, "bootstrap-enrollment.json")))
			case "downgrade":
				var e bootstrapEnrollment
				must(t, d.ReadJSON("bootstrap-enrollment.json", &e))
				e.Receipt.Version = 1
				must(t, d.WriteJSON("bootstrap-enrollment.json", e, true))
			}
			if s, err := OpenManagedVerification(path, after, b.Identity); err == nil {
				s.Close()
				t.Fatal("lost enrollment authority accepted")
			}
			if _, err = EnrollBootstrap(ctx, path, b.Identity, after, op); err == nil {
				t.Fatal("lost authority recreated")
			}
		})
	}
}
