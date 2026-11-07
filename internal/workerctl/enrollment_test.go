package workerctl

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestReservedConsumerEnrollmentLostReplyAndNoOp(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, mode := range []string{"plain", "disabled-output", "adopted", "commands"} {
		t.Run(mode, func(t *testing.T) {
			outputHistory := mode == "disabled-output"
			r, paths := bootstrapControlFixture(t)
			if mode == "commands" {
				declareFixtureReadCommands(t, &r)
			}
			if r.Desired.Backup == nil {
				t.Fatal("fixture requires backup declaration")
			}
			input, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
			if err != nil {
				t.Fatal(err)
			}
			d, err := ensureConfigDirectory(paths.Control)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if outputHistory {
				input.LEDSocket = "/private/fixture-led.sock"
			}
			id, _ := safefs.NewID("n")
			child, _ := safefs.NewID("n")
			w, _ := safefs.NewID("w")
			e, _ := safefs.NewID("n")
			g, _ := safefs.NewID("g")
			reservation := backup.ProducerReservation{Version: 1, Producer: protocol.Producer{ID: w, Epoch: e}, Generation: g}
			gw, _ := safefs.NewID("w")
			ge, _ := safefs.NewID("g")
			before := install.ManagedManifest{Version: 1, Root: paths.Installation, Installation: "fixture-installation", Identity: protocol.Identity{Worker: gw, Recovery: ge}, Mode: "verification", BinaryHash: r.HelperSHA256, UnitDirectory: paths.UnitDirectory, Paths: install.ServicePaths{State: paths.State}}
			old := OperationJournal{Version: 1, ID: id, Complete: true, Pins: OperationPins{Worker: r.Worker, Action: "apply", DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Artifacts: map[string]string{"helper": r.HelperSHA256}, Installations: map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}}, Children: []ChildReference{{Name: "notification-bootstrap", Operation: child, Complete: true, Receipt: protocol.Hash(before)}}}
			if outputHistory {
				before.LEDOutput = &install.ManagedLEDOutput{Operation: child, Disabled: true}
				old.Children[0].Name = "notification-led-output"
				old.Children[0].Receipt = protocol.Hash(before)
			}
			lock, err := d.Lock("operation.lock", true, true)
			if err != nil {
				t.Fatal(err)
			}
			lock.Close()
			if err = d.WriteJSON("operation.json", old, false); err != nil {
				t.Fatal(err)
			}
			if mode == "adopted" {
				var review *install.PolicyReview
				r, paths, review = adoptionFixture(t)
				l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }}
				if _, _, err = reconcileNotificationAdoption(context.Background(), r, paths, l); err != nil {
					t.Fatal(err)
				}
				d, err = safefs.Open(paths.Control)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				if err = d.ReadJSON("operation.json", &old); err != nil {
					t.Fatal(err)
				}
				before, input = review.Manifest, review.Input
			}
			observed := install.InstallationObservation{Manifest: before, Phase: "complete", Unbound: true}
			calls := 0
			lifecycle := notificationEnrollmentLifecycle{
				inspect: func(context.Context, string) (install.InstallationObservation, error) { return observed, nil },
				enroll: func(ctx context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
					calls++
					if expected != protocol.Hash(before) {
						t.Fatal("lost reviewed predecessor")
					}
					raw, err := os.ReadFile(spec.RuntimeFile)
					if err != nil {
						t.Fatal(err)
					}
					var generated install.RuntimeInput
					if err = safefs.DecodeJSON(raw, &generated, maxControlFrame); err != nil {
						t.Fatal(err)
					}
					if generated.Bootstrap || len(generated.Worker.Producers) != 1 || generated.Worker.Producers[0].Producer != reservation.Producer {
						t.Fatal("enrollment did not use reserved producer", generated.Worker.Producers)
					}
					if mode == "commands" && (generated.Worker.Interaction == nil || generated.Telegram == nil || generated.Worker.Interaction.Workers[0].Producer != reservation.Producer) {
						t.Fatal("lost declared commands during enrollment")
					}
					for name, route := range input.Worker.Routes {
						if generated.Worker.Routes[name].Revision != route.Revision {
							t.Fatal("enrollment replaced retained route labels")
						}
					}
					if generated.LEDSocket != input.LEDSocket {
						t.Fatal("enrollment discarded disabled endpoint")
					}
					observed.Manifest.RuntimeHash = fmt.Sprintf("%x", sha256.Sum256(raw))
					observed.Manifest.Revision = "consumer-revision"
					observed.Unbound = false
					return observed.Manifest, errors.New("lost enrollment response")
				},
			}
			if _, err = enrollReservedBackup(context.Background(), r, paths, "/helper", d, old, input, reservation, lifecycle); err == nil {
				t.Fatal("lost response hidden")
			}
			var pending OperationJournal
			if err = d.ReadJSON("operation.json", &pending); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				result, e := enrollReservedBackup(context.Background(), r, paths, "/helper", d, pending, input, reservation, lifecycle)
				if e != nil || result.State != "verification" || result.Identity == nil || *result.Identity != before.Identity || calls != 1 {
					t.Fatal("enrollment replay or identity change", result, calls, e)
				}
				if err = d.ReadJSON("operation.json", &pending); err != nil {
					t.Fatal(err)
				}
			}
			changed := reservation
			diagnostic, err := testNotificationControl(context.Background(), r, paths, func(ctx context.Context, root, expected string) (install.DiagnosticResult, error) {
				if root != paths.Installation || expected != protocol.Hash(observed.Manifest) {
					t.Fatal("diagnostic used obsolete bootstrap receipt")
				}
				return install.DiagnosticResult{Version: 1, Identity: observed.Manifest.Identity, Status: "provider_accepted"}, nil
			})
			if err != nil || diagnostic.Diagnostic == nil || diagnostic.Diagnostic.Qualified {
				t.Fatal("enrolled diagnostic unavailable or qualified production", diagnostic, err)
			}
			changed.Generation, _ = safefs.NewID("g")
			if _, err = enrollReservedBackup(context.Background(), r, paths, "/helper", d, pending, input, changed, lifecycle); err == nil {
				t.Fatal("retargeted enrollment generation")
			}
			if calls != 1 {
				t.Fatal("drift repeated migration")
			}
		})
	}
}
