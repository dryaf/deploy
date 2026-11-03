//go:build notificationintegration

package install

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

func TestManagedTelegramNativeExecutable(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(out))
	}
	b, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	for _, interrupted := range []bool{false, true} {
		name := "complete"
		if interrupted {
			name = "resume-without-credential"
		}
		t.Run(name, func(t *testing.T) {
			var native *executableManagedServices
			spec, i, _, p := managedTelegramFixture(t, func(spec *ManagedSpec, i *managedInstaller, f *fakeManagedServices) {
				short, e := os.MkdirTemp("", "telegram-native-")
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { os.RemoveAll(short) })
				state := filepath.Join(short, "state")
				if e = os.Rename(spec.State, state); e != nil {
					t.Fatal(e)
				}
				spec.State = state
				if e = os.WriteFile(spec.BinaryFile, b, 0700); e != nil {
					t.Fatal(e)
				}
				spec.BinarySHA256 = managedDigest(b)
				native = &executableManagedServices{fakeManagedServices: f, root: spec.Root}
				i.services, i.probe = native, probeManagedBinary
				i.recoveryProbe, i.telegramProbe, i.helperProbe = nil, nil, nil
				t.Cleanup(func() {
					if native.cmd != nil {
						native.cmd.Process.Kill()
						select {
						case <-native.done:
						case <-time.After(5 * time.Second):
						}
						if native.log != nil {
							native.log.Close()
						}
					}
				})
			})
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			spec.RecoveryCommitHash = ""
			// Exercise the real helper capability protocol as well as its restart.
			if _, e := i.prepare(ctx, "upgrade-helper", spec); e != nil {
				t.Fatal(e)
			}
			stop := errors.New("after durable worker renewal")
			if interrupted {
				i.fault = func(at string) error {
					if at == "telegram-renewed" {
						return stop
					}
					return nil
				}
			}
			m, e := i.prepareTelegramRenewal(ctx, spec.Root)
			if interrupted {
				if !errors.Is(e, stop) {
					t.Fatal(e)
				}
				c, readErr := ReadRuntime(spec.RuntimeFile)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if e = os.Remove(c.Telegram.TokenFile); e != nil {
					t.Fatal(e)
				}
				p.before = func(*http.Request) error { t.Fatal("resume repeated provider work"); return nil }
				i.fault = nil
				m, e = i.resume(ctx, spec.Root)
			}
			if e != nil {
				out, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
				t.Fatal(e, string(out))
			}
			managedTelegramReceiptForTest(t, m)
			api := client.Client{Socket: m.Paths.Socket, Identity: m.Identity}
			var h worker.Health
			if _, e = api.Call(ctx, protocol.Request{Operation: "system.status"}, &h); e != nil {
				t.Fatal(e)
			}
			if !h.VerificationOnly || h.TelegramRenewalID != m.TelegramRenewalID || h.InstallationRevision != m.Revision || h.RecoveryCommitHash != m.RecoveryCommitHash || p.calls != 3 {
				t.Fatal(h, p.calls)
			}
			stale := m
			stale.TelegramRenewalID = ""
			if e = (userManagedServices{}).Verify(ctx, stale); e == nil {
				t.Fatal("health accepted stale renewal pin")
			}
			// The process holds the worker lease; completed resume must use health.
			if _, e = i.resume(ctx, spec.Root); e != nil {
				t.Fatal(e)
			}
			if _, e = i.prepare(ctx, "update", spec); e != nil {
				t.Fatal(e)
			}
			if _, e = i.uninstall(ctx, spec.Root); e != nil {
				t.Fatal(e)
			}
		})
	}
}
