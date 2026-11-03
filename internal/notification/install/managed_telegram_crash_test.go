package install

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestManagedTelegramSIGKILL(t *testing.T) {
	for _, point := range []string{"prepared", "stop-returned", "telegram-request", "telegram-renewed", "telegram-receipt", "manifest-published", "published", "start-returned"} {
		t.Run(point, func(t *testing.T) {
			spec, i, f, p := managedTelegramFixture(t, nil)
			dir := t.TempDir()
			d, err := safefs.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err = d.WriteJSON("input.json", managedCrashInput{Spec: spec, Operation: "renew-telegram", Boundary: point, Service: f.state}, false); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestManagedCrashHelper$", "-test.timeout=45s")
			cmd.Env = append(os.Environ(), "DEPLOY_MANAGED_CRASH_INPUT="+dir)
			out, err := process.LimitedOutput(cmd, 50*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected exit", string(out))
			}
			if err = d.ReadJSON("manager.json", &f.state); err != nil {
				t.Fatal(err)
			}
			var calls int
			if err = d.ReadJSON("provider-calls.json", &calls); err != nil {
				t.Fatal(err)
			}
			if calls != 0 && calls != 3 {
				t.Fatal("unexpected provider phase", calls)
			}
			if calls > 0 {
				c, e := ReadRuntime(spec.RuntimeFile)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.Remove(c.Telegram.TokenFile); e != nil {
					t.Fatal(e)
				}
			}
			if err = os.Remove(spec.RuntimeFile); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(spec.BinaryFile); err != nil {
				t.Fatal(err)
			}
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			managedTelegramReceiptForTest(t, m)
			if calls+p.calls != 3 {
				t.Fatal("completed renewal polled again", calls, p.calls)
			}
			if _, err = i.resume(context.Background(), spec.Root); err != nil {
				t.Fatal(err)
			}
		})
	}
}
