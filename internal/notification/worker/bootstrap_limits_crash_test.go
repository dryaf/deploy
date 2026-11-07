package worker

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

type bootstrapLimitsCrashInput struct {
	State, Operation, Boundary string
	Identity                   protocol.Identity
	Before, After              Config
}

func TestBootstrapLimitsCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_BOOTSTRAP_LIMITS_CRASH_INPUT")
	if path == "" {
		t.Skip("subprocess only")
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var input bootstrapLimitsCrashInput
	must(t, d.ReadJSON("input.json", &input))
	fault := func(at string) error {
		if at == input.Boundary {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	if os.Getenv("DEPLOY_BOOTSTRAP_POLICY_KIND") == "enrolled-command-activity" {
		_, err = migrateEnrolledPolicy(context.Background(), input.State, input.Identity, input.Before, input.After, input.Operation, 13, fault)
	} else if os.Getenv("DEPLOY_BOOTSTRAP_POLICY_KIND") == "enrolled-command-policy" {
		_, err = migrateEnrolledPolicy(context.Background(), input.State, input.Identity, input.Before, input.After, input.Operation, 12, fault)
	} else if os.Getenv("DEPLOY_BOOTSTRAP_POLICY_KIND") == "enrolled-commands" {
		_, err = migrateEnrolledPolicy(context.Background(), input.State, input.Identity, input.Before, input.After, input.Operation, 11, fault)
	} else if os.Getenv("DEPLOY_BOOTSTRAP_POLICY_KIND") == "enrolled-providers" {
		_, err = migrateEnrolledPolicy(context.Background(), input.State, input.Identity, input.Before, input.After, input.Operation, 5, fault)
	} else if os.Getenv("DEPLOY_BOOTSTRAP_POLICY_KIND") == "enrolled-producer" {
		_, err = migrateEnrolledPolicy(context.Background(), input.State, input.Identity, input.Before, input.After, input.Operation, 4, fault)
	} else if os.Getenv("DEPLOY_BOOTSTRAP_POLICY_KIND") == "enrolled-routes" {
		_, err = migrateEnrolledPolicy(context.Background(), input.State, input.Identity, input.Before, input.After, input.Operation, 3, fault)
	} else if os.Getenv("DEPLOY_BOOTSTRAP_POLICY_KIND") == "enrolled-freshness" {
		_, err = migrateEnrolledPolicy(context.Background(), input.State, input.Identity, input.Before, input.After, input.Operation, 2, fault)
	} else if os.Getenv("DEPLOY_BOOTSTRAP_POLICY_KIND") == "enrolled" {
		_, err = migrateEnrolledLimits(context.Background(), input.State, input.Identity, input.Before, input.After, input.Operation, fault)
	} else {
		_, err = migrateBootstrapPolicy(context.Background(), input.State, input.Identity, input.Before, input.After, input.Operation, os.Getenv("DEPLOY_BOOTSTRAP_POLICY_KIND"), fault)
	}
	t.Fatal("limits crash helper survived", err)
}
func TestBootstrapLimitsSIGKILLPreservesReservationAndPolicy(t *testing.T) {
	testBootstrapPolicySIGKILL(t, "")
}
func TestBootstrapRoutesSIGKILLPreservesReservationAndPolicy(t *testing.T) {
	testBootstrapPolicySIGKILL(t, "routes")
}
func testBootstrapPolicySIGKILL(t *testing.T, kind string) {
	for _, outputHistory := range []bool{false, true} {
		for _, boundary := range []string{"intent", "committed", "complete"} {
			t.Run(boundary+map[bool]string{false: "/plain", true: "/led"}[outputHistory], func(t *testing.T) {
				path, op := bootstrapTarget(t)
				before := bootstrapConfig()
				b, err := BootstrapGateway(context.Background(), path, op, before)
				must(t, err)
				var output LEDOutputRequest
				if outputHistory {
					path, before, output, _ = bootstrapLimitsOutputFixture(t, true)
					b.Identity = output.Identity
				}
				after := before
				if kind == "routes" {
					after.Routes = make(map[string]Route, len(before.Routes)+1)
					for name, route := range before.Routes {
						after.Routes[name] = route
						after.Routes["audit"] = route
					}
				} else {
					after.MaxMessages++
				}
				change, err := safefs.NewID("n")
				must(t, err)
				input := bootstrapLimitsCrashInput{path, change, boundary, b.Identity, before, after}
				dir := t.TempDir()
				d, err := safefs.Open(dir)
				must(t, err)
				defer d.Close()
				must(t, d.WriteJSON("input.json", input, false))
				cmd := exec.Command(os.Args[0], "-test.run=^TestBootstrapLimitsCrashHelper$", "-test.timeout=30s")
				cmd.Env = append(os.Environ(), "DEPLOY_BOOTSTRAP_LIMITS_CRASH_INPUT="+dir, "DEPLOY_BOOTSTRAP_POLICY_KIND="+kind)
				out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
				if err == nil || cmd.ProcessState == nil {
					t.Fatal("missing crash", err, string(out))
				}
				status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatal("wrong exit", string(out))
				}
				r, err := migrateBootstrapPolicy(context.Background(), path, b.Identity, before, after, change, kind, nil)
				must(t, err)
				if r.Identity != b.Identity {
					t.Fatal("identity changed")
				}
				s, err := OpenBootstrapVerification(path, after, b.Identity, "")
				must(t, err)
				if outputHistory {
					must(t, s.RequireLEDOutput(context.Background(), output.Operation, true))
				}
				must(t, s.Close())
				if s, err := OpenBootstrapVerification(path, before, b.Identity, ""); err == nil {
					s.Close()
					t.Fatal("old policy accepted")
				}
			})
		}
	}
}
