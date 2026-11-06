package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/safefs"
)

func replacementServiceSpec(t *testing.T, prior SignalServiceManifest) SignalServiceSpec {
	t.Helper()
	s := signalServiceSpecFor(t, prior.Endpoint, prior.Spec.UnitDirectory, prior.Spec.BinaryFile)
	p := prior.Spec.Endpoint
	p.ServiceRoot, p.ServiceHash = prior.Spec.Root, prior.ManifestHash
	s.Previous = &p
	return s
}

func TestSignalServiceReplacement(t *testing.T) {
	s, i, manager := signalServiceFixture(t)
	ctx := context.Background()
	m, err := i.setup(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	next := replacementServiceSpec(t, m)
	if _, err := i.setup(ctx, next); err == nil {
		t.Fatal("active predecessor replaced")
	}
	if _, err = i.uninstall(ctx, s.Root); err != nil {
		t.Fatal(err)
	}
	n, err := i.setup(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	if manager.starts != 2 {
		t.Fatal(manager.starts)
	}
	if _, err = i.resume(ctx, s.Root); err == nil {
		t.Fatal("old service resumed across replacement")
	}
	if _, err = i.resume(ctx, next.Root); err != nil {
		t.Fatal(err)
	}
	if _, err = i.uninstall(ctx, next.Root); err != nil {
		t.Fatal(err)
	}
	// A rollback is another explicit immutable enrollment, including the same
	// binary bytes if desired. It never revives a removed service root.
	third := replacementServiceSpec(t, n)
	if _, err = i.setup(ctx, third); err != nil {
		t.Fatal(err)
	}
}

func TestSignalServiceReplacementInterrupted(t *testing.T) {
	for _, at := range []string{"prepared", "replacement-intent", "replacement-bound", "replacement-complete", "bound", "unit", "published", "start-returned", "complete"} {
		t.Run(at, func(t *testing.T) {
			s, i, manager := signalServiceFixture(t)
			ctx := context.Background()
			m, err := i.setup(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = i.uninstall(ctx, s.Root); err != nil {
				t.Fatal(err)
			}
			next := replacementServiceSpec(t, m)
			stop := errors.New("interrupted replacement")
			i.fault = func(p string) error {
				if p == at {
					return stop
				}
				return nil
			}
			if _, err = i.setup(ctx, next); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			if _, err = i.resume(ctx, next.Root); err != nil {
				t.Fatal(err)
			}
			if manager.starts != 2 {
				t.Fatal("duplicate start", manager.starts)
			}
		})
	}
}

func TestSignalServiceReplacementLostEvidence(t *testing.T) {
	for _, anchor := range []string{"old-intent", "old-complete", "endpoint-intent", "endpoint-complete", "new-complete", "head-rollback"} {
		t.Run(anchor, func(t *testing.T) {
			s, i, manager := signalServiceFixture(t)
			ctx := context.Background()
			m, err := i.setup(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = i.uninstall(ctx, s.Root); err != nil {
				t.Fatal(err)
			}
			next := replacementServiceSpec(t, m)
			n, err := i.setup(ctx, next)
			if err != nil {
				t.Fatal(err)
			}
			if err = manager.Stop(ctx); err != nil {
				t.Fatal(err)
			}
			files := map[string]string{
				"old-intent":        filepath.Join(s.Root, "replacement.json"),
				"old-complete":      filepath.Join(s.Root, "replacement-complete.json"),
				"endpoint-intent":   filepath.Join(s.Endpoint.Root, "replacement-"+n.ManifestHash+".json"),
				"endpoint-complete": filepath.Join(s.Endpoint.Root, "replacement-"+n.ManifestHash+"-complete.json"),
				"new-complete":      filepath.Join(next.Root, "enrollment-complete.json"),
			}
			if anchor == "head-rollback" {
				d, e := safefs.Open(s.Endpoint.Root)
				if e != nil {
					t.Fatal(e)
				}
				err = d.WriteJSON(signalServiceEndpointBinding, m.binding(), true)
				d.Close()
			} else {
				err = os.Remove(files[anchor])
			}
			if err != nil {
				t.Fatal(err)
			}
			starts := manager.starts
			if _, err = i.resume(ctx, next.Root); err == nil {
				t.Fatal("lost authority accepted")
			}
			if manager.starts != starts {
				t.Fatal("unqualified service started")
			}
		})
	}
}

func TestSignalServiceReplacementCannotBranchOrStealLease(t *testing.T) {
	s, i, manager := signalServiceFixture(t)
	ctx := context.Background()
	m, err := i.setup(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = i.uninstall(ctx, s.Root); err != nil {
		t.Fatal(err)
	}
	next := replacementServiceSpec(t, m)
	old, err := safefs.Open(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	lease, err := old.Lock("installation.lock", true, false)
	if err != nil {
		t.Fatal(err)
	}
	starts := manager.starts
	if _, err = i.setup(ctx, next); err == nil {
		t.Fatal("retained predecessor lease bypassed")
	}
	if manager.starts != starts {
		t.Fatal("started while predecessor lease held")
	}
	lease.Close()
	stop := errors.New("pause replacement")
	i.fault = func(at string) error {
		if at == "replacement-intent" {
			return stop
		}
		return nil
	}
	if _, err = i.resume(ctx, next.Root); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	i.fault = nil
	branch := replacementServiceSpec(t, m)
	if _, err = i.setup(ctx, branch); err == nil {
		t.Fatal("branched replacement authority")
	}
	if manager.starts != starts {
		t.Fatal("branch started")
	}
	if _, err = i.resume(ctx, next.Root); err != nil {
		t.Fatal(err)
	}
}
