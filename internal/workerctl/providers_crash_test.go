package workerctl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

type providerCrashFixture struct {
	Request  ControlRequest
	Paths    ResolvedPaths
	Observed install.InstallationObservation
	Calls    int
}

func providerCrashLifecycle(t *testing.T, file string, fixture *providerCrashFixture) notificationLimitsLifecycle {
	t.Helper()
	return notificationLimitsLifecycle{
		inspect: func(context.Context, string) (install.InstallationObservation, error) { return fixture.Observed, nil },
		migrate: func(ctx context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
			if protocol.Hash(fixture.Observed.Manifest) != expected {
				t.Fatal("changed provider predecessor")
			}
			old, err := readLimitsRuntime(fixture.Paths.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := readLimitsRuntime(spec.RuntimeFile)
			if err != nil {
				t.Fatal(err)
			}
			var before, after install.RuntimeInput
			if err = safefs.DecodeJSON(old, &before, maxControlFrame); err != nil {
				t.Fatal(err)
			}
			if err = safefs.DecodeJSON(raw, &after, maxControlFrame); err != nil {
				t.Fatal(err)
			}
			b, err := before.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			a, err := after.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			op, _ := safefs.NewID("n")
			if protocol.Hash(b.Worker) != protocol.Hash(a.Worker) {
				if _, err = worker.MigrateBootstrapRoutes(ctx, spec.State, fixture.Observed.Manifest.Identity, b.Worker, a.Worker, op); err != nil {
					t.Fatal(err)
				}
			} else if err = worker.PreflightBootstrapRoutes(ctx, spec.State, fixture.Observed.Manifest.Identity, b.Worker, a.Worker); err != nil {
				t.Fatal(err)
			}
			m := &fixture.Observed.Manifest
			m.ConfigHash, m.RuntimeHash = protocol.Hash(a), inputDigest(raw)
			m.Revision, _ = safefs.NewID("n")
			m.HelperRevision, m.HelperHash = m.Revision, m.BinaryHash
			fixture.Calls++
			if file != "" {
				raw, err = json.Marshal(fixture)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(file, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			return *m, nil
		},
	}
}

func TestNotificationProvidersProcessDeath(t *testing.T) {
	if file := os.Getenv("DEPLOY_PROVIDERS_CRASH_INPUT"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var f providerCrashFixture
		if err = json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		l := providerCrashLifecycle(t, file, &f)
		l.boundary = func(at string) error {
			if at == os.Getenv("DEPLOY_PROVIDERS_CRASH_AT") {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
			return nil
		}
		_, _, err = reconcileNotificationProviders(context.Background(), f.Request, f.Paths, "/helper", l)
		t.Fatal("crash boundary not reached", err)
	}
	for _, audience := range []bool{false, true} {
		for _, at := range []string{"limits-intent", "limits-domain-complete", "limits-runtime-published", "limits-inputs-published"} {
			t.Run(at+map[bool]string{false: "/token", true: "/audience"}[audience], func(t *testing.T) {
				r, paths, o, d := notificationLimitsFixture(t)
				changeDeclaredTelegramFiles(t, &r, paths, audience)
				r.DesiredHash, _ = r.Desired.Fingerprint()
				f := providerCrashFixture{Request: r, Paths: paths, Observed: *o}
				file := filepath.Join(t.TempDir(), "fixture.json")
				raw, _ := json.Marshal(f)
				if err := os.WriteFile(file, raw, 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNotificationProvidersProcessDeath$")
				cmd.Env = append(os.Environ(), "DEPLOY_PROVIDERS_CRASH_INPUT="+file, "DEPLOY_PROVIDERS_CRASH_AT="+at)
				out, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
					t.Fatalf("not killed: %v %s", err, out)
				}
				raw, err = os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(raw, &f); err != nil {
					t.Fatal(err)
				}
				var pending OperationJournal
				if err = d.ReadJSON("operation.json", &pending); err != nil {
					t.Fatal(err)
				}
				l := providerCrashLifecycle(t, file, &f)
				for range 2 {
					if got, handled, err := reconcileNotificationProviders(ctx, r, paths, "/helper", l); !handled || err != nil || got.Code != "ok" {
						t.Fatal(got, handled, err)
					}
				}
				var complete OperationJournal
				if err = d.ReadJSON("operation.json", &complete); err != nil {
					t.Fatal(err)
				}
				if !complete.Complete || complete.ID != pending.ID || f.Calls != 1 || f.Observed.Manifest.Identity != o.Manifest.Identity {
					t.Fatal("recovery replaced operation or repeated provider migration")
				}
			})
		}
	}
}

func TestNotificationProvidersRejectsCredentialDriftAfterIntent(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "old"}[old], func(t *testing.T) {
			r, paths, o, d := notificationLimitsFixture(t)
			raw, err := readLimitsRuntime(paths.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			var input install.RuntimeInput
			if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
				t.Fatal(err)
			}
			changeDeclaredTelegramFiles(t, &r, paths, true)
			r.DesiredHash, _ = r.Desired.Fingerprint()
			l := notificationLimitsLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil }, boundary: func(at string) error {
				if at == "limits-intent" {
					return errors.New("stop")
				}
				return nil
			}, migrate: func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error) {
				t.Fatal("drift crossed provider boundary")
				return install.ManagedManifest{}, nil
			}}
			if _, _, err = reconcileNotificationProviders(context.Background(), r, paths, "/helper", l); err == nil {
				t.Fatal("missing interruption")
			}
			var pending OperationJournal
			if err = d.ReadJSON("operation.json", &pending); err != nil {
				t.Fatal(err)
			}
			path := r.Desired.Notification.Providers["telegram"].TokenFile
			if old {
				path = input.Providers["telegram"].TokenFile
			}
			if err = os.WriteFile(path, []byte("123:tampered"), 0600); err != nil {
				t.Fatal(err)
			}
			l.boundary = nil
			if _, _, err = reconcileNotificationProviders(context.Background(), r, paths, "/helper", l); err == nil {
				t.Fatal("changed credential resumed")
			}
			var after OperationJournal
			if err = d.ReadJSON("operation.json", &after); err != nil {
				t.Fatal(err)
			}
			if after.Digest() != pending.Digest() {
				t.Fatal("drift changed pending authority")
			}
		})
	}
}
