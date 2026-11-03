package signalaccount

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

// Persistent fake manager activity survives coordinator death. This tests real
// process/lock/journal recovery, not systemd or a live Signal account.
type crashService struct{ path, binding string }

func (s crashService) Inspect(context.Context) (ServiceState, error) {
	raw, err := os.ReadFile(filepath.Join(s.path, "test-service"))
	return ServiceState{s.binding, string(raw) == "active"}, err
}
func (s crashService) Stop(context.Context) error {
	return os.WriteFile(filepath.Join(s.path, "test-service"), []byte("inactive"), 0600)
}
func (s crashService) Start(context.Context) error {
	lease, err := AcquireRuntime(s.path)
	if err != nil {
		return err
	}
	defer lease.Close()
	return os.WriteFile(filepath.Join(s.path, "test-service"), []byte("active"), 0600)
}
func crashLaunch(path string, request ServiceRequest) func(context.Context) error {
	return func(ctx context.Context) error {
		l := linker{now: time.Now, run: func(context.Context, *Lease, string, io.Writer) error {
			f, err := os.OpenFile(filepath.Join(path, "test-attempts"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err = f.WriteString("attempt\n"); err != nil {
				return err
			}
			return f.Sync()
		}}
		_, err := l.link(ctx, path, request.Name, request.Operation, io.Discard)
		return err
	}
}
func TestSignalMaintenanceCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_SIGNAL_MAINTENANCE_CRASH_STATE")
	if path == "" {
		t.Skip("subprocess helper")
	}
	var input struct {
		Request ServiceRequest
		Point   string
	}
	raw, err := os.ReadFile(filepath.Join(path, "test-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	c := serviceCoordinator{now: time.Now, fault: func(point string) error {
		if point == input.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	_, err = c.run(context.Background(), path, input.Request, crashService{path, input.Request.Binding}, crashLaunch(path, input.Request))
	t.Fatal("kill boundary not reached", err)
}
func TestSignalMaintenanceSIGKILL(t *testing.T) {
	points := []string{"maintenance_child_returned"}
	for _, phase := range maintenancePhases {
		points = append(points, "maintenance_"+phase+"_record", "maintenance_"+phase+"_head")
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			path, request, _, _, _ := maintenanceFixture(t, false)
			raw, _ := json.Marshal(struct {
				Request ServiceRequest
				Point   string
			}{request, point})
			for name, body := range map[string][]byte{"test-request.json": raw, "test-service": []byte("active")} {
				if err := os.WriteFile(filepath.Join(path, name), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestSignalMaintenanceCrashHelper$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "DEPLOY_SIGNAL_MAINTENANCE_CRASH_STATE="+path)
			out, err := process.LimitedOutput(cmd, 20*time.Second, 8192)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong exit", cmd.ProcessState, string(out))
			}
			view, err := InspectMaintenance(path)
			if err != nil {
				t.Fatal(err)
			}
			requireRuntime(t, path, view.RuntimeAllowed)
			svc := crashService{path, request.Binding}
			view, err = MaintainLink(context.Background(), path, request, svc, crashLaunch(path, request))
			if err != nil || view.Phase != "complete" || !view.BeforeActive {
				t.Fatal(view, err)
			}
			activity, err := svc.Inspect(context.Background())
			if err != nil || !activity.Active {
				t.Fatal(activity, err)
			}
			attempts, err := os.ReadFile(filepath.Join(path, "test-attempts"))
			if err != nil || string(attempts) != "attempt\n" {
				t.Fatal("link repeated after process death", string(attempts), err)
			}
		})
	}
}
