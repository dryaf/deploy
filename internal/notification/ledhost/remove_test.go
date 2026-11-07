package ledhost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type removalFake struct {
	*fakeServices
	stops int
}

func fakeRemovalFence(m manager, s Spec, hash string) error {
	raw, err := json.Marshal(daemonRemovalFence{1, hash})
	if err != nil {
		return err
	}
	return m.publish(Root(s.UID)+"/state/removal.json", raw, 0600)
}

func TestHostRemovalRefusesClientObligationsAndLostFence(t *testing.T) {
	m, s, artifact, f := fixture(t)
	p, _ := Build(s)
	if _, err := m.install(context.Background(), s, p.Hash, artifact); err != nil {
		t.Fatal(err)
	}
	service := &removalFake{fakeServices: f}
	m.services = service
	remove, _ := BuildRemoval(s)
	m.prepareRemoval = func(context.Context, Spec, string) error { return errors.New("states_remaining") }
	if _, err := m.remove(context.Background(), s, remove.Hash); err == nil || service.stops != 0 || !f.active {
		t.Fatal("client obligations stopped daemon", service.stops, err)
	}
	m.prepareRemoval = func(_ context.Context, s Spec, hash string) error { return fakeRemovalFence(m, s, hash) }
	if _, err := m.remove(context.Background(), s, remove.Hash); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.path(Root(s.UID) + "/state/removal.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.remove(context.Background(), s, remove.Hash); err == nil {
		t.Fatal("removed daemon fence recreated")
	}
}

func (f *removalFake) Stop(_ context.Context, _ Spec) error {
	if f.foreign {
		return errors.New("foreign service")
	}
	if f.active {
		f.stops++
		f.active = false
	}
	return nil
}
func (f *removalFake) VerifyStopped(_ context.Context, _ Spec) error {
	if f.active || f.foreign {
		return errors.New("service not stopped or foreign")
	}
	return nil
}

func TestHostRemovalResumesAndRetainsState(t *testing.T) {
	for _, boundary := range []string{"remove-prepared", "remove-fenced", "remove-stop-returned", "removed:deploy-led-host-1000.socket", "removed:deploy-led-host-1000.service", "remove-receipt", "remove-complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			m, s, artifact, f := fixture(t)
			installPlan, _ := Build(s)
			if _, err := m.install(ctx, s, installPlan.Hash, artifact); err != nil {
				t.Fatal(err)
			}
			services := &removalFake{fakeServices: f}
			m.services = services
			state := m.path(Root(s.UID) + "/state/retained-registry")
			if err := os.WriteFile(state, []byte("foreign state and unresolved history"), 0600); err != nil {
				t.Fatal(err)
			}
			p, _ := BuildRemoval(s)
			m.boundary = func(at string) error {
				if at == boundary {
					return errors.New("lost removal reply")
				}
				return nil
			}
			if _, err := m.remove(ctx, s, p.Hash); err == nil {
				t.Fatal("boundary not exercised")
			}
			m.boundary = nil
			r, err := m.remove(ctx, s, p.Hash)
			if err != nil || !r.Complete || services.stops != 1 {
				t.Fatal(r, services.stops, err)
			}
			reloads := services.reloads
			for range 2 {
				again, e := m.remove(ctx, s, p.Hash)
				if e != nil || again != r || services.stops != 1 || services.reloads != reloads {
					t.Fatal("completed removal caused effects", e)
				}
			}
			if body, err := os.ReadFile(state); err != nil || string(body) != "foreign state and unresolved history" {
				t.Fatal("removal erased registry", err)
			}
			for _, name := range []string{"daemon.pyz", "config.toml", "receipt.json", "journal.json", "operation.lock"} {
				if _, err := os.Stat(m.path(Root(s.UID) + "/" + name)); err != nil {
					t.Fatal("lost retained evidence", name, err)
				}
			}
			got, err := m.readRemovalReceipt(s.UID)
			if err != nil || got != r {
				t.Fatal("public removed receipt unavailable", got, err)
			}
			if _, err = m.install(ctx, s, installPlan.Hash, artifact); err == nil {
				t.Fatal("ordinary install resurrected removed daemon")
			}
		})
	}
}

func TestHostRemovalRefusesDriftAndLostEvidence(t *testing.T) {
	for _, kind := range []string{"approval", "foreign", "unit", "artifact", "receipt", "lock", "symlink", "lost-removal", "lost-completion", "reappeared"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			m, s, artifact, f := fixture(t)
			p, _ := Build(s)
			if _, err := m.install(ctx, s, p.Hash, artifact); err != nil {
				t.Fatal(err)
			}
			service := &removalFake{fakeServices: f}
			m.services = service
			remove, _ := BuildRemoval(s)
			approved := remove.Hash
			unit := m.path(remove.Files[0])
			switch kind {
			case "approval":
				approved = strings.Repeat("0", 64)
			case "foreign":
				f.foreign = true
			case "unit":
				if err := os.WriteFile(unit, []byte("foreign file"), 0644); err != nil {
					t.Fatal(err)
				}
			case "artifact":
				if err := os.WriteFile(m.path(Root(s.UID)+"/daemon.pyz"), []byte("changed"), 0644); err != nil {
					t.Fatal(err)
				}
			case "receipt":
				if err := os.Remove(m.path(Root(s.UID) + "/receipt.json")); err != nil {
					t.Fatal(err)
				}
			case "lock":
				if err := os.Remove(m.path(Root(s.UID) + "/operation.lock")); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(unit); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(m.prefix, "foreign"), unit); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := m.remove(ctx, s, approved); err != nil {
					t.Fatal(err)
				}
				name := "removal.json"
				if kind == "lost-completion" {
					name = "removal-receipt.json"
				}
				if kind == "reappeared" {
					if err := os.WriteFile(unit, []byte("foreign replacement"), 0644); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Remove(m.path(Root(s.UID) + "/" + name)); err != nil {
					t.Fatal(err)
				}
			}
			stops := service.stops
			if _, err := m.remove(ctx, s, approved); err == nil {
				t.Fatal("drift accepted", kind)
			}
			if service.stops != stops {
				t.Fatal("refused operation stopped services")
			}
		})
	}
}
