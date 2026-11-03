package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func rollbackFixture(t *testing.T) (ManagedSpec, managedInstaller, *fakeManagedServices, ManagedManifest, ManagedManifest) {
	t.Helper()
	spec, i, f, id := managedFixture(t)
	first, err := i.prepare(context.Background(), "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, []byte("synthetic revision two")...)
	if err = os.WriteFile(spec.BinaryFile, b, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(b)
	second, err := i.prepare(context.Background(), "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	checkManagedInstalled(t, spec, second, id)
	return spec, i, f, first, second
}

func TestManagedRollbackPreservesStateAndUsesFrozenRelease(t *testing.T) {
	spec, i, f, first, second := rollbackFixture(t)
	c, err := ReadRuntime(second.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(spec.State, c.Worker, false)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p := c.Worker.Producers[0].Producer
	m := protocol.Message{Producer: p, Sequence: 1, ID: "retained-report", Generation: "one", Subject: "subject", Kind: "report", Severity: "info", ObservedAt: now, ExpiresAt: now.Add(time.Minute), Text: "Pending during rollback", Route: "ops", ReportKey: "day", ReportType: "daily", ReportRevision: 1, BasisRevision: 1}
	if _, err = s.Enqueue(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(spec.BinaryFile); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(spec.RuntimeFile); err != nil {
		t.Fatal(err)
	}
	rolled, err := i.rollback(context.Background(), spec.Root, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.BinaryHash != first.BinaryHash || rolled.Revision == first.Revision || rolled.Revision == second.Revision || rolled.Identity != second.Identity || rolled.HelperHash != second.HelperHash || rolled.ConfigHash != second.ConfigHash || rolled.RuntimeHash != second.RuntimeHash {
		t.Fatal("rollback changed pinned authority", rolled)
	}
	checkManagedInstalled(t, spec, rolled, first.Identity)
	starts := f.starts
	again, err := i.rollback(context.Background(), spec.Root, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(again) != protocol.Hash(rolled) || f.starts != starts {
		t.Fatal("lost response caused another rollback")
	}
	s, err = worker.Open(spec.State, c.Worker, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	j, err := s.Get(context.Background(), p, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(j.Message) != protocol.Hash(m) || j.Tries != 0 || j.Attempt != "" {
		t.Fatal("rollback changed pending message", j)
	}
}

func TestManagedRollbackResumeAllLifecycleBoundaries(t *testing.T) {
	for _, point := range []string{"rollback_reviewed", "rollback_staged", "prepared", "stopped", "unit-published", "manifest-published", "reloaded", "started", "complete"} {
		t.Run(point, func(t *testing.T) {
			spec, i, _, first, _ := rollbackFixture(t)
			injected := errors.New("interrupted")
			i.fault = func(p string) error {
				if p == point {
					return injected
				}
				return nil
			}
			if _, err := i.rollback(context.Background(), spec.Root, first.Revision); !errors.Is(err, injected) {
				t.Fatal(err)
			}
			i.fault = nil
			var m ManagedManifest
			var err error
			if point == "rollback_reviewed" || point == "rollback_staged" {
				m, err = i.rollback(context.Background(), spec.Root, first.Revision)
			} else {
				m, err = i.resume(context.Background(), spec.Root)
			}
			if err != nil {
				t.Fatal(err)
			}
			if m.BinaryHash != first.BinaryHash {
				t.Fatal("resume lost selected executable")
			}
			checkManagedInstalled(t, spec, m, first.Identity)
		})
	}
}

func TestManagedRollbackRejectsDriftAndChangedRetainedArtifacts(t *testing.T) {
	for _, mode := range []string{"binary", "runtime", "missing", "current", "unfinished", "concurrent_update", "no_capacity"} {
		t.Run(mode, func(t *testing.T) {
			spec, i, f, first, second := rollbackFixture(t)
			revision := first.Revision
			switch mode {
			case "binary":
				if err := os.WriteFile(first.Paths.Executable, []byte("changed"), 0700); err != nil {
					t.Fatal(err)
				}
			case "runtime":
				if err := os.WriteFile(first.Paths.Config, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				revision = "n-00000000000000000000000000000000"
			case "current":
				revision = second.Revision
			case "unfinished":
				i.fault = func(point string) error {
					if point == "prepared" {
						return errors.New("interrupted update")
					}
					return nil
				}
				if _, err := i.prepare(context.Background(), "update", spec); err == nil {
					t.Fatal("update not interrupted")
				}
				i.fault = nil
			case "concurrent_update":
				i.fault = func(point string) error {
					if point == "rollback_reviewed" {
						next := i
						next.fault = nil
						_, err := next.prepare(context.Background(), "update", spec)
						return err
					}
					return nil
				}
			case "no_capacity": // An unowned staging release counts against bounded history.
				for n := 0; n < 30; n++ {
					id, err := safefs.NewID("n")
					if err != nil {
						t.Fatal(err)
					}
					if err = os.Mkdir(filepath.Join(spec.Root, "releases", id), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			stops := f.stops
			if _, err := i.rollback(context.Background(), spec.Root, revision); err == nil {
				t.Fatal("unsafe rollback accepted")
			}
			want := stops
			if mode == "concurrent_update" {
				want++
			}
			if f.stops != want {
				t.Fatal("rejected rollback stopped runtime", f.stops, want)
			}
		})
	}
}

func TestManagedRollbackDryRunDoesNotChangeInstallation(t *testing.T) {
	spec, _, f, first, second := rollbackFixture(t)
	starts, stops := f.starts, f.stops
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	if err := RunCLI([]string{"rollback", "--local", "--root", spec.Root, "--from-revision", first.Revision}); err != nil {
		t.Fatal(err)
	}
	if f.starts != starts || f.stops != stops {
		t.Fatal("dry-run contacted service manager")
	}
	d, err := safefs.Open(spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		t.Fatal(err)
	}
	if j.Intent.After.Revision != second.Revision {
		t.Fatal("dry-run changed revision")
	}
	if err := RunCLI([]string{"rollback", "--local", "--root", spec.Root, "--from-revision", first.Revision, "--spec", "unused"}); err == nil {
		t.Fatal("rollback accepted replacement spec")
	}
}

func TestManagedRollbackAfterCancelledFailedUpdate(t *testing.T) {
	spec, i, f, first, _ := rollbackFixture(t)
	f.unhealthy = true
	if _, err := i.prepare(context.Background(), "update", spec); err == nil {
		t.Fatal("unhealthy candidate accepted")
	}
	if _, err := i.uninstall(context.Background(), spec.Root); err != nil {
		t.Fatal(err)
	}
	f.unhealthy = false
	m, err := i.rollback(context.Background(), spec.Root, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "verification" || m.BinaryHash != first.BinaryHash {
		t.Fatal("failed update rollback lost selected release", m)
	}
	checkManagedInstalled(t, spec, m, first.Identity)
}
