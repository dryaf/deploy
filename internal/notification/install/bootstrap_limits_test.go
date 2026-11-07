package install

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func changeBootstrapLimitsInput(t *testing.T, spec ManagedSpec, routesOnly ...bool) RuntimeInput {
	t.Helper()
	raw, err := os.ReadFile(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	var input RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	if len(routesOnly) > 0 && routesOnly[0] {
		for _, route := range input.Worker.Routes {
			input.Worker.Routes["audit"] = route
			break
		}
	} else {
		input.Worker.MaxMessages++
	}
	raw, err = input.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return input
}

func TestManagedBootstrapLimitsDurableBoundaries(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "bootstrap-limits-migrated", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f, before := unboundUpdateFixture(t)
			i.bootstrapLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			input := changeBootstrapLimitsInput(t, spec)
			stop := errors.New("lost limits update response")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err := i.migrateBootstrapLimits(ctx, spec, protocol.Hash(before)); !errors.Is(err, stop) {
				t.Fatal("boundary", boundary, err)
			}
			i.fault = nil
			after, err := i.migrateBootstrapLimits(ctx, spec, protocol.Hash(before))
			if err != nil {
				t.Fatal(err)
			}
			if after.Identity != before.Identity || after.Installation != before.Installation || after.BinaryHash != before.BinaryHash || after.ConfigHash == before.ConfigHash || after.RuntimeHash == before.RuntimeHash || after.HelperRevision != after.Revision || after.Mode != "verification" {
				t.Fatal("limits lost authority")
			}
			starts := f.starts
			if again, err := i.migrateBootstrapLimits(ctx, spec, protocol.Hash(before)); err != nil || protocol.Hash(again) != protocol.Hash(after) || f.starts != starts {
				t.Fatal("repeated effects", err)
			}
			resolved, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			s, err := worker.OpenBootstrapVerification(spec.State, resolved.Worker, after.Identity, "")
			if err != nil {
				t.Fatal("cannot open revised runtime", err)
			}
			s.Close()
			// Enrollment must use the new limits without rewriting the old bootstrap.
			input.Bootstrap = false
			input.Worker.Producers = []worker.Enrollment{{Producer: protocol.Producer{ID: "backup-worker", Epoch: "reserved"}, Required: true, Routes: []string{"ops"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 10}}
			raw, _ := input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			bound, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(after))
			if err != nil {
				t.Fatal("enrollment after limits", err)
			}
			if bound.Identity != before.Identity {
				t.Fatal("enrollment changed identity")
			}
		})
	}
}

func TestManagedBootstrapLimitsRefusesUnrelatedChangesAndIncapableHelpers(t *testing.T) {
	for _, kind := range []string{"route", "capability", "bound", "led-history"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f, before := unboundUpdateFixture(t)
			i.bootstrapLimitsProbe = func(context.Context, *safefs.Directory, string, string) error {
				if kind == "capability" {
					return errors.New("helper lacks limits support")
				}
				return nil
			}
			if kind == "bound" {
				var err error
				spec, i, f, _ = managedFixture(t)
				before, err = i.prepare(ctx, "setup", spec)
				if err != nil {
					t.Fatal(err)
				}
			}
			input := changeBootstrapLimitsInput(t, spec)
			if kind == "route" {
				r := input.Worker.Routes["ops"]
				r.Revision = "changed"
				input.Worker.Routes["ops"] = r
				raw, _ := input.Bytes()
				if err := os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "led-history" {
				// Test the validator directly; a fabricated managed history is never
				// installed merely to exercise this preflight refusal.
				before.LEDOutput = &ManagedLEDOutput{Disabled: true}
				c, e := input.Resolve()
				if e != nil {
					t.Fatal(e)
				}
				if validateManagedBootstrapLimits(before, c, c) == nil {
					t.Fatal("LED history silently rebound")
				}
				return
			}
			starts := f.starts
			if _, err := i.migrateBootstrapLimits(ctx, spec, protocol.Hash(before)); err == nil {
				t.Fatal("unsafe migration accepted")
			}
			if starts != f.starts {
				t.Fatal("unsafe migration restarted service")
			}
			if _, err := os.Stat(spec.State + "/bootstrap-limits.json"); !os.IsNotExist(err) {
				t.Fatal("unsafe migration created domain intent", err)
			}
		})
	}
}
