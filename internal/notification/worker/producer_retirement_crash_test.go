package worker

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestProducerRetirementCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_RETIREMENT_TEST_PATH")
	if path == "" {
		t.Skip("subprocess only")
	}
	s, err := Open(path, configuration(), false)
	must(t, err)
	defer s.Close()
	s.ownershipFault = func(at string) error {
		if at == os.Getenv("DEPLOY_RETIREMENT_TEST_BOUNDARY") {
			if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			select {}
		}
		return nil
	}
	_, err = s.RetireProducer(context.Background(), producer, "operator", "Decommissioned", true)
	t.Fatal("crash boundary was not reached", err)
}

func TestProducerRetirementSIGKILL(t *testing.T) {
	for _, point := range []string{"intent", "history", "owner"} {
		t.Run(point, func(t *testing.T) {
			s, _, path := retirementFixture(t)
			must(t, s.Close())
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProducerRetirementCrashHelper$")
			cmd.Env = append(os.Environ(), "DEPLOY_RETIREMENT_TEST_PATH="+path, "DEPLOY_RETIREMENT_TEST_BOUNDARY="+point)
			raw, err := cmd.CombinedOutput()
			exit, ok := err.(*exec.ExitError)
			if ctx.Err() != nil || !ok || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("child did not SIGKILL", err, string(raw))
			}
			r, err := Open(path, configuration(), false)
			must(t, err)
			defer r.Close()
			owner := readOwner(t, r)
			if owner.Retirement == nil {
				t.Fatal("retirement disappeared")
			}
			v, err := r.RetireProducer(context.Background(), producer, "operator", "Decommissioned", true)
			must(t, err)
			if *v.Retirement != *owner.Retirement {
				t.Fatal("resume replaced operation")
			}
			if err = r.Stage(producer, "new"); err == nil {
				t.Fatal("crash resurrected producer")
			}
		})
	}
}
