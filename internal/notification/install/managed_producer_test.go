package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type producerTestServices struct{ *fakeDeliveryServices }

func (s producerTestServices) Verify(ctx context.Context, m ManagedManifest) error {
	if s.state.Active != "active" {
		return errors.New("inactive")
	}
	c, err := ReadRuntime(m.Paths.Config)
	if err != nil {
		return err
	}
	store, err := openManagedStore(m, c)
	if err != nil {
		return err
	}
	return store.Close()
}
func TestManagedProducerCutoverResumeAndRevoke(t *testing.T) {
	spec, i, f, c, before, record := deliveryFixture(t)
	ctx := context.Background()
	m, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	i.services = producerTestServices{f}
	r := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Producer: c.Worker.Producers[0].Producer, Generation: "cutover-one", Route: c.Worker.Producers[0].Routes[0]}
	stop := errors.New("crash after activation")
	err = i.withManagedProducer(ctx, r, func(s ManagedProducerSession) error {
		if err := s.Stage(); err != nil {
			return err
		}
		if err := s.Activate(); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) || f.state.Active != "inactive" {
		t.Fatal("failed transaction resumed dispatcher", err)
	}
	var retained ManagedProducerSession
	err = i.withManagedProducer(ctx, r, func(s ManagedProducerSession) error {
		retained = s
		if err := s.Stage(); err != nil {
			return err
		}
		return s.Activate()
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = retained.Activate(); err == nil {
		t.Fatal("closed session retained activation authority")
	}
	s, err := openManagedStore(m, c)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.OwnershipStatus(r.Producer, "one")
	if err != nil || status.State != "staged" {
		t.Fatal("unrelated generation changed", status, err)
	}
	if err = s.RequireInactiveProducers(); err == nil {
		t.Fatal("lifecycle accepted active producer")
	}
	s.Close()
	if err = os.Remove(c.Providers["telegram"].TokenFile); err != nil {
		t.Fatal(err)
	}
	r.Revoke = true
	if err = i.withManagedProducer(ctx, r, func(s ManagedProducerSession) error { return s.Revoke(ctx) }); err != nil {
		t.Fatal(err)
	}
	s, err = openManagedStore(m, c)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	status, err = s.OwnershipStatus(r.Producer, r.Generation)
	if err != nil || status.State != "revoked" || status.Active {
		t.Fatal(status, err)
	}
	if err = s.Stage(r.Producer, r.Generation); err == nil {
		t.Fatal("reused tombstone")
	}
}
func TestManagedProducerPinsBeforeStop(t *testing.T) {
	spec, i, f, c, before, record := deliveryFixture(t)
	ctx := context.Background()
	m, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	r := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Producer: c.Worker.Producers[0].Producer, Generation: "cutover", Route: c.Worker.Producers[0].Routes[0]}
	stops := f.stops
	for _, mutate := range []func(*ManagedProducerRequest){func(r *ManagedProducerRequest) { r.ManifestHash = protocol.Hash("wrong") }, func(r *ManagedProducerRequest) { r.Route = "foreign" }, func(r *ManagedProducerRequest) { r.Producer.Epoch = "foreign" }} {
		bad := r
		mutate(&bad)
		if err = i.withManagedProducer(ctx, bad, func(ManagedProducerSession) error { t.Fatal("callback admitted"); return nil }); err == nil {
			t.Fatal("bad pin accepted")
		}
		if f.stops != stops {
			t.Fatal("bad request stopped service")
		}
	}
}

func TestManagedProducerCutoverPreservesOtherProducer(t *testing.T) {
	ctx := context.Background()
	spec, i, f, _ := managedFixture(t)
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	other := c.Worker.Producers[0]
	other.Producer.ID = "other-producer"
	c.Worker.Producers = append(c.Worker.Producers, other)
	writeRuntime(t, spec.RuntimeFile, c)
	qualificationFixtureInputs(t, spec.RuntimeFile)
	c, err = ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	spec.State = t.TempDir()
	if err = os.Chmod(spec.State, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := worker.Open(spec.State, c.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err = i.prepare(ctx, "setup", spec); err != nil {
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
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil {
		t.Fatal(err)
	}
	delivery := &fakeDeliveryServices{f}
	i.services = delivery
	i.deliveryProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.deliveryInspect = func(context.Context, ManagedManifest, bool) error { return nil }
	m, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(report.Binding.Manifest), protocol.Hash(*status.Record), false)
	if err != nil {
		t.Fatal(err)
	}
	i.services = producerTestServices{delivery}
	r := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Producer: other.Producer, Generation: "other-live", Route: "ops"}
	activate := func(s ManagedProducerSession) error {
		if err := s.Stage(); err != nil {
			return err
		}
		return s.Activate()
	}
	if err = i.withManagedProducer(ctx, r, activate); err != nil {
		t.Fatal(err)
	}
	store, err = openManagedStore(m, c)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = store.Replace(ctx, protocol.Snapshot{Producer: other.Producer, Sequence: 1, BasisRevision: 1, ConfigRevision: "one", EvaluatedAt: now, Subjects: []protocol.Subject{{Key: "subject", Condition: "ok", Reason: "fixture", ValidUntil: now.Add(time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	message := protocol.Message{Producer: other.Producer, Sequence: 1, ID: "other-message", Generation: r.Generation, Subject: "subject", Kind: "test", Severity: "warning", ObservedAt: now, Text: "other queue stays owned", Route: "ops"}
	if _, err = store.Enqueue(ctx, message); err != nil {
		t.Fatal(err)
	}
	beforeQueue, err := store.InspectQueue(ctx, other.Producer, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	ownerPath := filepath.Join(spec.State, "owner-"+safefs.LockName(other.Producer.ID)+".json")
	beforeOwner, err := os.ReadFile(ownerPath)
	if err != nil {
		t.Fatal(err)
	}
	r.Producer = c.Worker.Producers[0].Producer
	r.Generation = "backup-live"
	if err = i.withManagedProducer(ctx, r, activate); err != nil {
		t.Fatal(err)
	}
	r.Revoke = true
	if err = i.withManagedProducer(ctx, r, func(s ManagedProducerSession) error { return s.Revoke(ctx) }); err != nil {
		t.Fatal(err)
	}
	afterOwner, err := os.ReadFile(ownerPath)
	if err != nil || !bytes.Equal(beforeOwner, afterOwner) {
		t.Fatal("unrelated ownership changed", err)
	}
	store, err = openManagedStore(m, c)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	afterQueue, err := store.InspectQueue(ctx, other.Producer, 0, 100)
	if err != nil || protocol.Hash(beforeQueue) != protocol.Hash(afterQueue) {
		t.Fatal("unrelated queue changed", err)
	}
}

func TestManagedProducerRejectsReplacedWorkerLease(t *testing.T) {
	spec, i, _, c, before, record := deliveryFixture(t)
	ctx := context.Background()
	m, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	r := ManagedProducerRequest{Root: spec.Root, ManifestHash: protocol.Hash(m), Producer: c.Worker.Producers[0].Producer, Generation: "lease-test", Route: c.Worker.Producers[0].Routes[0]}
	err = i.withManagedProducer(ctx, r, func(s ManagedProducerSession) error {
		if err := s.Stage(); err != nil {
			return err
		}
		path := filepath.Join(spec.State, "worker.lock")
		if err := os.Rename(path, path+".held"); err != nil {
			return err
		}
		defer os.Rename(path+".held", path)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			return err
		}
		err := s.Activate()
		if err == nil {
			t.Fatal("replaced worker lease authorized activation")
		}
		return err
	})
	if err == nil {
		t.Fatal("lost lease operation succeeded")
	}
	store, err := openManagedStore(m, c)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	status, err := store.OwnershipStatus(r.Producer, r.Generation)
	if err != nil || status.State != "staged" {
		t.Fatal(status, err)
	}
}
