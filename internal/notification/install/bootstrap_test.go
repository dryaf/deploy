package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestManagedUnboundGateway(t *testing.T) {
	spec, i, services, _ := managedFixture(t)
	i.bootstrapProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	// Keep the ordinary fixture's enrolled state to prove that bootstrap creates a
	// distinct new gateway, never reinitializes/adopts an existing state directory.
	spec.State = filepath.Join(filepath.Dir(spec.State), "unbound")
	raw, err := os.ReadFile(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	var input RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	input.Bootstrap = true
	input.Worker.Producers = nil
	input.Worker.Interaction = nil
	input.Worker.LEDBindings = nil
	c, err := input.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	op, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := worker.BootstrapGateway(context.Background(), spec.State, op, c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = input.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := i.prepare(context.Background(), "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	if m, err = i.resume(context.Background(), spec.Root); err != nil {
		t.Fatal(err)
	}
	if m.Identity != reservation.Identity || m.Mode != "verification" || services.starts != 1 {
		t.Fatal("bootstrap installation changed authority", m, services.starts)
	}
	s, err := openManagedStore(m, c)
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Condition != "unknown" || !h.VerificationOnly {
		t.Fatal("unbound gateway claims production health", h)
	}
	if err = s.EnrollManagedDelivery(deliveryOwner(m, c)); err == nil {
		t.Fatal("empty gateway granted delivery authority")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	observation, err := i.inspectInstallation(context.Background(), spec.Root)
	if err != nil || !observation.Unbound || observation.Phase != "complete" || observation.Manifest.Identity != m.Identity {
		t.Fatal("canonical bootstrap observation", observation, err)
	}
	beforeStarts := services.starts
	for retry := 0; retry < 2; retry++ {
		current, e := i.ensureVerification(context.Background(), spec)
		if e != nil {
			t.Fatal(e)
		}
		if current.Revision != m.Revision || current.Identity != m.Identity || services.starts != beforeStarts {
			t.Fatal("unchanged ensure restarted or replaced gateway")
		}
		_, done, e := i.inspectVerification(context.Background(), spec)
		if e != nil || !done {
			t.Fatal("canonical receipt unavailable", e)
		}
	}
	stopCount := services.stops
	if _, err = i.uninstallExpected(context.Background(), spec.Root, strings.Repeat("a", 64)); err == nil || services.stops != stopCount {
		t.Fatal("stale removal decision changed service")
	}
	removed, err := i.uninstallExpected(context.Background(), spec.Root, protocol.Hash(m))
	if err != nil {
		t.Fatal(err)
	}
	if removed.Mode != "uninstalled" || removed.Identity != m.Identity {
		t.Fatal("removal changed identity")
	}
	if _, err = os.Stat(filepath.Join(spec.State, "identity.json")); err != nil {
		t.Fatal("removal erased state", err)
	}
	if _, err = i.uninstallExpected(context.Background(), spec.Root, protocol.Hash(m)); err != nil {
		t.Fatal("removal retry failed", err)
	}
	// Resolving retained input twice is stable; the input route label is untouched.
	again, err := input.Resolve()
	if err != nil || again.Worker.Routes["ops"] != c.Worker.Routes["ops"] {
		t.Fatal("route resolved more than once", err)
	}
}

func TestManagedBootstrapEnrollmentResumes(t *testing.T) {
	for _, fault := range []string{"stop-returned", "bootstrap-enrolled", "published", "complete"} {
		t.Run(fault, func(t *testing.T) {
			spec, i, services, _ := managedFixture(t)
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
			consumers := input.Worker.Producers
			input.Bootstrap = true
			input.Worker.Producers = nil
			resolved, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			op, _ := safefs.NewID("n")
			reserved, err := worker.BootstrapGateway(context.Background(), spec.State, op, resolved.Worker)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := i.ensureVerification(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			input.Bootstrap = false
			input.Worker.Producers = consumers
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			hit := false
			i.fault = func(stage string) error {
				if stage == fault {
					hit = true
					return fmt.Errorf("interrupted")
				}
				return nil
			}
			after, err := i.prepareExpected(context.Background(), "enroll-bootstrap", spec, protocol.Hash(before))
			if !hit || err == nil {
				t.Fatal("fault boundary not exercised", fault, err)
			}
			i.fault = nil
			after, err = i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			if after.Identity != reserved.Identity || after.Installation != before.Installation || after.Mode != "verification" {
				t.Fatal("enrollment changed gateway authority")
			}
			bound, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			s, err := openManagedStore(after, bound)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.RequireInactiveProducers(); err != nil {
				t.Fatal(err)
			}
			s.Close()
			starts := services.starts
			repeated, e := i.enrollVerificationConsumers(context.Background(), spec, protocol.Hash(before))
			if e != nil || repeated.Revision != after.Revision || services.starts != starts {
				t.Fatal("typed retry changed enrollment", e)
			}
			if _, err = i.resume(context.Background(), spec.Root); err != nil || services.starts != starts {
				t.Fatal("completed enrollment restarted service", err)
			}
		})
	}
}
