//go:build notificationintegration

package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

func TestNativeManagedLEDDelivery(t *testing.T) {
	socket := ledfixture.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	daemon := led.Client{Socket: socket, Budget: time.Second}
	foreign := led.State{ID: "other.application", LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true}
	if err := daemon.Set(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	spec, i, f, _ := managedFixture(t)
	qualificationFixtureInputs(t, spec.RuntimeFile)
	c := rawManagedRuntime(t, spec.RuntimeFile)
	c.LEDSocket = socket
	writeRuntime(t, spec.RuntimeFile, c)
	spec.BinarySHA256 = managedDigest(buildSignalCommandFixture(t, "./cmd/deploy", spec.BinaryFile))
	native := &nativeDeliveryServices{executableManagedServices: &executableManagedServices{fakeManagedServices: f, root: spec.Root}, t: t}
	t.Cleanup(func() {
		if native.cmd != nil {
			_ = native.Stop(context.Background())
		}
	})
	i.services, i.probe = native, probeManagedBinary
	i.deliveryInspect = func(ctx context.Context, m ManagedManifest, stale bool) error {
		_, err := native.inspector(m).inspect(ctx, m, stale)
		return err
	}
	before, err := i.prepare(ctx, "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = runQualificationCLI([]string{"qualification-template", "--local", "--root", spec.Root}, &out); err != nil {
		t.Fatal(err)
	}
	var report QualificationReport
	if err = json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	qualificationPass(&report)
	report.Environment.LEDVersion = ledfixture.Commit
	report.Environment.LEDConfigHash = protocol.Hash("offline mock hardware fixture")
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil {
		t.Fatal(err)
	}
	m, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(*status.Record), false)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Worker.Producers[0].Producer
	request := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Producer: p, Generation: "one", Route: "ops"}
	if err = i.withManagedProducer(ctx, request, func(s ManagedProducerSession) error { return s.Activate() }); err != nil {
		t.Fatal(err)
	}
	api := client.Client{Socket: m.Paths.Socket, Identity: m.Identity}
	now := time.Now().UTC()
	snapshot := protocol.Snapshot{Producer: p, Sequence: 1, BasisRevision: 1, ConfigRevision: "native", EvaluatedAt: now, Subjects: []protocol.Subject{{Key: "fixture", Condition: "ok", Reason: "synthetic", ValidUntil: now.Add(time.Minute)}}}
	if _, err = api.Call(ctx, protocol.Request{Operation: "status.replace", Producer: p, Snapshot: &snapshot}, nil); err != nil {
		t.Fatal(err)
	}
	await := func(healthy bool) {
		t.Helper()
		for deadline := time.Now().Add(12 * time.Second); ; {
			entries, err := daemon.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			found, foreignFound := false, false
			for _, e := range entries {
				if e.ID == foreign.ID {
					foreignFound = protocol.Hash(e.State) == protocol.Hash(foreign)
				}
				if strings.HasSuffix(e.ID, ".healthy") && e.TTL != nil && *e.TTL > 0 {
					found = true
				}
			}
			if !foreignFound {
				t.Fatal("managed LED changed foreign registry state")
			}
			if found == healthy {
				return
			}
			if time.Now().After(deadline) {
				raw, _ := os.ReadFile(spec.Root + "/../runtime-output")
				t.Fatal("LED lease did not follow managed admission", healthy, entries, string(raw))
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	await(true)
	request.Revoke = true
	if err = i.withManagedProducer(ctx, request, func(s ManagedProducerSession) error { return s.Revoke(ctx) }); err != nil {
		t.Fatal(err)
	}
	if _, err = i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(m), "", false); err != nil {
		t.Fatal(err)
	}
	await(false)
}
