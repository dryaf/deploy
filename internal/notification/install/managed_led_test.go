package install

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

type managedTestLED struct {
	states    map[string]led.State
	mutations int
	lostSet   bool
}

func rawManagedRuntime(t *testing.T, path string) Runtime {
	t.Helper()
	raw, err := readManagedInput(path, safefs.MaxRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	var c Runtime
	if err = safefs.DecodeJSON(raw, &c, safefs.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	return c
}

func (a *managedTestLED) List(context.Context) ([]led.Entry, error) {
	var out []led.Entry
	for _, v := range a.states {
		out = append(out, led.Entry{State: v, OwnerUID: os.Getuid()})
	}
	return out, nil
}
func (a *managedTestLED) Status(context.Context) (led.DaemonStatus, error) {
	return led.DaemonStatus{LEDs: map[string]json.RawMessage{"green": json.RawMessage(`{"enabled":true,"available":true,"default_pattern":"off"}`)},
		Patterns: map[string]json.RawMessage{"blink-slow": json.RawMessage(`[]`), "blink-fast": json.RawMessage(`[]`), "heartbeat": json.RawMessage(`[]`), "blink-1": json.RawMessage(`[]`), "blink-2": json.RawMessage(`[]`)}}, nil
}
func (a *managedTestLED) Set(_ context.Context, v led.State) error {
	a.mutations++
	a.states[v.ID] = v
	if a.lostSet {
		a.lostSet = false
		return errors.New("lost daemon set response")
	}
	return nil
}
func (a *managedTestLED) Clear(_ context.Context, id string) error {
	a.mutations++
	delete(a.states, id)
	return nil
}

func managedLEDSetup(t *testing.T) (ManagedSpec, managedInstaller, *fakeManagedServices, *managedTestLED, ManagedManifest, Runtime) {
	t.Helper()
	spec, i, f, _ := managedFixture(t)
	before := rawManagedRuntime(t, spec.RuntimeFile)
	before.LEDSocket = "/private/fixture-led.sock"
	writeRuntime(t, spec.RuntimeFile, before)
	old, err := i.prepare(context.Background(), "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	after := rawManagedRuntime(t, spec.RuntimeFile)
	before, err = ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	after.Worker.LEDBindings = []worker.LEDBinding{{ID: "backup", Producer: after.Worker.Producers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
	writeRuntime(t, spec.RuntimeFile, after)
	api := &managedTestLED{states: map[string]led.State{}}
	i.ledAPI = func(string) worker.LEDAPI { return api }
	i.ledProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	return spec, i, f, api, old, before
}

func TestManagedLEDMappingLifecycle(t *testing.T) {
	spec, i, f, api, old, before := managedLEDSetup(t)
	ctx := context.Background()
	if _, err := i.prepare(ctx, "update", spec); err == nil {
		t.Fatal("ordinary update migrated bindings")
	}
	m, err := i.prepare(ctx, "migrate-led", spec)
	if err != nil {
		t.Fatal(err)
	}
	if m.Identity != old.Identity || m.ConfigHash == old.ConfigHash || m.HelperRevision != m.Revision || m.Mode != "verification" {
		t.Fatal(m)
	}
	checkManagedInstalled(t, spec, m, old.Identity)
	for _, state := range api.states {
		if !strings.HasSuffix(state.ID, ".baseline") || !state.Persistent || state.Priority > 9 {
			t.Fatal("migration enabled positive/fault output", state)
		}
	}
	if len(api.states) != 2 {
		t.Fatal("new baselines missing", api.states)
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	j, err := readManagedJournal(d)
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	after, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.OpenManagedVerification(spec.State, after.Worker, m.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.VerifyManagedLEDMapping(ctx, spec.State, before.Worker, after.Worker, before.LEDSocket, m.Identity, j.Intent.ID); err != nil {
		t.Fatal("read-only verification while worker owns lease", err)
	}
	s.Close()
	starts, calls := f.starts, api.mutations
	again, err := i.resume(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(again) != protocol.Hash(m) || f.starts != starts || api.mutations != calls {
		t.Fatal("completed resume repeated effects")
	}
	if _, err = i.rollback(ctx, spec.Root, old.Revision); err == nil {
		t.Fatal("rollback reversed configuration migration")
	}
	if _, err = i.prepare(ctx, "update", spec); err != nil {
		t.Fatal(err)
	}
	if _, err = i.uninstall(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
}

func TestManagedLEDMappingInterruptions(t *testing.T) {
	for _, point := range []string{"prepared", "stop-returned", "led-migrated", "stopped", "files", "unit-published", "manifest-published", "reloaded", "started", "complete", "daemon"} {
		t.Run(point, func(t *testing.T) {
			spec, i, _, api, old, _ := managedLEDSetup(t)
			i.fault = func(at string) error {
				if at == point {
					return errors.New("interrupted")
				}
				return nil
			}
			api.lostSet = point == "daemon"
			if _, err := i.prepare(context.Background(), "migrate-led", spec); err == nil {
				t.Fatal("interruption missing")
			}
			i.fault = nil
			if point != "complete" {
				if _, err := i.uninstall(context.Background(), spec.Root); err == nil {
					t.Fatal("unfinished migration discarded by uninstall")
				}
			}
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			checkManagedInstalled(t, spec, m, old.Identity)
			calls := api.mutations
			if _, err = i.resume(context.Background(), spec.Root); err != nil {
				t.Fatal(err)
			}
			if calls != api.mutations {
				t.Fatal("completed retry touched daemon")
			}
		})
	}
}

func TestManagedLEDMappingRefusesUnrelatedChangesBeforeStop(t *testing.T) {
	for _, change := range []string{"socket", "provider", "enrollment", "limit", "unchanged", "capability"} {
		t.Run(change, func(t *testing.T) {
			spec, i, f, api, _, _ := managedLEDSetup(t)
			after := rawManagedRuntime(t, spec.RuntimeFile)
			switch change {
			case "socket":
				after.LEDSocket += ".changed"
			case "provider":
				p := after.Providers["telegram"]
				p.ChatFile += ".changed"
				after.Providers["telegram"] = p
			case "enrollment":
				after.Worker.Producers[0].Required = false
			case "limit":
				after.Worker.MaxDatabaseBytes *= 2
			case "unchanged":
				after.Worker.LEDBindings = nil
			case "capability":
				i.ledProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("unsupported") }
			}
			writeRuntime(t, spec.RuntimeFile, after)
			stops := f.stops
			if _, err := i.prepare(context.Background(), "migrate-led", spec); err == nil {
				t.Fatal("unsafe migration accepted")
			}
			if f.stops != stops || api.mutations != 0 {
				t.Fatal("failed preflight changed service or daemon")
			}
		})
	}
}

func TestManagedLEDMappingCompletedEvidenceCannotBeRepairedByResume(t *testing.T) {
	spec, i, _, api, _, _ := managedLEDSetup(t)
	if _, err := i.prepare(context.Background(), "migrate-led", spec); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(spec.State, "state.db")+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE led_migrations SET hash='changed'")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	calls := api.mutations
	if _, err = i.resume(context.Background(), spec.Root); err == nil {
		t.Fatal("damaged completion accepted")
	}
	if calls != api.mutations {
		t.Fatal("damaged completion caused daemon I/O")
	}
}

func TestManagedLEDMappingRefusesRepresentationOnlyChangesBeforeIntent(t *testing.T) {
	for _, change := range []string{"defaults", "binding-order", "subject-order"} {
		t.Run(change, func(t *testing.T) {
			spec, i, f, api, _, _ := managedLEDSetup(t)
			c := rawManagedRuntime(t, spec.RuntimeFile)
			if change == "binding-order" {
				second := c.Worker.LEDBindings[0]
				second.ID = "second"
				c.Worker.LEDBindings = append(c.Worker.LEDBindings, second)
			}
			if change == "subject-order" {
				c.Worker.LEDBindings[0].Subjects = []string{"a", "b"}
			}
			writeRuntime(t, spec.RuntimeFile, c)
			if _, err := i.prepare(context.Background(), "migrate-led", spec); err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(spec.Root, "lifecycle.json")
			old, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "defaults":
				c.Worker.LEDBindings[0].Styles = map[string]worker.LEDStyle{"critical": {Pattern: "blink-fast", Priority: 90}}
			case "binding-order":
				c.Worker.LEDBindings[0], c.Worker.LEDBindings[1] = c.Worker.LEDBindings[1], c.Worker.LEDBindings[0]
			case "subject-order":
				c.Worker.LEDBindings[0].Subjects = []string{"b", "a"}
			}
			writeRuntime(t, spec.RuntimeFile, c)
			stops, calls := f.stops, api.mutations
			if _, err = i.prepare(context.Background(), "migrate-led", spec); err == nil {
				t.Fatal("representation-only migration accepted")
			}
			after, err := os.ReadFile(journal)
			if err != nil {
				t.Fatal(err)
			}
			if string(old) != string(after) || f.stops != stops || api.mutations != calls {
				t.Fatal("no-op migration stranded installation")
			}
		})
	}
}

func TestManagedLEDMappingFaultPrerequisiteDoesNotStopService(t *testing.T) {
	spec, i, f, api, _, _ := managedLEDSetup(t)
	ctx := context.Background()
	m, err := i.prepare(ctx, "migrate-led", spec)
	if err != nil {
		t.Fatal(err)
	}
	// Install a real-format owned fault on the existing binding, then request a
	// changed selector. The existing installation must remain usable on refusal.
	nonce, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	owner := struct{ Worker, Recovery, Binding, Nonce string }{m.Identity.Worker, m.Identity.Recovery, "backup", nonce}
	raw, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(spec.State, "state.db")+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO led_states(id,binding,condition,identity,obsolete) VALUES(?,'backup','critical',?,0)", "deploy.f."+protocol.Hash(owner)[:48], string(raw))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	c := rawManagedRuntime(t, spec.RuntimeFile)
	c.Worker.LEDBindings[0].Subjects = []string{"changed"}
	writeRuntime(t, spec.RuntimeFile, c)
	journal := filepath.Join(spec.Root, "lifecycle.json")
	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	stops, calls := f.stops, api.mutations
	if _, err = i.prepare(ctx, "migrate-led", spec); err == nil {
		t.Fatal("migration ignored unresolved fault")
	}
	after, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || f.stops != stops || api.mutations != calls {
		t.Fatal("fault preflight stranded installation")
	}
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal("old installation cannot be verified", err)
	}
}
