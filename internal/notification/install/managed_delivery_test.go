package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type fakeDeliveryServices struct{ *fakeManagedServices }

func (f *fakeDeliveryServices) Verify(ctx context.Context, m ManagedManifest) error {
	if m.Mode != "delivery" {
		return f.fakeManagedServices.Verify(ctx, m)
	}
	if f.state.Active != "active" {
		return errors.New("delivery process inactive")
	}
	c, err := ReadRuntime(m.Paths.Config)
	if err != nil {
		return err
	}
	s, err := openManagedStore(m, c)
	if err != nil {
		return err
	}
	defer s.Close()
	h, err := s.Health(ctx)
	if err != nil || h.VerificationOnly || h.DeliveryActivationHash != m.DeliveryActivation.Hash {
		return errors.New("delivery health pin differs")
	}
	return nil
}

func deliveryFixture(t *testing.T) (ManagedSpec, managedInstaller, *fakeDeliveryServices, Runtime, ManagedManifest, QualificationRecord) {
	t.Helper()
	spec, i, c, report := qualificationFixture(t)
	qualificationPass(&report)
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDeliveryServices{i.services.(*fakeManagedServices)}
	i.services = f
	i.deliveryProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.deliveryInspect = func(ctx context.Context, m ManagedManifest, _ bool) error {
		_, err := inspectManagedService(ctx, f, m, false)
		return err
	}
	return spec, i, f, c, report.Binding.Manifest, *status.Record
}

