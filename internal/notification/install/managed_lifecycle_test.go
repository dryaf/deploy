package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

// This manager models persistent manager state separately from the unit on disk,
// including a stale loaded unit between unlink and daemon-reload.
type fakeManagedServices struct {
	t             *testing.T
	units         string
	state         managedServiceState
	starts, stops int
	lostStart     bool
	unhealthy     bool
	onStop        func()
}

func (f *fakeManagedServices) Inspect(context.Context) (managedServiceState, error) {
	return f.state, nil
}
func (f *fakeManagedServices) Stop(context.Context) error {
	f.stops++
	f.state.Active = "inactive"
	if f.onStop != nil {
		f.onStop()
	}
	return nil
}
func (f *fakeManagedServices) Reload(context.Context) error {
	_, err := os.Stat(filepath.Join(f.units, managedUnit))
	if os.IsNotExist(err) {
		f.state = managedServiceState{Load: "not-found", Active: "inactive"}
		return nil
	}
	if err != nil {
		return err
	}
	unitFile := f.state.UnitFile
	if unitFile == "" {
		unitFile = "disabled"
	}
	f.state = managedServiceState{Load: "loaded", Active: "inactive", Fragment: filepath.Join(f.units, managedUnit), UnitFile: unitFile}
	return nil
}
func (f *fakeManagedServices) EnableStart(context.Context) error {
	f.starts++
	f.state.Active = "active"
	f.state.UnitFile = "enabled"
	if f.lostStart {
		f.lostStart = false
		return fmt.Errorf("start acknowledgement lost")
	}
	return nil
}
func (f *fakeManagedServices) Disable(context.Context) error {
	if _, err := os.Stat(filepath.Join(f.units, managedUnit)); err != nil {
		return fmt.Errorf("cannot disable missing unit: %w", err)
	}
	f.state.UnitFile = "disabled"
	return nil
}
func (f *fakeManagedServices) Verify(ctx context.Context, m ManagedManifest) error {
	if f.unhealthy {
		return fmt.Errorf("health check failed")
	}
	if f.state.Active != "active" || m.Mode != "verification" {
		return fmt.Errorf("not running verification service")
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
	if s.Identity() != m.Identity {
		return fmt.Errorf("wrong health identity")
	}
	return s.RequireInactiveProducers()
}

func managedFixture(t *testing.T) (ManagedSpec, managedInstaller, *fakeManagedServices, protocol.Identity) {
	t.Helper()
	_, config := runtimeFixture(t)
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	spec := ManagedSpec{Version: 1, Root: filepath.Join(base, "installation"), State: filepath.Join(base, "state"), UnitDirectory: filepath.Join(base, "units"), RuntimeFile: config, BinaryFile: filepath.Join(base, "input-deploy")}
	for _, path := range []string{spec.State, spec.UnitDirectory} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// A tiny real native ELF keeps the state-machine suite bounded. Probing is
	// separately tested using the real deploy executable in the integration test.
	b, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(spec.BinaryFile, b, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(b)
	c, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(spec.State, c.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	id := s.Identity()
	if err = s.Stage(c.Worker.Producers[0].Producer, "one"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	f := &fakeManagedServices{t: t, units: spec.UnitDirectory, state: managedServiceState{Load: "not-found", Active: "inactive"}}
	i := managedInstaller{services: f, probe: func(context.Context, *safefs.Directory, string, string, bool) error { return nil }}
	return spec, i, f, id
}

func checkManagedInstalled(t *testing.T, spec ManagedSpec, m ManagedManifest, id protocol.Identity) {
	t.Helper()
	if m.Identity != id || m.Mode != "verification" {
		t.Fatal("installation changed identity/mode", m)
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	j, err := readManagedJournal(d)
	if err != nil || j.Phase != "complete" {
		t.Fatal("unfinished journal", j.Phase, err)
	}
	if err = validateManagedPointer(d, j, true); err != nil {
		t.Fatal(err)
	}
	if _, err = verifyManagedArtifacts(d, m); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(spec.UnitDirectory, managedUnit))
	if err != nil || managedDigest(b) != m.UnitHash || !strings.Contains(string(b), "__managed-serve") || strings.Contains(string(b), " serve --local") {
		t.Fatal("unmanaged unit", err)
	}
	c, err := ReadRuntime(m.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.OpenVerification(spec.State, c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	status, err := s.OwnershipStatus(c.Worker.Producers[0].Producer, "one")
	if err != nil || status.Active || status.State != "staged" {
		t.Fatal("ownership changed", status, err)
	}
}

func TestManagedSetupUpdateUninstallRetainIdentityAndHelper(t *testing.T) {
	spec, i, f, id := managedFixture(t)
	m, err := i.prepare(context.Background(), "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	checkManagedInstalled(t, spec, m, id)
	helper, err := os.ReadFile(filepath.Join(spec.Root, "recovery-helper"))
	if err != nil {
		t.Fatal(err)
	}
	// An independently hashed new executable revision is allowed; configuration
	// and schema stay fixed until explicit migration is implemented.
	b, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, []byte("new-release")...)
	if err = os.WriteFile(spec.BinaryFile, b, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(b)
	updated, err := i.prepare(context.Background(), "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	checkManagedInstalled(t, spec, updated, id)
	if updated.Revision == m.Revision || updated.Installation != m.Installation || updated.HelperHash != m.HelperHash || updated.BinaryHash == m.BinaryHash {
		t.Fatal("incorrect update", updated)
	}
	got, err := os.ReadFile(filepath.Join(spec.Root, "recovery-helper"))
	if err != nil || string(got) != string(helper) {
		t.Fatal("helper overwritten", err)
	}
	removed, err := i.uninstall(context.Background(), spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Mode != "uninstalled" || removed.Identity != id || f.state.Load != "not-found" {
		t.Fatal(removed, f.state)
	}
	if _, err = os.Stat(filepath.Join(spec.UnitDirectory, managedUnit)); !os.IsNotExist(err) {
		t.Fatal("unit retained", err)
	}
	if _, err = os.Stat(filepath.Join(spec.State, "state.db")); err != nil {
		t.Fatal("state deleted", err)
	}
	if _, err = i.uninstall(context.Background(), spec.Root); err != nil {
		t.Fatal("uninstall not idempotent", err)
	}
	reinstalled, err := i.prepare(context.Background(), "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	checkManagedInstalled(t, spec, reinstalled, id)
}

func TestManagedResumeEveryDurableBoundary(t *testing.T) {
	for _, op := range []string{"setup", "update", "uninstall"} {
		points := []string{"prepared", "stop-returned", "stopped", "files", "unit-published", "manifest-published", "published", "reload-returned", "reloaded", "started", "complete"}
		if op == "uninstall" {
			points = append(points, "disable-returned")
		} else {
			points = append(points, "start-returned")
		}
		for _, point := range points {
			t.Run(op+"/"+point, func(t *testing.T) {
				spec, i, _, id := managedFixture(t)
				if op != "setup" {
					if _, err := i.prepare(context.Background(), "setup", spec); err != nil {
						t.Fatal(err)
					}
				}
				hit := false
				injected := errors.New("interrupted")
				i.fault = func(p string) error {
					if p == point {
						hit = true
						return injected
					}
					return nil
				}
				var err error
				if op == "uninstall" {
					_, err = i.uninstall(context.Background(), spec.Root)
				} else {
					_, err = i.prepare(context.Background(), op, spec)
				}
				if !hit || !errors.Is(err, injected) {
					t.Fatal("boundary not exercised", hit, err)
				}
				// Recovery must use frozen artifacts, even after originals disappear.
				if err = os.Remove(spec.RuntimeFile); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(spec.BinaryFile); err != nil {
					t.Fatal(err)
				}
				i.fault = nil
				m, err := i.resume(context.Background(), spec.Root)
				if err != nil {
					t.Fatal(err)
				}
				if op != "uninstall" {
					checkManagedInstalled(t, spec, m, id)
				} else if m.Mode != "uninstalled" || m.Identity != id {
					t.Fatal(m)
				}
			})
		}
	}
}

func TestManagedRejectsForeignAndDamagedArtifacts(t *testing.T) {
	for _, scenario := range []string{"unit", "dropin", "manager-dropin", "manager-fragment", "binary", "helper", "runtime", "state", "identity", "operation-lock", "installation-lock", "intent", "pointer", "root-move"} {
		t.Run(scenario, func(t *testing.T) {
			spec, i, _, _ := managedFixture(t)
			i.fault = func(p string) error {
				if p == "prepared" {
					return fmt.Errorf("interrupt")
				}
				return nil
			}
			if _, err := i.prepare(context.Background(), "setup", spec); err == nil {
				t.Fatal("no interruption")
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
			path := ""
			switch scenario {
			case "unit":
				path = filepath.Join(spec.UnitDirectory, managedUnit)
			case "dropin":
				if err = os.Mkdir(filepath.Join(spec.UnitDirectory, managedUnit+".d"), 0700); err != nil {
					t.Fatal(err)
				}
			case "manager-dropin":
				i.services.(*fakeManagedServices).state.DropIns = "/foreign/override.conf"
			case "manager-fragment":
				i.services.(*fakeManagedServices).state = managedServiceState{Load: "loaded", Active: "active", Fragment: "/foreign/unit", UnitFile: "enabled"}
			case "binary":
				path = j.Intent.After.Paths.Executable
			case "runtime":
				path = j.Intent.After.Paths.Config
			case "helper":
				path = filepath.Join(spec.Root, "recovery-helper")
			case "state":
				err = os.Remove(filepath.Join(spec.State, "state.db"))
			case "identity":
				err = os.Remove(filepath.Join(spec.State, "identity.json"))
			case "operation-lock", "installation-lock":
				err = os.Remove(filepath.Join(spec.Root, strings.TrimSuffix(scenario, "-lock")+".lock"))
			case "intent":
				path = filepath.Join(spec.Root, "operations", j.Intent.ID, "intent.json")
			case "pointer":
				path = filepath.Join(spec.Root, "installation.json")
			case "root-move":
				next := spec.Root + "-moved"
				err = os.Rename(spec.Root, next)
				spec.Root = next
			}
			if err != nil {
				t.Fatal(err)
			}
			if path != "" {
				if err = os.WriteFile(path, []byte("foreign"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = i.resume(context.Background(), spec.Root); err == nil {
				t.Fatal("damaged authority accepted")
			}
			if i.services.(*fakeManagedServices).starts != 0 {
				t.Fatal("started despite changed authority")
			}
			if scenario == "state" {
				if _, err = os.Stat(filepath.Join(spec.State, "state.db")); !os.IsNotExist(err) {
					t.Fatal("missing DB recreated")
				}
			}
			if path != "" {
				b, e := os.ReadFile(path)
				if e != nil || string(b) != "foreign" {
					t.Fatal("foreign file overwritten", e)
				}
			}
		})
	}
}

func TestManagedLostStartAckAndHealthFailureRemainResumable(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			spec, i, f, id := managedFixture(t)
			f.lostStart = lost
			f.unhealthy = !lost
			if _, err := i.prepare(context.Background(), "setup", spec); err == nil {
				t.Fatal("failure ignored")
			}
			f.unhealthy = false
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			checkManagedInstalled(t, spec, m, id)
			if f.starts != 2 || f.stops != 1 {
				t.Fatal("did not re-establish stopped state", f.starts, f.stops)
			}
		})
	}
}

func TestManagedRefusesLiveProducerAndConfigMigration(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			spec, i, f, _ := managedFixture(t)
			c, err := ReadRuntime(spec.RuntimeFile)
			if err != nil {
				t.Fatal(err)
			}
			if active {
				s, err := worker.Open(spec.State, c.Worker, false)
				if err != nil {
					t.Fatal(err)
				}
				err = s.Activate(c.Worker.Producers[0].Producer, "one")
				s.Close()
				if err != nil {
					t.Fatal(err)
				}
				if _, err = i.prepare(context.Background(), "setup", spec); err == nil {
					t.Fatal("adopted active producer")
				}
				if f.starts != 0 {
					t.Fatal("started active producer")
				}
			} else {
				if _, err = i.prepare(context.Background(), "setup", spec); err != nil {
					t.Fatal(err)
				}
				_, rawConfig := runtimeFixture(t)
				raw, err := ReadRuntime(rawConfig)
				if err != nil {
					t.Fatal(err)
				}
				raw.Worker.MaxMessages++
				writeRuntime(t, spec.RuntimeFile, raw)
				if _, err = i.prepare(context.Background(), "update", spec); err == nil {
					t.Fatal("implicitly migrated config")
				}
				if f.stops != 0 {
					t.Fatal("stopped before config validation")
				}
			}
		})
	}
}

func TestManagedLeasesPreventConcurrentResumeAndStopWhileBusy(t *testing.T) {
	for _, name := range []string{"operation.lock", "installation.lock"} {
		t.Run(name, func(t *testing.T) {
			spec, i, f, _ := managedFixture(t)
			i.fault = func(p string) error {
				if p == "prepared" {
					return fmt.Errorf("interrupt")
				}
				return nil
			}
			if _, err := i.prepare(context.Background(), "setup", spec); err == nil {
				t.Fatal("not interrupted")
			}
			i.fault = nil
			d, err := privateManagedDir(spec.Root, true)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			lease, err := d.Lock(name, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = i.resume(context.Background(), spec.Root); err == nil {
				t.Fatal("lease bypassed")
			}
			lease.Close()
			if f.starts != 0 {
				t.Fatal("service started")
			}
			if _, err = i.resume(context.Background(), spec.Root); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedUninstallCanAbortUnhealthyOrInterruptedInstallation(t *testing.T) {
	for _, op := range []string{"setup", "update"} {
		for _, point := range []string{"prepared", "unit-published", "manifest-published", "reloaded", "start-returned"} {
			t.Run(op+"/"+point, func(t *testing.T) {
				spec, i, f, id := managedFixture(t)
				if op == "update" {
					if _, err := i.prepare(context.Background(), "setup", spec); err != nil {
						t.Fatal(err)
					}
				}
				i.fault = func(p string) error {
					if p == point {
						return fmt.Errorf("interrupted")
					}
					return nil
				}
				if _, err := i.prepare(context.Background(), op, spec); err == nil {
					t.Fatal("not interrupted")
				}
				i.fault = nil
				f.unhealthy = true
				// Also interrupt the cancellation itself after publication. It must
				// resume uninstall, never start the original failed installation.
				i.fault = func(p string) error {
					if p == "unit-published" {
						return fmt.Errorf("cancellation interrupted")
					}
					return nil
				}
				if _, err := i.uninstall(context.Background(), spec.Root); err == nil {
					t.Fatal("cancellation not interrupted")
				}
				i.fault = nil
				starts := f.starts
				m, err := i.uninstall(context.Background(), spec.Root)
				if err != nil {
					t.Fatal(err)
				}
				if m.Identity != id || m.Mode != "uninstalled" || f.starts != starts || f.state.Load != "not-found" {
					t.Fatal("cancellation started a service or changed identity", m, f.starts, starts)
				}
				if _, err = os.Stat(filepath.Join(spec.State, "state.db")); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
