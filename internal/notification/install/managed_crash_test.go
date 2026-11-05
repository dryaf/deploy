package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type managedCrashInput struct {
	ManifestHash, QualificationHash string
	Spec                            ManagedSpec
	Operation, Boundary             string
	Service                         managedServiceState
	Revision                        string
}

func TestManagedCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_MANAGED_CRASH_INPUT")
	if path == "" {
		t.Skip("subprocess helper")
	}
	d, err := safefs.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var in managedCrashInput
	if err = d.ReadJSON("input.json", &in); err != nil {
		t.Fatal(err)
	}
	f := &fakeManagedServices{t: t, units: in.Spec.UnitDirectory, state: in.Service}
	api := &managedTestLED{states: map[string]led.State{}}
	p := &managedTelegramTransport{}
	i := managedInstaller{services: f, probe: func(context.Context, *safefs.Directory, string, string, bool) error { return nil }, fault: func(point string) error {
		if point != in.Boundary {
			return nil
		}
		if err := d.WriteJSON("manager.json", f.state, false); err != nil {
			return err
		}
		if in.Operation == "migrate-led" {
			if err := d.WriteJSON("led-registry.json", api.states, false); err != nil {
				return err
			}
		}
		if in.Operation == "renew-telegram" {
			if err := d.WriteJSON("provider-calls.json", p.calls, false); err != nil {
				return err
			}
		}
		return syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}}
	i.signalProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.signalProfilesProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.signalCheck = func(ctx context.Context, m ManagedManifest) error {
		if m.SignalVerification.ServiceRoot != "" {
			service, e := inspectSignalServiceRetained(m.SignalVerification.SignalVerificationReference, false)
			if e != nil {
				return e
			}
			v, e := openSignalEndpointVerifierService(ctx, m.SignalVerification.Root, m.SignalVerification.ManifestHash, service.binding())
			if e != nil {
				return e
			}
			defer v.close()
			return v.check(ctx)
		}
		v, e := openSignalEndpointVerifier(ctx, m.SignalVerification.Root, m.SignalVerification.ManifestHash)
		if e != nil {
			return e
		}
		v.close()
		return nil
	}
	i.telegramTransport = p
	i.telegramProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.helperProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.ledAPI = func(string) worker.LEDAPI { return api }
	i.recoveryProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.ledProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	if in.Operation == "activate-delivery" || in.Operation == "deactivate-delivery" {
		i.services = &fakeDeliveryServices{f}
		i.deliveryProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
		i.deliveryInspect = func(ctx context.Context, m ManagedManifest, _ bool) error {
			_, err := inspectManagedService(ctx, f, m, false)
			return err
		}
		_, err = i.prepareDelivery(context.Background(), in.Operation, in.Spec.Root, in.ManifestHash, in.QualificationHash, false)
	} else if in.Operation == "renew-telegram" {
		_, err = i.prepareTelegramRenewal(context.Background(), in.Spec.Root)
	} else if in.Operation == "uninstall" {
		_, err = i.uninstall(context.Background(), in.Spec.Root)
	} else if in.Operation == "rollback" {
		_, err = i.rollback(context.Background(), in.Spec.Root, in.Revision)
	} else {
		_, err = i.prepare(context.Background(), in.Operation, in.Spec)
	}
	t.Fatal("SIGKILL boundary not reached", err)
}