func TestManagedDeliveryLifecycle(t *testing.T) {
	spec, i, f, c, before, record := deliveryFixture(t)
	ctx := context.Background()
	starts, stops := f.starts, f.stops
	after, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode != "delivery" || after.Revision != before.Revision || after.Identity != before.Identity || after.ConfigHash != before.ConfigHash || after.DeliveryActivation == nil || after.DeliveryOwnerHash == "" || f.starts != starts+1 || f.stops != stops+1 {
		t.Fatal(after)
	}
	unit, err := os.ReadFile(filepath.Join(spec.UnitDirectory, managedUnit))
	if err != nil || !strings.Contains(string(unit), "AF_UNIX AF_INET AF_INET6") {
		t.Fatal("delivery unit networking", err)
	}
	s, err := openManagedStore(after, c)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.OwnershipStatus(c.Worker.Producers[0].Producer, "one")
	if err != nil || status.Active || status.State != "staged" {
		t.Fatal("global activation changed producer generation", status, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if rogue, err := worker.Open(spec.State, c.Worker, false); err == nil {
		rogue.Close()
		t.Fatal("unmanaged runtime bypassed enrollment")
	}
	if _, err := worker.OpenVerification(spec.State, c.Worker); err == nil {
		t.Fatal("unmanaged verification bypassed retained owner")
	}
	stops = f.stops
	if got, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false); err != nil || protocol.Hash(got) != protocol.Hash(after) || stops != f.stops {
		t.Fatal("exact retry changed activation", err)
	}
	if _, err := i.prepare(ctx, "update", spec); err == nil {
		t.Fatal("active delivery update accepted")
	}
	if _, err := i.uninstall(ctx, spec.Root); err == nil {
		t.Fatal("active delivery uninstall accepted")
	}
	if _, _, _, err := reviewManagedRollback(spec.Root, before.Revision); err == nil {
		t.Fatal("active rollback accepted")
	}
	// Deactivation remains possible after credentials disappear or evidence expires.
	if err := os.Remove(c.Providers["telegram"].TokenFile); err != nil {
		t.Fatal(err)
	}
	deactivated, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(after), "", false)
	if err != nil {
		t.Fatal(err)
	}
	if deactivated.Mode != "verification" || deactivated.DeliveryActivation != nil || deactivated.DeliveryOwnerHash != after.DeliveryOwnerHash {
		t.Fatal(deactivated)
	}
	if _, err := i.resume(ctx, spec.Root); err != nil {
		t.Fatal("deactivated resume", err)
	}
	binary, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	binary = append(binary, []byte("\n# delivery update\n")...)
	if err := os.WriteFile(spec.BinaryFile, binary, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(binary)
	if _, err := i.prepare(ctx, "update", spec); err != nil {
		t.Fatal("verification update lost enrollment", err)
	}
	rolled, err := i.rollback(ctx, spec.Root, before.Revision)
	if err != nil || rolled.BinaryHash != before.BinaryHash || rolled.DeliveryOwnerHash != after.DeliveryOwnerHash || rolled.Mode != "verification" {
		t.Fatal("verification rollback lost enrollment", err)
	}
	if _, err := i.uninstall(ctx, spec.Root); err != nil {
		t.Fatal("uninstall lost enrollment", err)
	}
	if rogue, err := worker.Open(spec.State, c.Worker, false); err == nil {
		rogue.Close()
		t.Fatal("uninstall removed managed ownership")
	}
}

func TestManagedDeliveryInterruptedAndCancel(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "delivery-enrolled", "stopped", "unit-published", "manifest-published", "reloaded", "start-returned", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			spec, i, _, _, before, record := deliveryFixture(t)
			interrupted := errors.New("delivery interruption")
			i.fault = func(at string) error {
				if at == boundary {
					return interrupted
				}
				return nil
			}
			if _, err := i.prepareDelivery(context.Background(), "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false); !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			i.fault = nil
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil || m.Mode != "delivery" {
				t.Fatal("delivery resume", m, err)
			}
		})
	}
	for _, boundary := range []string{"prepared", "delivery-enrolled", "unit-published", "manifest-published", "reloaded"} {
		t.Run("cancel-"+boundary, func(t *testing.T) {
			spec, i, _, c, before, record := deliveryFixture(t)
			i.fault = func(at string) error {
				if at == boundary {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := i.prepareDelivery(context.Background(), "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false); err == nil {
				t.Fatal("missing interruption")
			}
			i.fault = nil
			d, err := privateManagedDir(spec.Root, true)
			if err != nil {
				t.Fatal(err)
			}
			j, err := readManagedJournal(d)
			d.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(c.Providers["telegram"].TokenFile); err != nil {
				t.Fatal(err)
			}
			m, err := i.prepareDelivery(context.Background(), "deactivate-delivery", spec.Root, protocol.Hash(j.Intent.After), "", false)
			if err != nil || m.Mode != "verification" {
				t.Fatal("could not cancel activation without credentials", err)
			}
			if _, err := i.resume(context.Background(), spec.Root); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedDeliveryRejectsQualificationAndCapability(t *testing.T) {
	for _, kind := range []string{"manifest", "record", "token", "destination", "pending", "failed", "capability", "helper"} {
		t.Run(kind, func(t *testing.T) {
			spec, i, f, c, before, record := deliveryFixture(t)
			manifestHash, qualificationHash := protocol.Hash(before), protocol.Hash(record)
			switch kind {
			case "manifest":
				manifestHash = protocol.Hash("other")
			case "record":
				qualificationHash = protocol.Hash("other")
			case "token":
				if err := os.WriteFile(c.Providers["telegram"].TokenFile, []byte("123:rotated"), 0600); err != nil {
					t.Fatal(err)
				}
			case "destination":
				if err := os.WriteFile(c.Providers["telegram"].ChatFile, []byte("other-chat"), 0600); err != nil {
					t.Fatal(err)
				}
			case "capability", "helper":
				i.deliveryProbe = func(_ context.Context, _ *safefs.Directory, name, _ string) error {
					if kind == "capability" || filepath.Base(name) == "recovery-helper" {
						return errors.New("old executable")
					}
					return nil
				}
			case "pending", "failed":
				report := record.Report
				report.Checks = append([]QualificationCheck(nil), report.Checks...)
				report.ID, _ = safefs.NewID("n")
				if kind == "failed" {
					report.Checks[0].Result = "failed"
					status, err := qualifyFixture(t, spec.Root, report)
					if err != nil {
						t.Fatal(err)
					}
					qualificationHash = protocol.Hash(*status.Record)
				} else {
					d, err := privateManagedDir(spec.Root, true)
					if err != nil {
						t.Fatal(err)
					}
					_, err = appendQualification(d, report, time.Now().UTC(), func(string) error { return errors.New("unacknowledged report") })
					d.Close()
					if err == nil {
						t.Fatal("pending report fixture did not interrupt")
					}
				}
			}
			stops := f.stops
			if _, err := i.prepareDelivery(context.Background(), "activate-delivery", spec.Root, manifestHash, qualificationHash, false); err == nil || f.stops != stops {
				t.Fatal("invalid activation accepted or stopped gateway", err)
			}
		})
	}
}

func TestManagedDeliveryCLIValidation(t *testing.T) {
	var out bytes.Buffer
	if err := runDeliveryCLI([]string{"activate-delivery", "--root", "/tmp/absent"}, &out); err == nil || out.Len() != 0 {
		t.Fatal("invalid CLI accepted")
	}
	spec, _, _, _, manifest, record := deliveryFixture(t)
	before := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	if err := runDeliveryCLI([]string{"activate-delivery", "--local", "--root", spec.Root, "--manifest-hash", protocol.Hash(manifest), "--qualification-hash", protocol.Hash(record)}, &out); err != nil {
		t.Fatal(err)
	}
	if before != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) || !strings.Contains(out.String(), "[DRY]") {
		t.Fatal("dry-run changed retained installation or state")
	}
}

func TestManagedDeliveryQualificationTimeAndSealedLoss(t *testing.T) {
	spec, i, _, c, before, record := deliveryFixture(t)
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, at := range []time.Time{record.Report.CreatedAt.Add(qualificationWindow), record.RecordedAt.Add(-time.Nanosecond)} {
		if err := checkDeliveryQualification(d, before, c, record, at); err == nil {
			t.Fatal("expired/future qualification admitted")
		}
	}
	i.fault = func(at string) error {
		if at == "stopped" {
			return errors.New("sealed interruption")
		}
		return nil
	}
	if _, err := i.prepareDelivery(context.Background(), "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false); err == nil {
		t.Fatal("missing sealed interruption")
	}
	i.fault = nil
	state, err := safefs.Open(spec.State)
	if err != nil {
		t.Fatal(err)
	}
	// Completed enrollment cannot be silently reconstructed during publication retry.
	if err := os.Remove(filepath.Join(spec.State, "managed-delivery-owner.json")); err != nil {
		t.Fatal(err)
	}
	state.Close()
	if _, err := i.resume(context.Background(), spec.Root); err == nil {
		t.Fatal("sealed owner loss was repaired")
	}
}

func TestManagedDeliverySendGate(t *testing.T) {
	calls := 0
	failed := false
	adapter := admittedDeliverySender{check: func(context.Context) error { return errors.New("expired admission") }, fail: func(error) { failed = true }, sender: runtimeTestSender(func(context.Context, string, protocol.Message) worker.Result {
		calls++
		return worker.Result{Accepted: true}
	})}
	result := adapter.Send(context.Background(), "telegram", protocol.Message{})
	if calls != 0 || !failed || result.Accepted || result.Ambiguous || result.Permanent || result.RetryAfter <= 0 {
		t.Fatal("failed admission made an external attempt or terminal claim", result)
	}
}

func TestManagedDeliveryReactivation(t *testing.T) {
	spec, i, f, _, before, record := deliveryFixture(t)
	ctx := context.Background()
	active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(active), "", false)
	if err != nil {
		t.Fatal(err)
	}
	stops := f.stops
	if _, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(verification), protocol.Hash(record), false); err == nil || f.stops != stops {
		t.Fatal("initial enrollment's changed manifest reused old qualification", err)
	}
	var out bytes.Buffer
	if err := runQualificationCLI([]string{"qualification-template", "--local", "--root", spec.Root}, &out); err != nil {
		t.Fatal(err)
	}
	var report QualificationReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	qualificationPass(&report)
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil {
		t.Fatal(err)
	}
	reactivated, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(verification), protocol.Hash(*status.Record), false)
	if err != nil || reactivated.DeliveryOwnerHash != active.DeliveryOwnerHash || reactivated.DeliveryActivation.Hash == active.DeliveryActivation.Hash || reactivated.Revision != active.Revision {
		t.Fatal("reactivation did not retain owner with a new exact admission", err)
	}
	if _, err := i.resume(ctx, spec.Root); err != nil {
		t.Fatal("reactivated resume", err)
	}
}
