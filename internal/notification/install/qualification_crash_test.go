package install

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
)

func TestQualificationCrashHelper(t *testing.T) {
	root := os.Getenv("DEPLOY_QUALIFICATION_CRASH_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	d, err := privateManagedDir(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lease, err := d.Lock("operation.lock", true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var report QualificationReport
	if err = d.ReadJSON("test-report.json", &report); err != nil {
		t.Fatal(err)
	}
	_, err = appendQualification(d, report, time.Now(), func(point string) error {
		if point == os.Getenv("DEPLOY_QUALIFICATION_CRASH_POINT") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("did not reach SIGKILL boundary", err)
}

func TestQualificationSIGKILL(t *testing.T) {
	for _, point := range []string{"qualification_record", "qualification_head"} {
		t.Run(point, func(t *testing.T) {
			spec, _, _, report := qualificationFixture(t)
			qualificationPass(&report)
			d, err := privateManagedDir(spec.Root, true)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err = d.WriteJSON("test-report.json", report, false); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(executable, "-test.run=^TestQualificationCrashHelper$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "DEPLOY_QUALIFICATION_CRASH_ROOT="+spec.Root, "DEPLOY_QUALIFICATION_CRASH_POINT="+point)
			output, err := process.LimitedOutput(cmd, 20*time.Second, 8192)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("child did not crash", err, string(output))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong exit", cmd.ProcessState, string(output))
			}
			h, err := readQualificationHistory(d, report.Binding.Manifest.Installation)
			if err != nil || len(h.Records) != 1 || h.Pending != (point == "qualification_record") {
				t.Fatal(h, err)
			}
			before := protocol.Hash(h.Records[0])
			view, err := qualifyFixture(t, spec.Root, report)
			if err != nil || view.Pending || !view.EvidenceComplete || view.Qualified || view.ActivationAllowed || protocol.Hash(view.Record) != before {
				t.Fatal(view, err)
			}
		})
	}
}
