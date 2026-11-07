package worker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestBootstrapEnrollmentLEDHandoff(t *testing.T) {
	for _, disabled := range []bool{true, false} {
		for _, boundary := range []string{"intent", "producers", "owner/backup-worker", "complete"} {
			t.Run(boundary+map[bool]string{true: "/disabled", false: "/enabled"}[disabled], func(t *testing.T) {
				ctx := context.Background()
				path, before, output, api := bootstrapLimitsOutputFixture(t, disabled)
				// Exercise both handoffs together without rewriting the original
				// output intent, bootstrap or limits history.
				for range 2 {
					after := before
					after.MaxMessages++
					op, _ := safefs.NewID("n")
					_, err := MigrateBootstrapLimits(ctx, path, output.Identity, before, after, op)
					must(t, err)
					before = after
				}
				original, err := os.ReadFile(filepath.Join(path, ledOutputFile))
				must(t, err)
				states, clears := protocol.Hash(api.states), len(api.cleared)
				after := bootstrapConsumerConfig(before)
				must(t, PreflightBootstrapEnrollment(ctx, path, output.Identity, before, after, output.Operation, disabled))
				op, _ := safefs.NewID("n")
				stop := errors.New("lost enrollment output handoff")
				r, err := enrollBootstrap(ctx, path, output.Identity, after, op, func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				})
				if !errors.Is(err, stop) || r.Version != 2 || r.LEDOutputOperation != output.Operation || r.LEDOutputHash == "" {
					t.Fatal("handoff boundary", r, err)
				}
				if boundary != "complete" {
					if s, e := OpenManagedVerification(path, after, output.Identity); e == nil {
						s.Close()
						t.Fatal("pending enrollment admitted runtime")
					}
				}
				for range 2 {
					again, e := EnrollBootstrap(ctx, path, output.Identity, after, op)
					must(t, e)
					if again != r {
						t.Fatal("receipt changed")
					}
				}
				s, err := OpenManagedVerification(path, after, output.Identity)
				must(t, err)
				must(t, s.RequireLEDOutput(ctx, output.Operation, disabled))
				must(t, s.RequireInactiveProducers())
				var faults int
				must(t, s.db.QueryRow("SELECT count(*) FROM led_states WHERE condition='critical' AND obsolete=0").Scan(&faults))
				if faults == 0 {
					t.Fatal("enrollment erased retained fault")
				}
				must(t, s.Close())
				got, err := os.ReadFile(filepath.Join(path, ledOutputFile))
				must(t, err)
				if string(got) != string(original) || protocol.Hash(api.states) != states || len(api.cleared) != clears {
					t.Fatal("enrollment rewrote or replayed LED output")
				}
				required, err := BootstrapEnrollmentLEDRequired(path)
				must(t, err)
				if !required {
					t.Fatal("handoff downgrade not fenced")
				}
				// Subsequent output operations retain the enrollment's old pin.
				output.Operation, _ = safefs.NewID("n")
				output.Disabled = !disabled
				_, err = ConfigureLEDOutput(ctx, path, after, output, api)
				must(t, err)
				_, err = BootstrapEnrollmentLEDRequired(path)
				must(t, err)
			})
		}
	}
}

func TestBootstrapEnrollmentLEDRefusesMissingOrPendingAuthority(t *testing.T) {
	for _, damage := range []string{"pending", "lost-file", "wrong-manifest", "lost-receipt", "lost-record"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			path, before, output, api := bootstrapLimitsOutputFixture(t, true)
			after := bootstrapConsumerConfig(before)
			op, _ := safefs.NewID("n")
			switch damage {
			case "pending":
				output.Operation, _ = safefs.NewID("n")
				output.Disabled = false
				_, err := configureLEDOutput(ctx, path, before, output, api, func(at string) error {
					if at == "output-intent" {
						return errors.New("interrupt")
					}
					return nil
				})
				if err == nil {
					t.Fatal("missing interrupt")
				}
			case "lost-file":
				must(t, os.Remove(filepath.Join(path, ledOutputFile)))
			case "wrong-manifest":
				output.Operation, _ = safefs.NewID("n")
			case "lost-receipt", "lost-record":
				_, err := EnrollBootstrap(ctx, path, output.Identity, after, op)
				must(t, err)
				if damage == "lost-receipt" {
					must(t, os.Remove(filepath.Join(path, "bootstrap-enrollment.json")))
				} else {
					s, e := OpenManagedVerification(path, after, output.Identity)
					must(t, e)
					_, e = s.db.Exec("DELETE FROM metadata WHERE key=?", ledOutputPrefix+output.Operation)
					must(t, e)
					must(t, s.Close())
				}
				if _, err = BootstrapEnrollmentLEDRequired(path); err == nil {
					t.Fatal("damaged handoff allowed helper downgrade")
				}
			}
			if PreflightBootstrapEnrollment(ctx, path, output.Identity, before, after, output.Operation, output.Disabled) == nil {
				t.Fatal("damaged output passed preflight")
			}
			if damage != "wrong-manifest" {
				if _, err := EnrollBootstrap(ctx, path, output.Identity, after, op); err == nil {
					t.Fatal("damaged enrollment repaired itself")
				}
			}
		})
	}
}

func TestBootstrapEnrollmentLEDCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_ENROLLMENT_LED_CRASH_INPUT")
	if path == "" {
		t.Skip("child only")
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var input bootstrapLimitsCrashInput
	must(t, d.ReadJSON("input.json", &input))
	_, err = enrollBootstrap(context.Background(), input.State, input.Identity, input.After, input.Operation, func(at string) error {
		if at == input.Boundary {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("child survived", err)
}
func TestBootstrapEnrollmentLEDSIGKILL(t *testing.T) {
	testBootstrapEnrollmentSIGKILL(t, false)
}
func TestBootstrapCommandEnrollmentSIGKILL(t *testing.T) {
	testBootstrapEnrollmentSIGKILL(t, true)
}
func testBootstrapEnrollmentSIGKILL(t *testing.T, commands bool) {
	for _, boundary := range []string{"intent", "producers", "owner/backup-worker", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			path, before, output, _ := bootstrapLimitsOutputFixture(t, true)
			op, _ := safefs.NewID("n")
			after := bootstrapConsumerConfig(before)
			if commands {
				after = bootstrapCommandConfig(before)
			}
			dir := t.TempDir()
			d, err := safefs.Open(dir)
			must(t, err)
			defer d.Close()
			must(t, d.WriteJSON("input.json", bootstrapLimitsCrashInput{path, op, boundary, output.Identity, before, after}, false))
			cmd := exec.Command(os.Args[0], "-test.run=^TestBootstrapEnrollmentLEDCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_ENROLLMENT_LED_CRASH_INPUT="+dir)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("wrong child exit", string(out))
			}
			_, err = EnrollBootstrap(context.Background(), path, output.Identity, after, op)
			must(t, err)
			s, err := OpenManagedVerification(path, after, output.Identity)
			must(t, err)
			must(t, s.RequireInactiveProducers())
			must(t, s.RequireLEDOutput(context.Background(), output.Operation, true))
			must(t, s.Close())
		})
	}
}
