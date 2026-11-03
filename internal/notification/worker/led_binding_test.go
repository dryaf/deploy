package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestLEDBindingsMaximumPassAndUnavailablePatternIsolation(t *testing.T) {
	bindings := make([]LEDBinding, 32)
	for i := range bindings {
		bindings[i] = binding(fmt.Sprintf("selected-%02d", i), "backup")
	}
	bindings[0].Styles = map[string]LEDStyle{"critical": {"not-installed", 95}}
	s, now, _ := bindingFixture(t, bindings...)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	api := &fakeLED{states: map[string]led.State{}}
	r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	if r.Step(ctx) == nil {
		t.Fatal("missing configured pattern accepted")
	}
	h, err := s.Health(ctx)
	must(t, err)
	if len(h.LEDOutputs) != 32 || h.LEDOutputs[bindings[0].ID].RegistryAccepted {
		t.Fatal("missing binding diagnostics")
	}
	for _, b := range bindings[1:] {
		if !h.LEDOutputs[b.ID].RegistryAccepted {
			t.Fatal("one binding blocked another output", b.ID, h.LEDOutputs[b.ID])
		}
	}
	if len(api.states) > 128 {
		t.Fatal("binding pass exceeded per-UID quota")
	}
}

func TestLEDAllSubjectsRetiredClearsFaultsWithoutHealthyLease(t *testing.T) {
	s, now, _ := bindingFixture(t, binding("selected", "backup"))
	ctx := context.Background()
	observe, err := s.MonitorRuntimeLoop("command-conversion")
	must(t, err)
	observe(nil)
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	api := &fakeLED{states: map[string]led.State{}}
	r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	root, selected := bindingFault(t, s, workerLEDBinding, "critical"), bindingFault(t, s, "selected", "critical")
	v.Sequence, v.BasisRevision = 2, 2
	v.Subjects[0].Condition = "retired"
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "retired" || h.Bindings["selected"].Condition != "retired" {
		t.Fatal("runtime liveness converted retirement into healthy evidence", h)
	}
	for _, id := range []string{root, selected} {
		if _, exists := api.states[id]; exists {
			t.Fatal("explicit retirement left an old fault", id)
		}
	}
	for _, state := range api.states {
		if state.Pattern == "heartbeat" {
			t.Fatal("retirement enabled a healthy lease")
		}
	}
}

func TestLEDBindingRetirementSurvivesLaterProducerEvidence(t *testing.T) {
	s, now, _ := bindingFixture(t, binding("all", "*"), binding("retired", "backup"))
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "retired"
	v.Subjects[0].ValidUntil = now.Add(10 * time.Second)
	must(t, s.Replace(ctx, v))
	*now = now.Add(20 * time.Second)
	v = snapshot(*now, 2, 2)
	v.Subjects[0].Key = "other"
	must(t, s.Replace(ctx, v))
	h, err := s.Health(ctx)
	must(t, err)
	if h.Bindings["all"].Condition != "ok" || h.Bindings["retired"].Condition != "retired" {
		t.Fatal("historical retirement became missing evidence", h)
	}
}

func TestLEDBindingWildcardBoundsStatusWithoutHidingCritical(t *testing.T) {
	now := time.Now()
	var subjects []protocol.Subject
	for i := 0; i < 200; i++ {
		subjects = append(subjects, protocol.Subject{Key: fmt.Sprintf("subject-%03d-%s", i, strings.Repeat("a", 200)), Condition: "ok", Reason: strings.Repeat("detail", 40), ValidUntil: now.Add(time.Hour)})
	}
	h := selectLEDBinding(binding("all", "*"), subjects, now)
	if h.Condition != "unknown" || !h.Overflow || len(h.evidence) != protocol.MaxSubjects || h.OmittedSubjects != len(subjects)-len(h.Subjects) {
		t.Fatal("oversized wildcard implied complete healthy coverage", h)
	}
	raw, err := json.Marshal(h)
	must(t, err)
	if len(raw) > 1024 {
		t.Fatal("binding status repeated unbounded subject details", len(raw))
	}
	subjects[len(subjects)-1].Condition = "critical"
	h = selectLEDBinding(binding("all", "*"), subjects, now)
	if h.Condition != "critical" {
		t.Fatal("omitted critical fault disappeared")
	}
}

func bindingFixture(t *testing.T, bindings ...LEDBinding) (*Store, *time.Time, string) {
	t.Helper()
	c := configuration()
	c.LEDBindings = bindings
	path := t.TempDir()
	must(t, os.Chmod(path, 0700))
	s, err := Open(path, c, true)
	must(t, err)
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	must(t, s.Stage(producer, "generation-one"))
	must(t, s.Activate(producer, "generation-one"))
	return s, &now, path
}

func binding(id string, subjects ...string) LEDBinding {
	return LEDBinding{ID: id, Producer: producer, Subjects: subjects, LED: "green"}
}

