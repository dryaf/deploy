package install

import (
	"context"
	"errors"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
	"testing"
)

func TestManagedEnrolledLimitsDurableBoundaries(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "enrolled-limits-migrated", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f, unbound := unboundUpdateFixture(t)
			raw, err := os.ReadFile(spec.RuntimeFile)
			if err != nil {
				t.Fatal(err)
			}
			var input RuntimeInput
			if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
				t.Fatal(err)
			}
			input.Bootstrap = false
			input.Worker.Producers = []worker.Enrollment{{Producer: protocol.Producer{ID: "backup-worker", Epoch: "reserved"}, Required: true, Routes: []string{"ops"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 10}}
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(unbound))
			if err != nil {
				t.Fatal(err)
			}
			input = changeBootstrapLimitsInput(t, spec)
			probe := func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.bootstrapLimitsProbe = probe
			i.enrolledLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("older helper") }
			stops := f.stops
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-limits"); err == nil || f.stops != stops {
				t.Fatal("incapable helper stopped service", err)
			}
			i.enrolledLimitsProbe = probe
			stop := errors.New("lost reply")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-limits"); !errors.Is(err, stop) {
				t.Fatal(boundary, err)
			}
			i.fault = nil
			after, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-limits")
			if err != nil {
				t.Fatal(err)
			}
			if before.Identity != after.Identity || before.Installation != after.Installation || after.Mode != "verification" || before.ConfigHash == after.ConfigHash {
				t.Fatal("changed authority")
			}
			starts := f.starts
			if again, e := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-limits"); e != nil || protocol.Hash(again) != protocol.Hash(after) || f.starts != starts {
				t.Fatal("replayed completion", e)
			}
			runtime, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			s, err := worker.OpenManagedVerification(spec.State, runtime.Worker, after.Identity)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
		})
	}
}

func TestManagedEnrolledFreshnessDurableBoundaries(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "enrolled-freshness-migrated", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f, unbound := unboundUpdateFixture(t)
			raw, err := os.ReadFile(spec.RuntimeFile)
			if err != nil {
				t.Fatal(err)
			}
			var input RuntimeInput
			if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
				t.Fatal(err)
			}
			input.Bootstrap = false
			input.Worker.Producers = []worker.Enrollment{{Producer: protocol.Producer{ID: "backup-worker", Epoch: "reserved"}, Required: true, Routes: []string{"ops"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 10}}
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(unbound))
			if err != nil {
				t.Fatal(err)
			}
			input.Worker.Producers[0].MaxSilenceSeconds = 300
			input.Worker.Producers[0].MaxEvidenceSeconds = 172800
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			probe := func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.bootstrapLimitsProbe = probe
			i.enrolledLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("older helper") }
			stops := f.stops
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-freshness"); err == nil || f.stops != stops {
				t.Fatal("incapable helper stopped service", err)
			}
			i.enrolledLimitsProbe = probe
			stop := errors.New("lost reply")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-freshness"); !errors.Is(err, stop) {
				t.Fatal(boundary, err)
			}
			i.fault = nil
			after, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-freshness")
			if err != nil {
				t.Fatal(err)
			}
			if before.Identity != after.Identity || before.Installation != after.Installation || after.Mode != "verification" || before.ConfigHash == after.ConfigHash {
				t.Fatal("changed authority")
			}
			starts := f.starts
			if again, e := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-freshness"); e != nil || protocol.Hash(again) != protocol.Hash(after) || f.starts != starts {
				t.Fatal("replayed completion", e)
			}
			runtime, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			s, err := worker.OpenManagedVerification(spec.State, runtime.Worker, after.Identity)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
		})
	}
}

func TestManagedEnrolledRoutesDurableBoundaries(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "enrolled-routes-migrated", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f, unbound := unboundUpdateFixture(t)
			raw, err := os.ReadFile(spec.RuntimeFile)
			if err != nil {
				t.Fatal(err)
			}
			var input RuntimeInput
			if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
				t.Fatal(err)
			}
			input.Bootstrap = false
			input.Worker.Producers = []worker.Enrollment{{Producer: protocol.Producer{ID: "backup-worker", Epoch: "reserved"}, Required: true, Routes: []string{"ops"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 10}}
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(unbound))
			if err != nil {
				t.Fatal(err)
			}
			input.Worker.Routes["audit"] = worker.Route{Revision: "audit-1", Primary: input.Worker.Routes["ops"].Primary}
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			probe := func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.bootstrapLimitsProbe = probe
			i.enrolledLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("older helper") }
			stops := f.stops
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-routes"); err == nil || f.stops != stops {
				t.Fatal("incapable helper stopped service", err)
			}
			i.enrolledLimitsProbe = probe
			stop := errors.New("lost reply")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-routes"); !errors.Is(err, stop) {
				t.Fatal(boundary, err)
			}
			i.fault = nil
			after, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-routes")
			if err != nil {
				t.Fatal(err)
			}
			if before.Identity != after.Identity || before.Installation != after.Installation || after.Mode != "verification" || before.ConfigHash == after.ConfigHash {
				t.Fatal("changed authority")
			}
			starts := f.starts
			if again, e := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-routes"); e != nil || protocol.Hash(again) != protocol.Hash(after) || f.starts != starts {
				t.Fatal("replayed completion", e)
			}
			runtime, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			s, err := worker.OpenManagedVerification(spec.State, runtime.Worker, after.Identity)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
		})
	}
}

