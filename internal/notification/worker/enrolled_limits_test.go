package worker

import (
	"context"
	"errors"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func enrolledLimitsFixture(t *testing.T) (string, protocol.Identity, Config) {
	t.Helper()
	ctx := context.Background()
	path, op := bootstrapTarget(t)
	b, err := BootstrapGateway(ctx, path, op, bootstrapConfig())
	must(t, err)
	before := bootstrapConsumerConfig(b.Config)
	change, _ := safefs.NewID("n")
	_, err = EnrollBootstrap(ctx, path, b.Identity, before, change)
	must(t, err)
	return path, b.Identity, before
}
func TestEnrolledLimitsResumeAndHistory(t *testing.T) {
	for _, boundary := range []string{"intent", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			path, id, before := enrolledLimitsFixture(t)
			original, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
			must(t, err)
			after := before
			after.MaxMessages += 10
			after.Producers = append([]Enrollment(nil), before.Producers...)
			after.Producers[0].MaxMessages++
			op, _ := safefs.NewID("n")
			stop := errors.New("lost reply")
			receipt, err := migrateEnrolledLimits(ctx, path, id, before, after, op, func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if boundary != "complete" {
				for _, c := range []Config{before, after} {
					if s, e := OpenManagedVerification(path, c, id); e == nil {
						s.Close()
						t.Fatal("pending transition admitted runtime")
					}
				}
			}
			wrong := after
			wrong.MaxMessages++
			if _, e := MigrateEnrolledLimits(ctx, path, id, before, wrong, op); e == nil {
				t.Fatal("changed resume accepted")
			}
			got, err := MigrateEnrolledLimits(ctx, path, id, before, after, op)
			must(t, err)
			if protocol.Hash(got) != protocol.Hash(receipt) {
				t.Fatal("changed receipt")
			}
			if s, e := OpenManagedVerification(path, before, id); e == nil {
				s.Close()
				t.Fatal("old budget admitted")
			}
			s, err := OpenManagedVerification(path, after, id)
			must(t, err)
			must(t, s.Close())
			next := after
			next.ReceiptAuditDays = 180
			op2, _ := safefs.NewID("n")
			_, err = MigrateEnrolledLimits(ctx, path, id, after, next, op2)
			must(t, err)
			retained, err := os.ReadFile(filepath.Join(path, "bootstrap-enrollment.json"))
			must(t, err)
			if string(retained) != string(original) {
				t.Fatal("enrollment rewritten")
			}
			must(t, os.Remove(filepath.Join(path, enrolledLimitsFile)))
			if s, e := OpenManagedVerification(path, next, id); e == nil {
				s.Close()
				t.Fatal("missing history admitted")
			}
		})
	}
}
func TestEnrolledLimitsRefusesUnrelatedAndInsufficientPolicy(t *testing.T) {
	for _, kind := range []string{"route", "freshness", "database", "reserve", "identity"} {
		t.Run(kind, func(t *testing.T) {
			path, id, before := enrolledLimitsFixture(t)
			after := before
			after.MaxMessages++
			after.Producers = append([]Enrollment(nil), before.Producers...)
			switch kind {
			case "route":
				after.Producers[0].Routes = []string{"different"}
			case "freshness":
				after.Producers[0].MaxSilenceSeconds++
			case "database":
				after.MaxDatabaseBytes = 1
			case "reserve":
				after.MinFreeBytes = 1 << 60
			case "identity":
				id.Worker = "different"
			}
			op, _ := safefs.NewID("n")
			if _, err := MigrateEnrolledLimits(context.Background(), path, id, before, after, op); err == nil {
				t.Fatal("invalid change admitted")
			}
			if _, err := os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
				t.Fatal("published refused intent", err)
			}
		})
	}
}

func TestEnrolledLimitsKeepsQueuedWorkAndRejectsSmallerBudgets(t *testing.T) {
	ctx := context.Background()
	path, id, before := enrolledLimitsFixture(t)
	s, err := OpenManagedVerification(path, before, id)
	must(t, err)
	// Retained queue rows are synthetic here. Capacity review must count pending
	// bodies and per-producer quotas without rewriting jobs or receipts.
	for i := 1; i <= 3; i++ {
		_, err = s.db.Exec("INSERT INTO messages(producer,id,seq,hash,body,generation,route_revision,state) VALUES(?,?,?,?,?,?,?,?)", before.Producers[0].Producer.ID, fmt.Sprintf("pending-%d", i), i, protocol.Hash(i), strings.Repeat("x", 12000), "staged-generation", "alerts", "waiting_status")
		must(t, err)
	}
	must(t, s.Close())
	for _, kind := range []string{"global", "producer", "payload"} {
		after := before
		after.Producers = append([]Enrollment(nil), before.Producers...)
		switch kind {
		case "global":
			after.MaxMessages = 2
		case "producer":
			after.Producers[0].MaxMessages = 2
		case "payload":
			after.MaxPayloadBytes = 32768
		}
		op, _ := safefs.NewID("n")
		if err = PreflightEnrolledLimits(ctx, path, id, before, after); err == nil {
			t.Fatal("review accepted undersized", kind)
		}
		if _, err = MigrateEnrolledLimits(ctx, path, id, before, after, op); err == nil {
			t.Fatal("migration accepted undersized", kind)
		}
		if _, err = os.Stat(filepath.Join(path, enrolledLimitsFile)); !os.IsNotExist(err) {
			t.Fatal("refusal published intent", kind, err)
		}
	}
	after := before
	after.MaxMessages = 3
	after.Producers = append([]Enrollment(nil), before.Producers...)
	after.Producers[0].MaxMessages = 3
	op, _ := safefs.NewID("n")
	_, err = MigrateEnrolledLimits(ctx, path, id, before, after, op)
	must(t, err)
	s, err = OpenManagedVerification(path, after, id)
	must(t, err)
	defer s.Close()
	var count, size int
	must(t, s.db.QueryRow("SELECT count(*),sum(length(body)) FROM messages WHERE state='waiting_status'").Scan(&count, &size))
	if count != 3 || size != 36000 {
		t.Fatal("queued work changed", count, size)
	}
}

