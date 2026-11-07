package ledhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A filesystem marker stands in for systemd's independently retained state.
// It survives the installer process, unlike an in-process fake's booleans.
type durableServices struct{ root string }

func (s durableServices) Inspect(_ context.Context, _ Spec, owned bool) error {
	if _, err := os.Stat(filepath.Join(s.root, "service-active")); err == nil && !owned {
		return errors.New("unowned active service")
	}
	return nil
}
func (s durableServices) Reload(context.Context) error { return nil }
func (s durableServices) Start(context.Context, Spec) error {
	f, err := os.OpenFile(filepath.Join(s.root, "service-active"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write([]byte("one start"))
	if err == nil {
		err = f.Sync()
	}
	e := f.Close()
	if err != nil {
		return err
	}
	return e
}
func (s durableServices) Verify(context.Context, Spec) error {
	_, err := os.Stat(filepath.Join(s.root, "service-active"))
	return err
}
func (s durableServices) Stop(context.Context, Spec) error {
	err := os.Remove(filepath.Join(s.root, "service-active"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (s durableServices) VerifyStopped(context.Context, Spec) error {
	_, err := os.Stat(filepath.Join(s.root, "service-active"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.New("service remains active or unverifiable")
}

func TestHostInstallerCrashProcess(t *testing.T) {
	root := os.Getenv("DEPLOY_TEST_LEDHOST_CRASH_ROOT")
	if root == "" {
		t.Skip("process-death child")
	}
	artifact, err := os.ReadFile(filepath.Join(root, "input.pyz"))
	if err != nil {
		t.Fatal(err)
	}
	s := Spec{Version: 1, UID: 1000, Device: "ACT", DevicePath: "/sys/devices/platform/leds/leds/ACT", ArtifactSHA256: bytesHash(artifact)}
	if os.Getenv("DEPLOY_TEST_LEDHOST_POWER") == "1" {
		s = powerSpec(s)
	}
	m := manager{prefix: root, owner: os.Getuid(), services: durableServices{root}, validateHost: func(context.Context, Spec) error { return nil }, verifyDaemon: func(context.Context, Spec) error { return nil }, boundary: func(at string) error {
		if at == os.Getenv("DEPLOY_TEST_LEDHOST_CRASH_BOUNDARY") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	if os.Getenv("DEPLOY_TEST_LEDHOST_CRASH_REMOVAL") == "1" {
		m.prepareRemoval = func(_ context.Context, s Spec, hash string) error { return fakeRemovalFence(m, s, hash) }
		p, err := BuildRemoval(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = m.remove(context.Background(), s, p.Hash); err != nil {
			t.Fatal(err)
		}
		t.Fatal("removal child reached completion without requested SIGKILL")
	}
	p, err := Build(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.install(context.Background(), s, p.Hash, artifact); err != nil {
		t.Fatal(err)
	}
	t.Fatal("child reached completion without requested SIGKILL")
}

func TestHostRemovalSurvivesProcessDeath(t *testing.T) {
	for _, power := range []bool{false, true} {
		t.Run(fmt.Sprint("power=", power), func(t *testing.T) { runTestHostRemovalSurvivesProcessDeath(t, power) })
	}
}

func runTestHostRemovalSurvivesProcessDeath(t *testing.T, power bool) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, boundary := range []string{"remove-prepared", "remove-fenced", "remove-stop-returned", "removed:deploy-led-host-1000.socket", "removed:deploy-led-host-1000.service", "remove-receipt", "remove-complete"} {
		t.Run(boundary, func(t *testing.T) {
			m, s, artifact, _ := fixture(t)
			if power {
				s = powerSpec(s)
			}
			m.services = durableServices{m.prefix}
			p, _ := Build(s)
			if _, err = m.install(context.Background(), s, p.Hash, artifact); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filepath.Join(m.prefix, "input.pyz"), artifact, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestHostInstallerCrashProcess$")
			cmd.Env = append(os.Environ(), "DEPLOY_TEST_LEDHOST_CRASH_ROOT="+m.prefix, "DEPLOY_TEST_LEDHOST_CRASH_BOUNDARY="+boundary, "DEPLOY_TEST_LEDHOST_CRASH_REMOVAL=1")
			if power {
				cmd.Env = append(cmd.Env, "DEPLOY_TEST_LEDHOST_POWER=1")
			}
			err = cmd.Run()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL || ctx.Err() != nil {
				t.Fatal("removal child did not die at boundary", err)
			}
			remove, _ := BuildRemoval(s)
			r, err := m.remove(context.Background(), s, remove.Hash)
			if err != nil || !r.Complete {
				t.Fatal("removal failed to resume after process death", r, err)
			}
			if _, err = os.Stat(filepath.Join(m.prefix, "service-active")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("removed service remains", err)
			}
		})
	}
}

func TestHostInstallerSurvivesProcessDeath(t *testing.T) {
	for _, power := range []bool{false, true} {
		t.Run(fmt.Sprint("power=", power), func(t *testing.T) { runTestHostInstallerSurvivesProcessDeath(t, power) })
	}
}

func runTestHostInstallerSurvivesProcessDeath(t *testing.T, power bool) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, boundary := range []string{"prepared", "published:config.toml", "files", "started", "receipt", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			m, s, artifact, _ := fixture(t)
			if power {
				s = powerSpec(s)
			}
			m.services = durableServices{m.prefix}
			if err := os.WriteFile(filepath.Join(m.prefix, "input.pyz"), artifact, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestHostInstallerCrashProcess$")
			cmd.Env = append(os.Environ(), "DEPLOY_TEST_LEDHOST_CRASH_ROOT="+m.prefix, "DEPLOY_TEST_LEDHOST_CRASH_BOUNDARY="+boundary)
			if power {
				cmd.Env = append(cmd.Env, "DEPLOY_TEST_LEDHOST_POWER=1")
			}
			err := cmd.Run()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL || ctx.Err() != nil {
				t.Fatal("child did not die at the requested boundary", err)
			}
			p, _ := Build(s)
			r, err := m.install(context.Background(), s, p.Hash, artifact)
			if err != nil || !r.Complete {
				t.Fatal("failed to resume actual process death", r, err)
			}
			marker, err := os.ReadFile(filepath.Join(m.prefix, "service-active"))
			if err != nil || string(marker) != "one start" {
				t.Fatal("lost service authority", err)
			}
		})
	}
}
