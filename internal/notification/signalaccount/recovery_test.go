package signalaccount

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type recoveryServiceFixture struct {
	*fixtureService
	unsupported bool
}

func (s *recoveryServiceFixture) VerifyRecoverySupport(context.Context) error {
	if s.unsupported {
		return errors.New("old runtime")
	}
	return nil
}
func recoveryFixture(t *testing.T) (string, RecoveryRequest, *recoveryServiceFixture) {
	t.Helper()
	path, request, svc, _, _ := maintenanceFixture(t, true)
	return path, RecoveryRequest{request.Operation, request.Binding, protocol.Hash("gateway/inbox/configuration")}, &recoveryServiceFixture{fixtureService: svc}
}
func recoveryCallbacks(t *testing.T, path string) (RecoveryApply, RecoveryVerify, *int) {
	t.Helper()
	calls := 0
	result := protocol.Hash("retained-installation-proof")
	apply := func(_ context.Context, check func() error) (string, error) {
		calls++
		if err := check(); err != nil {
			return "", err
		}
		if _, err := AcquireRuntime(path); err == nil {
			t.Fatal("runtime entered recovery")
		}
		return result, os.WriteFile(filepath.Join(path, "fixture-result"), []byte(result), 0600)
	}
	verify := func(_ context.Context, hash string, check func() error) error {
		if err := check(); err != nil {
			return err
		}
		raw, err := os.ReadFile(filepath.Join(path, "fixture-result"))
		if err != nil {
			return err
		}
		if string(raw) != hash {
			return errors.New("lost independent result")
		}
		return nil
	}
	return apply, verify, &calls
}
func TestSignalRecoveryFenceAndReplay(t *testing.T) {
	path, req, svc := recoveryFixture(t)
	apply, verify, calls := recoveryCallbacks(t, path)
	ctx := context.Background()
	v, err := FenceRecovery(ctx, path, req, svc, apply, verify)
	if err != nil || v.RuntimeAllowed || v.Phase != "complete" || svc.active || svc.starts != 0 || *calls != 1 {
		t.Fatal(v, err, *calls)
	}
	again, err := FenceRecovery(ctx, path, req, svc, apply, verify)
	if err != nil || again != v || *calls != 1 {
		t.Fatal(again, err, *calls)
	}
	status, err := InspectRecovery(path)
	if err != nil || status != v {
		t.Fatal(status, err)
	}
	requireRuntime(t, path, false)
	linked, err := Inspect(path)
	if err != nil || linked.RuntimeAllowed {
		t.Fatal(linked, err)
	}
	maintenance, err := InspectMaintenance(path)
	if err != nil || maintenance.RuntimeAllowed {
		t.Fatal(maintenance, err)
	}
	id, _ := safefs.NewID("n")
	if _, err = Link(ctx, path, "fixture", id, io.Discard); err == nil {
		t.Fatal("link bypassed recovery")
	}
	if _, err = Resolve(path, id, "reviewed"); err == nil {
		t.Fatal("resolution bypassed recovery")
	}
	stops := svc.stops
	if _, err = MaintainLink(ctx, path, ServiceRequest{id, "fixture", req.Binding}, svc, func(context.Context) error { t.Fatal("link launched"); return nil }); err == nil || svc.stops != stops {
		t.Fatal("maintenance bypassed recovery", err)
	}
	req.Target = protocol.Hash("different target")
	if _, err = FenceRecovery(ctx, path, req, svc, apply, verify); err == nil {
		t.Fatal("target changed")
	}
	if err = os.Remove(filepath.Join(path, "fixture-result")); err != nil {
		t.Fatal(err)
	}
	req.Target = v.Target
	if _, err = FenceRecovery(ctx, path, req, svc, apply, verify); err == nil || *calls != 1 {
		t.Fatal("completed result reapplied", err)
	}
}
func TestSignalRecoveryInterrupted(t *testing.T) {
	for _, point := range []string{"recovery_intent", "recovery_fence", "recovery_stop", "recovery_applied", "recovery_result", "recovery_complete"} {
		t.Run(point, func(t *testing.T) {
			path, req, svc := recoveryFixture(t)
			apply, verify, calls := recoveryCallbacks(t, path)
			stop := errors.New("interrupted")
			c := recoveryCoordinator{fault: func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}}
			if _, err := c.run(context.Background(), path, req, svc, apply, verify); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			requireRuntime(t, path, false)
			before := *calls
			v, err := FenceRecovery(context.Background(), path, req, svc, apply, verify)
			if err != nil || v.Phase != "complete" || v.RuntimeAllowed {
				t.Fatal(v, err)
			}
			if (point == "recovery_result" || point == "recovery_complete") && *calls != before {
				t.Fatal("retained result applied again")
			}
		})
	}
}
func TestSignalRecoveryRejectsChangedAuthority(t *testing.T) {
	for _, kind := range []string{"capability", "binding", "active", "lease", "operation-lease", "directory", "intent-loss", "fence-loss", "result-loss", "completion-change"} {
		t.Run(kind, func(t *testing.T) {
			path, req, svc := recoveryFixture(t)
			apply, verify, _ := recoveryCallbacks(t, path)
			switch kind {
			case "capability":
				svc.unsupported = true
			case "binding":
				svc.binding = protocol.Hash("other service")
			default:
				original := apply
				apply = func(ctx context.Context, check func() error) (string, error) {
					result, err := original(ctx, check)
					if err != nil {
						return "", err
					}
					switch kind {
					case "active":
						svc.active = true
					case "lease", "operation-lease":
						name := "deploy-account.lock"
						if kind == "operation-lease" {
							name = "deploy-service-link.lock"
						}
						old := filepath.Join(path, name)
						if err = os.Rename(old, old+".old"); err != nil {
							return "", err
						}
						err = os.WriteFile(old, nil, 0600)
					case "directory":
						if err = os.Rename(path, path+"-moved"); err == nil {
							t.Cleanup(func() { os.RemoveAll(path + "-moved") })
							err = os.Mkdir(path, 0700)
						}
					case "intent-loss":
						err = os.Remove(filepath.Join(path, recoveryIntentFile))
					case "fence-loss":
						err = os.Remove(filepath.Join(path, recoveryFenceFile))
					}
					return result, err
				}
			}
			v, err := FenceRecovery(context.Background(), path, req, svc, apply, verify)
			if kind == "result-loss" || kind == "completion-change" {
				if err != nil {
					t.Fatal(err)
				}
				if kind == "result-loss" {
					err = os.Remove(filepath.Join(path, recoveryResultFile))
				} else {
					err = os.WriteFile(filepath.Join(path, recoveryCompleteFile), []byte("{}"), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				_, err = FenceRecovery(context.Background(), path, req, svc, apply, verify)
			}
			if err == nil {
				t.Fatal("changed authority accepted", kind, v)
			}
			if kind == "capability" || kind == "binding" {
				if svc.stops != 0 {
					t.Fatal("preflight stopped service")
				}
				if _, err = os.Stat(filepath.Join(path, recoveryIntentFile)); !os.IsNotExist(err) {
					t.Fatal("preflight wrote intent")
				}
			}
		})
	}
}
func TestSignalRecoveryRequiresQuiescentChildren(t *testing.T) {
	path, req, svc := recoveryFixture(t)
	apply, verify, calls := recoveryCallbacks(t, path)
	// Retain the old open-file description while the service reports stopped.
	held := svc.lease
	svc.lease = nil
	defer held.Close()
	if _, err := FenceRecovery(context.Background(), path, req, svc, apply, verify); err == nil || *calls != 0 {
		t.Fatal("busy child ignored", err)
	}
	held.Close()
	if _, err := FenceRecovery(context.Background(), path, req, svc, apply, verify); err != nil {
		t.Fatal(err)
	}
}

func TestSignalRecoveryLostLeaseAndHistory(t *testing.T) {
	for _, kind := range []string{"lease", "link-history", "maintenance-history"} {
		t.Run(kind, func(t *testing.T) {
			path, req, svc := recoveryFixture(t)
			apply, verify, _ := recoveryCallbacks(t, path)
			if _, err := FenceRecovery(context.Background(), path, req, svc, apply, verify); err != nil {
				t.Fatal(err)
			}
			name := "deploy-account.lock"
			if kind == "lease" {
				if err := os.Remove(filepath.Join(path, name)); err != nil {
					t.Fatal(err)
				}
			} else {
				d, err := accountDirectory(path)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				if kind == "link-history" {
					err = writeRecord(d, headFile, linkHead{Version: 1, Sequence: 1, ID: req.Operation, Hash: protocol.Hash("unknown link")}, false)
				} else {
					err = writeRecord(d, maintenanceHeadFile, maintenanceHead{Sequence: 1, Phase: "prepared", Hash: protocol.Hash("unknown maintenance")}, false)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := InspectRecovery(path); err == nil {
				t.Fatal("lost authority accepted by recovery inspection")
			}
			if _, err := Inspect(path); err == nil {
				t.Fatal("lost authority accepted by linking inspection")
			}
			requireRuntime(t, path, false)
			if _, err := FenceRecovery(context.Background(), path, req, svc, apply, verify); err == nil {
				t.Fatal("lost authority repaired")
			}
			if kind == "lease" {
				if _, err := os.Stat(filepath.Join(path, name)); !os.IsNotExist(err) {
					t.Fatal("diagnostics recreated missing account lease")
				}
			}
		})
	}
}