func TestManagedSIGKILLRecovery(t *testing.T) {
	for _, op := range []string{"setup", "update", "uninstall", "abort-setup", "abort-update", "rollback", "migrate-schema", "migrate-led", "upgrade-helper"} {
		points := []string{"prepared", "stopped", "unit-published", "manifest-published", "reloaded", "started", "complete"}
		if op == "migrate-schema" {
			points = append(points, "schema-migrated")
		}
		if op == "migrate-led" {
			points = append(points, "led-migrated")
		}
		for _, point := range points {
			t.Run(op+"/"+point, func(t *testing.T) {
				var spec ManagedSpec
				var i managedInstaller
				var f *fakeManagedServices
				var id protocol.Identity
				var api *managedTestLED
				if op == "migrate-led" {
					var old ManagedManifest
					spec, i, f, api, old, _ = managedLEDSetup(t)
					id = old.Identity
				} else {
					spec, i, f, id = managedFixture(t)
				}
				if op == "upgrade-helper" {
					i.helperProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
				}
				var selected ManagedManifest
				if op != "setup" && op != "abort-setup" && op != "migrate-led" {
					if _, err := i.prepare(context.Background(), "setup", spec); err != nil {
						t.Fatal(err)
					}
				}
				if op == "rollback" {
					d, err := privateManagedDir(spec.Root, true)
					if err != nil {
						t.Fatal(err)
					}
					j, err := readManagedJournal(d)
					d.Close()
					if err != nil {
						t.Fatal(err)
					}
					selected = j.Intent.After
					binary, err := os.ReadFile(spec.BinaryFile)
					if err != nil {
						t.Fatal(err)
					}
					binary = append(binary, []byte("crash rollback candidate")...)
					if err = os.WriteFile(spec.BinaryFile, binary, 0700); err != nil {
						t.Fatal(err)
					}
					spec.BinarySHA256 = managedDigest(binary)
					if _, err = i.prepare(context.Background(), "update", spec); err != nil {
						t.Fatal(err)
					}
				}
				if op == "migrate-schema" {
					downgradeManagedSchemaFixture(t, spec)
				}
				childOperation := op
				if strings.HasPrefix(op, "abort-") {
					i.fault = func(p string) error {
						if p == "reloaded" {
							return fmt.Errorf("unhealthy installation")
						}
						return nil
					}
					if _, err := i.prepare(context.Background(), strings.TrimPrefix(op, "abort-"), spec); err == nil {
						t.Fatal("installation not interrupted")
					}
					i.fault = nil
					childOperation = "uninstall"
				}
				dir := t.TempDir()
				d, err := safefs.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				if err = d.WriteJSON("input.json", managedCrashInput{Spec: spec, Operation: childOperation, Boundary: point, Service: f.state, Revision: selected.Revision}, false); err != nil {
					t.Fatal(err)
				}
				executable, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(executable, "-test.run=^TestManagedCrashHelper$", "-test.timeout=15s")
				cmd.Env = append(os.Environ(), "DEPLOY_MANAGED_CRASH_INPUT="+dir)
				out, err := process.LimitedOutput(cmd, 20*time.Second, 8192)
				if err == nil || cmd.ProcessState == nil {
					t.Fatal("helper did not crash", err, string(out))
				}
				status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatal("wrong exit", fmt.Sprint(cmd.ProcessState), err, string(out))
				}
				if err = d.ReadJSON("manager.json", &f.state); err != nil {
					t.Fatal(err)
				}
				if api != nil {
					if err = d.ReadJSON("led-registry.json", &api.states); err != nil {
						t.Fatal(err)
					}
				}
				if err = os.Remove(spec.BinaryFile); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(spec.RuntimeFile); err != nil {
					t.Fatal(err)
				}
				m, err := i.resume(context.Background(), spec.Root)
				if err != nil {
					t.Fatal(err)
				}
				if op == "setup" || op == "update" || op == "rollback" || op == "migrate-schema" || op == "migrate-led" || op == "upgrade-helper" {
					checkManagedInstalled(t, spec, m, id)
					if op == "rollback" && m.BinaryHash != selected.BinaryHash {
						t.Fatal("crash resumed wrong executable")
					}
				} else {
					if m.Identity != id || m.Mode != "uninstalled" {
						t.Fatal(m)
					}
					if _, err = os.Stat(filepath.Join(spec.State, "state.db")); err != nil {
						t.Fatal("state removed", err)
					}
				}
			})
		}
	}
}