func bindingFault(t *testing.T, s *Store, name, condition string) string {
	t.Helper()
	var id string
	must(t, s.db.QueryRow("SELECT id FROM led_states WHERE binding=? AND condition=? AND obsolete=0", name, condition).Scan(&id))
	return id
}

func TestLEDBindingsScopeResolutionAndRetirement(t *testing.T) {
	second := binding("second", "other")
	second.LED, second.AllowPower = "red", true
	s, now, _ := bindingFixture(t, binding("first", "backup"), second)
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	v.Subjects = append(v.Subjects, protocol.Subject{Key: "other", Condition: "critical", Reason: "other failure", ValidUntil: now.Add(time.Hour)})
	must(t, s.Replace(ctx, v))
	api := &fakeLED{states: map[string]led.State{}}
	r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	first, other := bindingFault(t, s, "first", "critical"), bindingFault(t, s, "second", "critical")
	if first == other || len(first) > 64 || api.states[other].LED != "red" {
		t.Fatal("binding namespaces or targets not isolated")
	}
	v.Sequence, v.BasisRevision = 2, 2
	v.Subjects[0].Condition = "ok"
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	if _, ok := api.states[first]; ok {
		t.Fatal("resolved binding retained its old fault")
	}
	if _, ok := api.states[other]; !ok {
		t.Fatal("one binding cleared another binding's fault")
	}
	h, err := s.Health(ctx)
	must(t, err)
	if h.Bindings["first"].Condition != "ok" || h.Bindings["second"].Condition != "critical" || h.LEDOutputs["first"].StateID == h.LEDOutputs["second"].StateID {
		t.Fatal(h)
	}
	v.Sequence, v.BasisRevision = 3, 3
	v.Subjects[1].Condition = "retired"
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	if _, ok := api.states[other]; ok {
		t.Fatal("explicit retirement did not retire the exact old fault")
	}
	_, healthy := (&LEDReconciler{Store: s, binding: &second}).ids()
	if _, ok := api.states[healthy]; ok {
		t.Fatal("retirement became healthy evidence")
	}
	v.Sequence, v.BasisRevision = 4, 4
	v.Subjects[1].Condition = "critical"
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	if bindingFault(t, s, "second", "critical") == other {
		t.Fatal("new fault episode reused a cleared incarnation")
	}
}

func TestLEDBindingsMissingSelectorsAndFreshness(t *testing.T) {
	s, now, _ := bindingFixture(t, binding("exact", "backup"), binding("missing", "absent"), binding("all", "*"))
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].ValidUntil = now.Add(20 * time.Second)
	must(t, s.Replace(ctx, v))
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "unknown" || h.Bindings["missing"].Condition != "unknown" || len(h.Bindings["missing"].Subjects) != 1 || h.Bindings["exact"].Condition != "ok" || !h.Bindings["all"].ValidUntil.Equal(v.Subjects[0].ValidUntil) {
		t.Fatal(h)
	}
	*now = now.Add(21 * time.Second)
	h, err = s.Health(ctx)
	must(t, err)
	if h.Bindings["exact"].Condition != "unknown" {
		t.Fatal("expired subject remained healthy")
	}
}

