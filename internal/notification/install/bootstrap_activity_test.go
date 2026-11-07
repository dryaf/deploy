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

func TestRestoreUnboundVerificationRetainsIdentity(t *testing.T) {
	for _, boundary := range []string{"", "stop-returned", "published", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
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
			input.Bootstrap, input.Worker.Producers = true, nil
			c, err := input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			op, _ := safefs.NewID("n")
			if _, err = worker.BootstrapGateway(ctx, spec.State, op, c.Worker); err != nil {
				t.Fatal(err)
			}
			raw, _ = input.Bytes()
			if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := i.ensureVerification(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			stopped, err := i.uninstallExpected(ctx, spec.Root, protocol.Hash(before))
			if err != nil {
				t.Fatal(err)
			}
			starts := services.starts
			if _, err = i.restoreBootstrapVerification(ctx, spec.Root, strings.Repeat("a", 64)); err == nil || services.starts != starts {
				t.Fatal("stale restore started service")
			}
			hit := false
			i.fault = func(stage string) error {
				if stage == boundary && boundary != "" {
					hit = true
					return fmt.Errorf("lost activity reply")
				}
				return nil
			}
			after, err := i.restoreBootstrapVerification(ctx, spec.Root, protocol.Hash(stopped))
			if boundary != "" && (!hit || err == nil) {
				t.Fatal("restore boundary not exercised", boundary, err)
			}
			if boundary == "" && err != nil {
				t.Fatal(err)
			}
			i.fault = nil
			after, err = i.restoreBootstrapVerification(ctx, spec.Root, protocol.Hash(stopped))
			if err != nil {
				t.Fatal(err)
			}
			if after.Identity != before.Identity || after.Installation != before.Installation || after.ConfigHash != before.ConfigHash || after.RuntimeHash != before.RuntimeHash || after.BinaryHash != before.BinaryHash || after.Mode != "verification" || after.Revision == before.Revision {
				t.Fatal("restore changed authority or failed to stage service")
			}
			starts = services.starts
			for range 2 {
				again, e := i.restoreBootstrapVerification(ctx, spec.Root, protocol.Hash(stopped))
				if e != nil || protocol.Hash(again) != protocol.Hash(after) || services.starts != starts {
					t.Fatal("restore retry changed service", e)
				}
			}
		})
	}
}

func TestRestoreVerificationRefusesBoundGateway(t *testing.T) {
	ctx := context.Background()
	spec, i, services, _ := managedFixture(t)
	m, err := i.prepare(ctx, "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	m, err = i.uninstallExpected(ctx, spec.Root, protocol.Hash(m))
	if err != nil {
		t.Fatal(err)
	}
	starts := services.starts
	if _, err = i.restoreBootstrapVerification(ctx, spec.Root, protocol.Hash(m)); err == nil || services.starts != starts {
		t.Fatal("bound gateway restored without dependency lifecycle")
	}
}
