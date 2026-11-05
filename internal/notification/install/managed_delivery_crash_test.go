package install

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestManagedDeliverySIGKILL(t *testing.T) {
	for _, op := range []string{"activate-delivery", "deactivate-delivery"} {
		for _, boundary := range []string{"prepared", "unit-published", "manifest-published", "start-returned", "complete"} {
			t.Run(op+"/"+boundary, func(t *testing.T) {
				spec, i, f, _, before, record := deliveryFixture(t)
				qualificationHash := protocol.Hash(record)
				if op == "deactivate-delivery" {
					var err error
					before, err = i.prepareDelivery(context.Background(), "activate-delivery", spec.Root, protocol.Hash(before), qualificationHash, false)
					if err != nil {
						t.Fatal(err)
					}
					qualificationHash = ""
				}
				dir := t.TempDir()
				d, err := safefs.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				if err := d.WriteJSON("input.json", managedCrashInput{Spec: spec, Operation: op, Boundary: boundary, Service: f.state, ManifestHash: protocol.Hash(before), QualificationHash: qualificationHash}, false); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestManagedCrashHelper$", "-test.timeout=60s")
				cmd.Env = append(os.Environ(), "DEPLOY_MANAGED_CRASH_INPUT="+dir)
				out, err := process.LimitedOutput(cmd, 65*time.Second, 16384)
				if err == nil || cmd.ProcessState == nil {
					t.Fatal("missing crash", err, string(out))
				}
				status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatal("wrong exit", string(out))
				}
				if err = d.ReadJSON("manager.json", &f.state); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(spec.BinaryFile); err != nil {
					t.Fatal(err)
				}
				m, err := i.resume(context.Background(), spec.Root)
				if err != nil {
					t.Fatal(err)
				}
				want := "delivery"
				if op == "deactivate-delivery" {
					want = "verification"
				}
				if m.Mode != want {
					t.Fatal(m)
				}
			})
		}
	}
}
