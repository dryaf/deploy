package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func unboundUpdateFixture(t *testing.T, withLED ...bool) (ManagedSpec, managedInstaller, *fakeManagedServices, ManagedManifest) {
	t.Helper()
	spec, i, f, _ := managedFixture(t)
	i.bootstrapProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	spec.State = filepath.Join(filepath.Dir(spec.State), "unbound")
	raw, err := os.ReadFile(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	var input RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	input.Bootstrap, input.Worker.Producers = true, nil
	if len(withLED) > 0 && withLED[0] {
		input.LEDSocket = "/private/fixture-led.sock"
	}
	c, err := input.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	op, _ := safefs.NewID("n")
	if _, err = worker.BootstrapGateway(context.Background(), spec.State, op, c.Worker); err != nil {
		t.Fatal(err)
	}
	raw, _ = input.Bytes()
	if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := i.ensureVerification(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return spec, i, f, m
}

func TestUnboundArtifactUpdateDurableBoundaries(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "files", "published", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f, before := unboundUpdateFixture(t)
			starts := f.starts
			// Metadata/scope conversion can validate the same release without a
			// service stop or a new installation revision.
			if got, err := i.updateBootstrapVerification(ctx, spec, protocol.Hash(before)); err != nil || protocol.Hash(got) != protocol.Hash(before) || f.starts != starts {
				t.Fatal("unchanged artifact caused effects", err)
			}
			raw, err := os.ReadFile(spec.BinaryFile)
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, []byte("second release fixture")...)
			if err = os.WriteFile(spec.BinaryFile, raw, 0700); err != nil {
				t.Fatal(err)
			}
			spec.BinarySHA256 = managedDigest(raw)
			stop := errors.New("lost update response")
			i.fault = func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}
			if _, err = i.updateBootstrapVerification(ctx, spec, protocol.Hash(before)); !errors.Is(err, stop) {
				t.Fatal("update boundary not exercised", boundary, err)
			}
			i.fault = nil
			after, err := i.updateBootstrapVerification(ctx, spec, protocol.Hash(before))
			if err != nil {
				t.Fatal(err)
			}
			if after.BinaryHash != spec.BinarySHA256 || after.ConfigHash != before.ConfigHash || after.RuntimeHash != before.RuntimeHash || after.Identity != before.Identity || after.Installation != before.Installation || after.HelperHash != before.HelperHash || after.HelperRevision != before.HelperRevision || after.Revision == before.Revision {
				t.Fatal("artifact update changed runtime authority or recovery helper")
			}
			starts = f.starts
			for range 2 {
				again, err := i.updateBootstrapVerification(ctx, spec, protocol.Hash(before))
				if err != nil || protocol.Hash(again) != protocol.Hash(after) || f.starts != starts {
					t.Fatal("completed update repeated", err)
				}
			}
		})
	}
}

func TestUnboundArtifactUpdateRefusesConfigurationAndEnrollment(t *testing.T) {
	for _, kind := range []string{"configuration", "bound"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			spec, i, f, before := unboundUpdateFixture(t)
			if kind == "bound" {
				var err error
				spec, i, f, _ = managedFixture(t)
				before, err = i.prepare(ctx, "setup", spec)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				raw, err := os.ReadFile(spec.RuntimeFile)
				if err != nil {
					t.Fatal(err)
				}
				var input RuntimeInput
				if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
					t.Fatal(err)
				}
				input.Worker.MaxMessages++
				raw, err = input.Bytes()
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			starts := f.starts
			if _, err := i.updateBootstrapVerification(ctx, spec, protocol.Hash(before)); err == nil || f.starts != starts {
				t.Fatal("unsafe artifact update accepted")
			}
		})
	}
}