func TestLEDBindingsPauseIsBoundedAndRestartNeedsFreshEvidence(t *testing.T) {
	s, now, path := bindingFixture(t, binding("selected", "backup"))
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "paused"
	v.Subjects[0].ValidUntil = now.Add(20 * time.Second)
	must(t, s.Replace(ctx, v))
	api := &fakeLED{states: map[string]led.State{}}
	r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	id := bindingFault(t, s, "selected", "paused")
	state := api.states[id]
	if state.Persistent || state.TTL == nil || *state.TTL != 17 {
		t.Fatal("pause outlived its source evidence", state)
	}
	v.Sequence, v.BasisRevision = 2, 2
	v.Subjects[0].Condition = "ok"
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	_, healthy := r.children[0].ids()
	if _, exists := api.states[healthy]; !exists {
		t.Fatal("healthy fixture missing")
	}
	c := s.config
	must(t, s.Close())
	reopened, err := Open(path, c, false)
	must(t, err)
	defer reopened.Close()
	reopened.now = func() time.Time { return *now }
	r = LEDReconciler{Store: reopened, API: api, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	if _, exists := api.states[healthy]; exists {
		t.Fatal("restart trusted the previous session's health")
	}
	h, err := reopened.Health(ctx)
	must(t, err)
	if h.Bindings["selected"].Condition != "unknown" {
		t.Fatal(h)
	}
}

func TestLEDBindingLayoutAndUIDChangesFailClosed(t *testing.T) {
	for _, damage := range []string{"mapping", "uid", "missing-pin"} {
		t.Run(damage, func(t *testing.T) {
			s, _, path := bindingFixture(t, binding("selected", "backup"))
			c := s.config
			switch damage {
			case "mapping":
				c.LEDBindings = []LEDBinding{binding("selected", "different")}
			case "uid":
				_, err := s.db.Exec("UPDATE metadata SET value=json_set(value,'$.uid',?) WHERE key='led-layout'", os.Getuid()+1)
				must(t, err)
			case "missing-pin":
				_, err := s.db.Exec("DELETE FROM metadata WHERE key='led-layout'")
				must(t, err)
			}
			must(t, s.Close())
			if opened, err := Open(path, c, false); err == nil {
				opened.Close()
				t.Fatal("changed LED authority accepted")
			}
		})
	}
}

func TestLEDBindingConfigurationValidation(t *testing.T) {
	for _, damage := range []string{"duplicate", "foreign", "power", "wildcard", "missing-selector", "bad-priority", "bad-pattern", "unknown-condition", "too-many"} {
		t.Run(damage, func(t *testing.T) {
			c := configuration()
			c.LEDBindings = []LEDBinding{binding("selected", "backup")}
			b := &c.LEDBindings[0]
			switch damage {
			case "duplicate":
				c.LEDBindings = append(c.LEDBindings, *b)
			case "foreign":
				b.Producer.Epoch = "different"
			case "power":
				b.LED = "red"
			case "wildcard":
				b.Subjects = []string{"*", "backup"}
			case "missing-selector":
				b.Subjects = nil
			case "bad-priority":
				b.Styles = map[string]LEDStyle{"ok": {"heartbeat", 95}}
			case "bad-pattern":
				b.Styles = map[string]LEDStyle{"critical": {"bad pattern", 95}}
			case "unknown-condition":
				b.Styles = map[string]LEDStyle{"good": {"solid", 10}}
			case "too-many":
				for i := 0; i < 33; i++ {
					c.LEDBindings = append(c.LEDBindings, binding(strings.Repeat("a", i+1), "backup"))
				}
			}
			if c.Validate() == nil {
				t.Fatal("invalid LED configuration accepted")
			}
		})
	}
}

func TestLEDBindingFaultIdentityDamageCannotClearState(t *testing.T) {
	for _, damage := range []string{"body", "binding", "legacy-bypass"} {
		t.Run(damage, func(t *testing.T) {
			s, now, _ := bindingFixture(t, binding("selected", "backup"))
			ctx := context.Background()
			v := snapshot(*now, 1, 1)
			v.Subjects[0].Condition = "critical"
			must(t, s.Replace(ctx, v))
			api := &fakeLED{states: map[string]led.State{}}
			r := LEDReconciler{Store: s, API: api, Budget: 2 * time.Second}
			must(t, r.Step(ctx))
			id := bindingFault(t, s, "selected", "critical")
			var err error
			switch damage {
			case "body":
				_, err = s.db.Exec("UPDATE led_states SET identity='{}' WHERE id=?", id)
			case "binding":
				_, err = s.db.Exec("UPDATE led_states SET binding=? WHERE id=?", workerLEDBinding, id)
			case "legacy-bypass":
				_, err = s.db.Exec("UPDATE led_states SET binding=?,identity='legacy' WHERE id=?", workerLEDBinding, id)
			}
			must(t, err)
			v.Sequence, v.BasisRevision = 2, 2
			v.Subjects[0].Condition = "ok"
			must(t, s.Replace(ctx, v))
			if r.Step(ctx) == nil {
				t.Fatal("damaged LED ownership accepted")
			}
			if _, exists := api.states[id]; !exists {
				t.Fatal("cleared state with damaged ownership")
			}
		})
	}
}

func TestLEDBindingSnapshotPreservesMappingInQuarantine(t *testing.T) {
	s, now, path := bindingFixture(t, binding("selected", "backup"))
	ctx := context.Background()
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "critical"
	must(t, s.Replace(ctx, v))
	r := LEDReconciler{Store: s, API: &fakeLED{states: map[string]led.State{}}, Budget: 2 * time.Second}
	must(t, r.Step(ctx))
	id := bindingFault(t, s, "selected", "critical")
	artifact := filepath.Join(t.TempDir(), "snapshot")
	m, err := s.Snapshot(ctx, artifact)
	must(t, err)
	if m.Schema != currentSchema {
		t.Fatal(m)
	}
	c := s.config
	must(t, s.Close())
	j, err := prepareSnapshotRecovery(ctx, path, artifact, c, nil)
	must(t, err)
	db := recoveryCandidate(t, path, j)
	var body string
	must(t, db.QueryRow("SELECT identity FROM led_states WHERE id=? AND binding='selected'", id).Scan(&body))
	var saved ledStateIdentity
	must(t, json.Unmarshal([]byte(body), &saved))
	if saved.Recovery != j.Before.Recovery || saved.Recovery == j.After.Recovery {
		t.Fatal("restore relabeled old LED ownership")
	}
	if opened, err := Open(path, c, false); err == nil {
		opened.Close()
		t.Fatal("LED snapshot restore activated without reconciliation")
	}
}
