package signalaccount

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type nextRecoveryServiceFixture struct {
	*recoveryServiceFixture
	old bool
}

func (s *nextRecoveryServiceFixture) VerifyNextRecoverySupport(context.Context) error {
	if s.old {
		return errors.New("old later-recovery runtime")
	}
	return nil
}

func completedRecoveryFixture(t *testing.T) (string, RecoveryStatus, *nextRecoveryServiceFixture) {
	t.Helper()
	path, req, svc := recoveryFixture(t)
	apply, verify, _ := recoveryCallbacks(t, path)
	parent, err := FenceRecovery(context.Background(), path, req, svc, apply, verify)
	if err != nil {
		t.Fatal(err)
	}
	return path, parent, &nextRecoveryServiceFixture{recoveryServiceFixture: svc}
}

func nextRequest(t *testing.T, parent RecoveryStatus) RecoveryRequest {
	t.Helper()
	id, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	return RecoveryRequest{id, parent.Binding, protocol.Hash(id)}
}

func TestSignalNextRecoveryRetainsAncestors(t *testing.T) {
	path, parent, svc := completedRecoveryFixture(t)
	first := parent
	for epoch := 2; epoch <= 3; epoch++ {
		req := nextRequest(t, parent)
		apply, verify, calls := recoveryCallbacks(t, path)
		current, err := FenceNextRecovery(context.Background(), path, parent, req, svc, apply, verify)
		if err != nil || current.Phase != "complete" || current.Operation != req.Operation || current.RuntimeAllowed || *calls != 1 {
			t.Fatal(epoch, current, err, *calls)
		}
		if err := VerifyRecoveryAncestor(path, current, first); err != nil {
			t.Fatal(err)
		}
		again, err := FenceNextRecovery(context.Background(), path, parent, req, svc, apply, verify)
		if err != nil || again != current || *calls != 1 {
			t.Fatal(epoch, again, err, *calls)
		}
		if old, err := OpenRecoveryVerification(path, parent); err == nil {
			old.Close()
			t.Fatal("superseded epoch regained account")
		}
		live, err := OpenRecoveryVerification(path, current)
		if err != nil {
			t.Fatal(err)
		}
		live.Close()
		requireRuntime(t, path, false)
		parent = current
	}
	d, err := accountDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var intent nextRecoveryIntent
	if ok, err := readRecord(d, nextRecoveryName(first), &intent); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err = os.Remove(filepath.Join(path, ancestorRecoveryName(intent.After.Ancestor))); err != nil {
		t.Fatal(err)
	}
	if _, err = InspectRecovery(path); err == nil {
		t.Fatal("missing grandparent proof accepted")
	}
	if v, err := OpenRecoveryVerification(path, parent); err == nil {
		v.Close()
		t.Fatal("lost ancestor granted account")
	}
}

func TestSignalNextRecoveryInterrupted(t *testing.T) {
	for _, point := range []string{"next_recovery_intent", "next_recovery_root", "next_recovery_retire_" + recoveryCompleteFile, "next_recovery_retire_" + recoveryResultFile, "next_recovery_retire_" + recoveryFenceFile, "recovery_fence", "recovery_stop", "recovery_applied", "recovery_result", "recovery_complete"} {
		t.Run(point, func(t *testing.T) {
			path, parent, svc := completedRecoveryFixture(t)
			req := nextRequest(t, parent)
			apply, verify, calls := recoveryCallbacks(t, path)
			stop := errors.New("interrupted")
			c := recoveryCoordinator{fault: func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}}
			if _, err := c.runNext(context.Background(), path, parent, req, svc, apply, verify); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			requireRuntime(t, path, false)
			if v, err := OpenRecoveryVerification(path, parent); err == nil {
				v.Close()
				t.Fatal("old recovered runtime entered partial transition")
			}
			before := *calls
			result, err := FenceNextRecovery(context.Background(), path, parent, req, svc, apply, verify)
			if err != nil || result.Phase != "complete" {
				t.Fatal(result, err)
			}
			if (point == "recovery_result" || point == "recovery_complete") && *calls != before {
				t.Fatal("committed result reapplied")
			}
		})
	}
}

func TestSignalNextRecoveryPreflight(t *testing.T) {
	for _, kind := range []string{"capability", "active", "parent", "lease", "binding"} {
		t.Run(kind, func(t *testing.T) {
			path, parent, svc := completedRecoveryFixture(t)
			req := nextRequest(t, parent)
			original := parent
			switch kind {
			case "capability":
				svc.old = true
			case "active":
				svc.active = true
			case "parent":
				parent.Target = protocol.Hash("other recovery")
			case "binding":
				svc.binding = protocol.Hash("other service")
			case "lease":
				v, err := OpenRecoveryVerification(path, parent)
				if err != nil {
					t.Fatal(err)
				}
				defer v.Close()
			}
			apply, verify, calls := recoveryCallbacks(t, path)
			if _, err := FenceNextRecovery(context.Background(), path, parent, req, svc, apply, verify); err == nil || *calls != 0 {
				t.Fatal("preflight accepted", err, *calls)
			}
			if _, err := os.Stat(filepath.Join(path, nextRecoveryName(original))); !os.IsNotExist(err) {
				t.Fatal("preflight wrote next intent", err)
			}
			if current, err := InspectRecovery(path); err != nil || current != original {
				t.Fatal("preflight changed account", current, err)
			}
		})
	}
}