func TestManagedEnrolledProducerPolicyDurableBoundaries(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "enrolled-producer-migrated", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f, unbound := unboundUpdateFixture(t)
			raw, err := os.ReadFile(spec.RuntimeFile)
			if err != nil {
				t.Fatal(err)
			}
			var input RuntimeInput
			if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
				t.Fatal(err)
			}
			input.Bootstrap = false
			input.Worker.Producers = []worker.Enrollment{{Producer: protocol.Producer{ID: "backup-worker", Epoch: "reserved"}, Required: true, Routes: []string{"ops"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 10}}
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(unbound))
			if err != nil {
				t.Fatal(err)
			}
			input.Worker.Producers[0].Required = false
			input.Worker.Producers[0].MaxMessages = 20
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			probe := func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.bootstrapLimitsProbe = probe
			i.enrolledLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("older helper") }
			stops := f.stops
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-producer"); err == nil || f.stops != stops {
				t.Fatal("incapable helper stopped service", err)
			}
			i.enrolledLimitsProbe = probe
			stop := errors.New("lost reply")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-producer"); !errors.Is(err, stop) {
				t.Fatal(boundary, err)
			}
			i.fault = nil
			after, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-producer")
			if err != nil {
				t.Fatal(err)
			}
			if before.Identity != after.Identity || before.Installation != after.Installation || after.Mode != "verification" || before.ConfigHash == after.ConfigHash {
				t.Fatal("changed authority")
			}
			starts := f.starts
			if again, e := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-producer"); e != nil || protocol.Hash(again) != protocol.Hash(after) || f.starts != starts {
				t.Fatal("replayed completion", e)
			}
			runtime, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			s, err := worker.OpenManagedVerification(spec.State, runtime.Worker, after.Identity)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
		})
	}
}

func TestManagedEnrolledProvidersDurableBoundaries(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "enrolled-providers-migrated", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f, unbound := providerUpdateFixture(t)
			raw, err := os.ReadFile(spec.RuntimeFile)
			if err != nil {
				t.Fatal(err)
			}
			var input RuntimeInput
			if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
				t.Fatal(err)
			}
			input.Bootstrap = false
			input.Worker.Producers = []worker.Enrollment{{Producer: protocol.Producer{ID: "backup-worker", Epoch: "reserved"}, Required: true, Routes: []string{"ops"}, MaxSilenceSeconds: 150, MaxEvidenceSeconds: 86400, MaxMessages: 10}}
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := i.enrollVerificationConsumers(ctx, spec, protocol.Hash(unbound))
			if err != nil {
				t.Fatal(err)
			}
			input = changeBootstrapProviderInput(t, spec, false)
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			probe := func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.bootstrapLimitsProbe = probe
			i.enrolledLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("older helper") }
			stops := f.stops
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-providers"); err == nil || f.stops != stops {
				t.Fatal("incapable helper stopped service", err)
			}
			i.enrolledLimitsProbe = probe
			stop := errors.New("lost reply")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-providers"); !errors.Is(err, stop) {
				t.Fatal(boundary, err)
			}
			i.fault = nil
			after, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-providers")
			if err != nil {
				t.Fatal(err)
			}
			if before.Identity != after.Identity || before.Installation != after.Installation || after.Mode != "verification" || before.ConfigHash == after.ConfigHash {
				t.Fatal("changed authority")
			}
			starts := f.starts
			if again, e := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "enrolled-providers"); e != nil || protocol.Hash(again) != protocol.Hash(after) || f.starts != starts {
				t.Fatal("replayed completion", e)
			}
			runtime, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			s, err := worker.OpenManagedVerification(spec.State, runtime.Worker, after.Identity)
			if err != nil {
				t.Fatal(err)
			}
			s.Close()
		})
	}
}
