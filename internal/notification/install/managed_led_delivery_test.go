package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

type deliveryTestLED struct{ *managedTestLED }

func (a deliveryTestLED) List(context.Context) ([]led.Entry, error) {
	winners := map[string]string{}
	for id, state := range a.states {
		old := winners[state.LED]
		if old == "" || state.Priority > a.states[old].Priority || state.Priority == a.states[old].Priority && id < old {
			winners[state.LED] = id
		}
	}
	var out []led.Entry
	for id, state := range a.states {
		out = append(out, led.Entry{State: state, OwnerUID: os.Getuid(), Winner: winners[state.LED] == id})
	}
	return out, nil
}
func (a deliveryTestLED) Status(ctx context.Context) (led.DaemonStatus, error) {
	status, _ := a.managedTestLED.Status(ctx)
	entries, _ := a.List(ctx)
	for _, entry := range entries {
		if entry.Winner {
			status.LEDs[entry.LED], _ = json.Marshal(map[string]any{"enabled": true, "available": true, "default_pattern": "off", "selected_state": entry})
		}
	}
	return status, nil
}

func managedLEDDeliveryFixture(t *testing.T) (ManagedSpec, managedInstaller, *fakeDeliveryServices, Runtime, ManagedManifest, QualificationRecord) {
	return managedLEDDeliveryFixtureUsing(t, nil, false)
}
func managedLEDDeliveryFixtureUsing(t *testing.T, configure func(*Runtime), disabled bool) (ManagedSpec, managedInstaller, *fakeDeliveryServices, Runtime, ManagedManifest, QualificationRecord) {
	t.Helper()
	spec, i, f, _ := managedFixture(t)
	qualificationFixtureInputs(t, spec.RuntimeFile)
	c := rawManagedRuntime(t, spec.RuntimeFile)
	if configure != nil {
		configure(&c)
	}
	c.LEDSocket = "/private/fixture-led.sock"
	writeRuntime(t, spec.RuntimeFile, c)
	m, err := i.prepare(context.Background(), "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	c.Worker.LEDBindings = []worker.LEDBinding{{ID: "backup", Producer: c.Worker.Producers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
	writeRuntime(t, spec.RuntimeFile, c)
	i.ledAPI = func(string) worker.LEDAPI { return &managedTestLED{states: map[string]led.State{}} }
	i.ledProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	m, err = i.prepare(context.Background(), "migrate-led", spec)
	if err != nil {
		t.Fatal(err)
	}
	i.ledOutputProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	if disabled {
		m, err = i.prepareLEDOutput(context.Background(), spec.Root, protocol.Hash(m), true)
		if err != nil {
			t.Fatal(err)
		}
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
	report.Environment.LEDVersion = "synthetic-v1"
	report.Environment.LEDConfigHash = protocol.Hash("synthetic LED configuration")
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil {
		t.Fatal(err)
	}
	delivery := &fakeDeliveryServices{f}
	i.services = delivery
	i.deliveryProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.ledDeliveryProbe = i.deliveryProbe
	i.deliveryInspect = func(ctx context.Context, m ManagedManifest, _ bool) error {
		_, err := inspectManagedService(ctx, delivery, m, false)
		return err
	}
	return spec, i, delivery, mustRuntime(t, spec.RuntimeFile), m, *status.Record
}

func TestManagedLEDDeliveryAdmissionAndRevocation(t *testing.T) {
	spec, i, _, c, before, record := managedLEDDeliveryFixture(t)
	ctx := context.Background()
	m, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openManagedStore(m, c)
	if err != nil {
		t.Fatal(err)
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	api := &managedTestLED{states: map[string]led.State{"foreign.application": {ID: "foreign.application", LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}}}
	var failure error
	guard := admittedLEDClient{api: deliveryTestLED{api}, check: func(context.Context) error { return checkDeliveryAdmission(d, m, c, time.Now()) }, fail: func(err error) { failure = err }}
	r := worker.LEDReconciler{Store: s, API: guard, Budget: 2 * time.Second}
	if err = r.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if api.mutations == 0 {
		t.Fatal("admitted LED runtime did not reconcile")
	}
	if _, ok := api.states["foreign.application"]; !ok {
		t.Fatal("foreign state cleared")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(m), "", false); err != nil {
		t.Fatal(err)
	}
	// The caller's current-manifest guard is independently necessary. The old
	// qualification alone is historical evidence and must not authorize a send.
	guard.check = func(context.Context) error {
		var current ManagedManifest
		if err := d.ReadJSON("installation.json", &current); err != nil {
			return err
		}
		if protocol.Hash(current) != protocol.Hash(m) {
			return errors.New("activation revoked")
		}
		return nil
	}
	mutations := api.mutations
	if err = guard.Clear(ctx, "foreign.application"); err == nil || failure == nil || api.mutations != mutations {
		t.Fatal("revoked admission reached daemon")
	}
}

func TestManagedLEDDeliveryRequiresCapableHelper(t *testing.T) {
	spec, i, f, _, before, record := managedLEDDeliveryFixture(t)
	i.ledDeliveryProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old helper") }
	stops := f.stops
	if _, err := i.prepareDelivery(context.Background(), "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(record), false); err == nil || f.stops != stops {
		t.Fatal("incapable helper admitted or service stopped", err)
	}
}

func TestManagedLEDAdmissionBudget(t *testing.T) {
	api := &managedTestLED{states: map[string]led.State{}}
	ctx, cancel := context.WithCancel(context.Background())
	a := admittedLEDClient{api: api, check: func(context.Context) error { cancel(); return nil }, fail: func(error) { t.Fatal("request cancellation killed runtime") }}
	if err := a.Set(ctx, led.State{ID: "healthy"}); !errors.Is(err, context.Canceled) || api.mutations != 0 {
		t.Fatal("late admission reached daemon", err)
	}
}
