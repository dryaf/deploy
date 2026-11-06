package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func installationRetirementFixture(t *testing.T) (managedInstaller, ManagedSpec, ManagedManifest, ReplacementTarget) {
	t.Helper()
	ctx := context.Background()
	spec, i, _, _ := managedFixture(t)
	m, err := i.prepare(ctx, "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.OpenManagedVerification(spec.State, c.Worker, m.Identity)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Worker.Producers[0].Producer
	if err = s.Revoke(ctx, p, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RetireProducer(ctx, p, "operator", "Replacement after settlement", true); err != nil {
		t.Fatal(err)
	}
	s.Close()
	m, err = i.uninstall(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	var next Runtime
	err = json.Unmarshal(raw, &next)
	if err != nil {
		t.Fatal(err)
	}
	next.Worker.Producers[0].Producer.Epoch = "replacement-epoch"
	next.Worker.MaxMessages += 100
	base := t.TempDir()
	os.Chmod(base, 0700)
	target := ReplacementTarget{1, os.Getuid(), filepath.Join(base, "installation"), filepath.Join(base, "state"), next, "UID/configuration/history replacement"}
	if err = os.Mkdir(target.State, 0700); err != nil {
		t.Fatal(err)
	}
	return i, spec, m, target
}
func TestInstallationRetirementAndReplacement(t *testing.T) {
	for _, stage := range []string{"retirement-target", "retirement-worker", "retirement-published"} {
		t.Run(stage, func(t *testing.T) {
			i, spec, m, target := installationRetirementFixture(t)
			ctx := context.Background()
			i.fault = func(at string) error {
				if at == stage {
					return errors.New("interruption")
				}
				return nil
			}
			if _, err := i.retireInstallation(ctx, spec.Root, protocol.Hash(m), target); err == nil {
				t.Fatal("fault not reached")
			}
			i.fault = nil
			if _, err := i.prepare(ctx, "update", spec); err == nil {
				t.Fatal("archived installation revived")
			}
			changed := target
			changed.Reason = "Changed review"
			if _, err := i.retireInstallation(ctx, spec.Root, protocol.Hash(m), changed); err == nil {
				t.Fatal("changed target resumed")
			}
			v, err := i.retireInstallation(ctx, spec.Root, protocol.Hash(m), target)
			if err != nil {
				t.Fatal(err)
			}
			id, err := initializeReplacement(ctx, v, protocol.Hash(v))
			if err != nil {
				t.Fatal(err)
			}
			again, err := initializeReplacement(ctx, v, protocol.Hash(v))
			if err != nil || again != id || id.Worker == m.Identity.Worker {
				t.Fatal("replacement identity changed", err)
			}
			effective, err := target.effectiveRuntime()
			if err != nil {
				t.Fatal(err)
			}
			s, err := worker.Open(target.State, effective.Worker, false)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Stage(target.Runtime.Worker.Producers[0].Producer, "fresh"); err != nil {
				t.Fatal(err)
			}
			s.Close()
			// Install the actual replacement configuration through the normal lifecycle;
			// no delivery activation or old queue import follows from initialization.
			raw, _ := json.Marshal(target.Runtime)
			config := filepath.Join(filepath.Dir(target.State), "runtime.json")
			os.WriteFile(config, raw, 0600)
			units := filepath.Join(filepath.Dir(target.State), "units")
			os.Mkdir(units, 0700)
			replacement := spec
			replacement.Root = target.Root
			replacement.State = target.State
			replacement.RuntimeFile = config
			replacement.UnitDirectory = units
			f := &fakeManagedServices{t: t, units: units, state: managedServiceState{Load: "not-found", Active: "inactive"}}
			ni := managedInstaller{services: f, probe: func(context.Context, *safefs.Directory, string, string, bool) error { return nil }}
			installed, err := ni.prepare(ctx, "setup", replacement)
			if err != nil {
				t.Fatal(err)
			}
			if installed.Identity != id || installed.Mode != "verification" {
				t.Fatal("replacement gained old authority")
			}
			if _, err = os.Stat(filepath.Join(spec.State, "state.db")); err != nil {
				t.Fatal("old audit database lost")
			}
		})
	}
}
func TestInstallationRetirementRefusesReplacementConflicts(t *testing.T) {
	i, spec, m, target := installationRetirementFixture(t)
	ctx := context.Background()
	for _, kind := range []string{"old-state", "producer", "manifest"} {
		t.Run(kind, func(t *testing.T) {
			changed := target
			hash := protocol.Hash(m)
			switch kind {
			case "old-state":
				changed.State = spec.State
			case "producer":
				c, _ := ReadRuntime(spec.RuntimeFile)
				changed.Runtime = c
			case "manifest":
				hash = protocol.Hash("foreign")
			}
			if _, err := i.retireInstallation(ctx, spec.Root, hash, changed); err == nil {
				t.Fatal("conflicting replacement accepted")
			}
		})
	}
	v, err := i.retireInstallation(ctx, spec.Root, protocol.Hash(m), target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = initializeReplacement(ctx, v, protocol.Hash("foreign")); err == nil {
		t.Fatal("wrong certificate digest accepted")
	}
	v.Target.UID++
	if _, err = initializeReplacement(ctx, v, protocol.Hash(v)); err == nil {
		t.Fatal("wrong UID accepted")
	}
}

func TestInstallationRetirementReservesTerminalHistory(t *testing.T) {
	spec, i, _, _ := managedFixture(t)
	ctx := context.Background()
	m, err := i.prepare(ctx, "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n < 32; n++ {
		m, err = i.prepare(ctx, "update", spec)
		if err != nil {
			t.Fatal(n, err)
		}
	}
	if _, err = i.prepare(ctx, "update", spec); err == nil {
		t.Fatal("ordinary history limit bypassed")
	}
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.OpenManagedVerification(spec.State, c.Worker, m.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Revoke(ctx, c.Worker.Producers[0].Producer, "one"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	m, err = i.uninstall(ctx, spec.Root)
	if err != nil {
		t.Fatal("terminal uninstall blocked by history budget", err)
	}
	if m.Mode != "uninstalled" {
		t.Fatal(m.Mode)
	}
}

func TestInstallationRetirementRecoveredSignal(t *testing.T) {
	var manager *fakeSignalServiceManager
	spec, i, _, m, _, _ := managedSignalDeliveryFixtureUsing(t, func(_ *ManagedSpec, service *SignalServiceSpec, _ *managedInstaller, _ *fakeManagedServices, _ SignalVerificationManifest) signalServiceInstaller {
		manager = &fakeSignalServiceManager{units: service.UnitDirectory}
		return signalServiceInstaller{manager: manager, probe: func(context.Context, *safefs.Directory, SignalServiceManifest) error { return nil }}
	})
	ctx := context.Background()
	c, err := ReadRuntime(m.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openManagedStore(m, c)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range c.Worker.Producers {
		raw, err := os.ReadFile(filepath.Join(spec.State, "owner-"+safefs.LockName(e.Producer.ID)+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var owner struct {
			Generations map[string]string `json:"generations"`
		}
		if err = json.Unmarshal(raw, &owner); err != nil {
			t.Fatal(err)
		}
		for generation := range owner.Generations {
			if err = s.Revoke(ctx, e.Producer, generation); err != nil {
				t.Fatal(err)
			}
		}
		now := time.Now().UTC()
		if err = s.Replace(ctx, protocol.Snapshot{Producer: e.Producer, Sequence: 2, BasisRevision: 2, ConfigRevision: "native", EvaluatedAt: now, Subjects: []protocol.Subject{{Key: "fixture", Condition: "retired", Reason: "Operator retired coverage", ValidUntil: now.Add(time.Minute)}}}); err != nil {
			t.Fatal(err)
		}
		if _, err = s.RetireProducer(ctx, e.Producer, "operator", "Recovered account retired", true); err != nil {
			s.Close()
			t.Fatal(err)
		}
	}
	s.Close()
	m, err = i.uninstall(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	si := signalServiceInstaller{manager: manager}
	if _, err = si.uninstall(ctx, m.SignalVerification.ServiceRoot); err != nil {
		t.Fatal(err)
	}
	_, input := runtimeFixture(t)
	raw, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	var next Runtime
	json.Unmarshal(raw, &next)
	next.Worker.Producers[0].Producer.Epoch = "replacement-epoch"
	base := t.TempDir()
	os.Chmod(base, 0700)
	target := ReplacementTarget{1, os.Getuid(), filepath.Join(base, "installation"), filepath.Join(base, "state"), next, "Replace recovered Signal installation"}
	os.Mkdir(target.State, 0700)
	v, err := i.retireInstallation(ctx, spec.Root, protocol.Hash(m), target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = initializeReplacement(ctx, v, protocol.Hash(v)); err != nil {
		t.Fatal(err)
	}
}
