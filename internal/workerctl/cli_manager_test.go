//go:build linux

package workerctl

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

func TestNativeCLIManagerHonorsUnitStopBudget(t *testing.T) {
	if ready := os.Getenv("DEPLOY_TEST_MANAGER_DELAYED_STOP"); ready != "" {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGTERM)
		if err := os.WriteFile(ready, []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		select {
		case <-stop:
			time.Sleep(6 * time.Second)
			return
		case <-time.After(20 * time.Second):
			t.Fatal("fixture never received stop")
		}
	}
	root := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "worker")
	if err = os.WriteFile(executable, binary, 0700); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(root, "config/systemd/user/deploy-notification-worker.service")
	if err = os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(unit, []byte("[Service]\nTimeoutStopSec=8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	manager := filepath.Join(root, "systemctl")
	if err = os.WriteFile(manager, []byte(nativeCLIManager), 0700); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "ready")
	child := exec.Command(executable, "-test.run=^TestNativeCLIManagerHonorsUnitStopBudget$", "-test.timeout=25s")
	child.Env = append(os.Environ(), "DEPLOY_TEST_MANAGER_DELAYED_STOP="+ready)
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Wait()
	defer child.Process.Kill()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err = os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("delayed worker did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	state, err := json.Marshal(map[string]any{"PID": child.Process.Pid, "ActiveState": "active"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "manager.json"), state, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(manager, "--user", "stop", "deploy-notification-worker.service")
	command.Env = append(os.Environ(), "DEPLOY_WORKER_E2E_ROOT="+root)
	if _, err = process.LimitedOutput(command, 15*time.Second, 4096); err != nil {
		detail, _ := os.ReadFile(filepath.Join(root, "manager-errors.log"))
		t.Fatalf("manager stopped waiting before unit deadline: %v\n%s", err, detail)
	}
	if err = child.Wait(); err != nil {
		t.Fatal(err)
	}
}