func TestEnrolledLimitsSIGKILLPreservesEnrollment(t *testing.T) {
	testEnrolledPolicySIGKILL(t, "enrolled")
}
func TestEnrolledProvidersSIGKILLPreservesEnrollment(t *testing.T) {
	testEnrolledPolicySIGKILL(t, "enrolled-providers")
}
func TestEnrolledRoutesSIGKILLPreservesEnrollment(t *testing.T) {
	testEnrolledPolicySIGKILL(t, "enrolled-routes")
}
func TestEnrolledFreshnessSIGKILLPreservesEnrollment(t *testing.T) {
	testEnrolledPolicySIGKILL(t, "enrolled-freshness")
}
func TestEnrolledProducerPolicySIGKILLPreservesEnrollment(t *testing.T) {
	testEnrolledPolicySIGKILL(t, "enrolled-producer")
}
func TestEnrolledCommandsSIGKILLPreservesEnrollment(t *testing.T) {
	testEnrolledPolicySIGKILL(t, "enrolled-commands")
}
func TestEnrolledCommandActivitySIGKILL(t *testing.T) {
	testEnrolledPolicySIGKILL(t, "enrolled-command-activity")
}
func TestEnrolledCommandPolicySIGKILL(t *testing.T) {
	testEnrolledPolicySIGKILL(t, "enrolled-command-policy")
}
func testEnrolledPolicySIGKILL(t *testing.T, kind string) {
	for _, boundary := range []string{"intent", "committed", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			path, id, before := enrolledLimitsFixture(t)
			after := before
			switch kind {
			case "enrolled-command-policy", "enrolled-command-activity":
				initial := before
				before.Interaction = bootstrapCommandConfig(before).Interaction
				op, _ := safefs.NewID("n")
				_, err := MigrateEnrolledCommands(context.Background(), path, id, initial, before, op)
				must(t, err)
				after = revisedVerificationCommands(t, before)
				if kind == "enrolled-command-activity" {
					after = before
					after.Interaction = nil
				}
			case "enrolled-commands":
				after.Interaction = bootstrapCommandConfig(before).Interaction
			case "enrolled-providers": // Same-account credential rotation can leave worker policy unchanged.
			case "enrolled-routes":
				after.Routes = map[string]Route{"alerts": {Revision: "next", Primary: "other"}}
			case "enrolled-freshness":
				after.Producers = append([]Enrollment(nil), before.Producers...)
				after.Producers[0].MaxSilenceSeconds++
			case "enrolled-producer":
				after.Producers = append([]Enrollment(nil), before.Producers...)
				after.Producers[0].Required = !before.Producers[0].Required
			default:
				after.MaxMessages++
			}
			op, _ := safefs.NewID("n")
			dir := t.TempDir()
			d, err := safefs.Open(dir)
			must(t, err)
			defer d.Close()
			must(t, d.WriteJSON("input.json", bootstrapLimitsCrashInput{path, op, boundary, id, before, after}, false))
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
			migrate := MigrateEnrolledLimits
			switch kind {
			case "enrolled-command-activity":
				migrate = MigrateEnrolledCommandActivity
			case "enrolled-command-policy":
				migrate = MigrateEnrolledCommandPolicy
			case "enrolled-commands":
				migrate = MigrateEnrolledCommands
			case "enrolled-providers":
				migrate = MigrateEnrolledProviders
			case "enrolled-routes":
				migrate = MigrateEnrolledRoutes
			case "enrolled-freshness":
				migrate = MigrateEnrolledFreshness
			case "enrolled-producer":
				migrate = MigrateEnrolledProducerPolicy
			}
			_, err = migrate(context.Background(), path, id, before, after, op)
			must(t, err)
			s, err := OpenManagedVerification(path, after, id)
			must(t, err)
			must(t, s.Close())
		})
	}
}

func TestEnrolledLimitsRetainsConfiguredCommandsWithoutAdmission(t *testing.T) {
	ctx := context.Background()
	path, op := bootstrapTarget(t)
	b, err := BootstrapGateway(ctx, path, op, bootstrapConfig())
	must(t, err)
	before := bootstrapCommandConfig(b.Config)
	enroll, _ := safefs.NewID("n")
	_, err = EnrollBootstrap(ctx, path, b.Identity, before, enroll)
	must(t, err)
	after := before
	after.MaxMessages++
	change, _ := safefs.NewID("n")
	_, err = MigrateEnrolledLimits(ctx, path, b.Identity, before, after, change)
	must(t, err)
	s, err := OpenManagedVerification(path, after, b.Identity)
	must(t, err)
	defer s.Close()
	var aliases int
	must(t, s.db.QueryRow("SELECT count(*) FROM command_aliases").Scan(&aliases))
	if aliases != 1 || s.commandIntakeEnabled() || s.commandRepliesEnabled() {
		t.Fatal("limit update changed command admission", aliases)
	}
	wrong := bootstrapCommandConfig(b.Config)
	wrong.MaxMessages = after.MaxMessages
	wrong.Interaction.Workers[0].Alias = "changed"
	if err = ValidateEnrolledLimitsChange(before, wrong); err == nil {
		t.Fatal("alias changed under budget update")
	}
}
