package worker

import (
	"context"
	"fmt"
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

type administrationCrashInput struct {
	Config   Config
	Decision MessageDecision
	Boundary string
	Recovery bool
}

func TestMessageAdministrationCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_ADMIN_CRASH_STATE")
	if path == "" {
		t.Skip("child helper")
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var in administrationCrashInput
	must(t, d.ReadJSON("admin-crash-input.json", &in))
	if in.Recovery {
		seen := 0
		r, e := openRecoveryReconciler(context.Background(), path, in.Config, func(p string) error {
			if p == in.Boundary {
				seen++
				// The first transaction initializes reconciliation; the second
				// preserves administrative audit from the original database.
				if seen == 2 {
					return syscall.Kill(os.Getpid(), syscall.SIGKILL)
				}
			}
			return nil
		})
		if e == nil {
			r.Close()
		}
		t.Fatal("recovery crash boundary not reached", e)
	}
	s, err := OpenManagedVerification(path, in.Config, in.Decision.Review.Identity)
	must(t, err)
	defer s.Close()
	a := messageAdministrator{s: s, clock: process.HostClock, fault: func(p string) error {
		if p == in.Boundary {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	_, err = a.decide(context.Background(), in.Decision)
	t.Fatal("crash boundary not reached", err)
}

func TestMessageAdministrationSIGKILL(t *testing.T) {
	for _, action := range []string{"retry", "cancel"} {
		for _, boundary := range []string{"administration_intent", "administration_transaction", "administration_commit", "administration_result"} {
			t.Run(action+"/"+boundary, func(t *testing.T) {
				s, now, path := fixture(t)
				*now = time.Now().UTC()
				m := report(*now, "crash-event", 1, 1, 1)
				ctx := context.Background()
				must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
				_, err := s.Enqueue(ctx, m)
				must(t, err)
				j, granted, err := s.Grant(ctx, producer, m.ID)
				must(t, err)
				if !granted {
					t.Fatal("no attempt")
				}
				must(t, s.Finish(ctx, j, Result{Ambiguous: true}))
				v, err := s.ReviewMessage(ctx, producer, m.ID)
				must(t, err)
				decision := MessageDecision{Version: 1, Review: v, Action: action, Reason: "Reviewed crash test", AcceptDuplicateRisk: action == "retry"}
				c := s.config
				must(t, s.dir.WriteJSON("admin-crash-input.json", administrationCrashInput{Config: c, Decision: decision, Boundary: boundary}, false))
				must(t, s.Close())
				executable, err := os.Executable()
				must(t, err)
				cmd := exec.Command(executable, "-test.run=^TestMessageAdministrationCrashHelper$", "-test.timeout=15s")
				cmd.Env = append(os.Environ(), "DEPLOY_ADMIN_CRASH_STATE="+path)
				_, err = process.LimitedOutput(cmd, 20*time.Second, 8192)
				if err == nil || cmd.ProcessState == nil {
					t.Fatal("missing crash", err)
				}
				status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatal(fmt.Sprint(cmd.ProcessState), err)
				}
				s, err = OpenManagedVerification(path, c, v.Identity)
				must(t, err)
				defer s.Close()
				if boundary == "administration_intent" || boundary == "administration_transaction" {
					job, err := s.Get(ctx, producer, m.ID)
					must(t, err)
					if job.State != "ambiguous" {
						t.Fatal("startup applied pending intent", job)
					}
				}
				record, err := s.DecideMessage(ctx, decision)
				must(t, err)
				replayed, err := s.DecideMessage(ctx, decision)
				must(t, err)
				if protocol.Hash(record) != protocol.Hash(replayed) {
					t.Fatal("replay changed result")
				}
				var n int
				must(t, s.db.QueryRow("SELECT count(*) FROM receipts WHERE producer=? AND json_extract(body,'$.state')=?", producer.ID, record.Receipt.State).Scan(&n))
				if n != 1 {
					t.Fatal("duplicate administrative receipt", n)
				}
				if _, err = os.Stat(filepath.Join(path, messageResultFile(v.ID))); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestMessageAdministrationRecoverySIGKILL(t *testing.T) {
	for _, boundary := range []string{"reconciliation_transaction", "reconciliation_commit"} {
		t.Run(boundary, func(t *testing.T) {
			s, _, path, d := administrationFixture(t)
			ctx := context.Background()
			artifact := filepath.Join(t.TempDir(), "snapshot")
			_, err := s.Snapshot(ctx, artifact)
			must(t, err)
			record, err := s.DecideMessage(ctx, d)
			must(t, err)
			c := s.config
			must(t, s.dir.WriteJSON("admin-crash-input.json", administrationCrashInput{Config: c, Decision: d, Boundary: boundary, Recovery: true}, false))
			must(t, s.Close())
			journal, err := prepareSnapshotRecovery(ctx, path, artifact, c, nil)
			must(t, err)
			executable, err := os.Executable()
			must(t, err)
			cmd := exec.Command(executable, "-test.run=^TestMessageAdministrationCrashHelper$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "DEPLOY_ADMIN_CRASH_STATE="+path)
			_, err = process.LimitedOutput(cmd, 20*time.Second, 8192)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err)
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal(cmd.ProcessState, err)
			}
			r, err := openRecoveryReconciler(ctx, path, c, nil)
			must(t, err)
			defer r.Close()
			must(t, verifyAdministrativeHistory(ctx, r.db, journal.After))
			a := messageAdministrator{s: &Store{dir: r.dir, db: r.db, identity: journal.After}}
			got, err := a.status(ctx, d.Review.ID)
			must(t, err)
			if protocol.Hash(got) != protocol.Hash(record) {
				t.Fatal("crash changed historical decision")
			}
			var receipts int
			must(t, r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts))
			if receipts != 0 {
				t.Fatal("audit restoration issued new receipt authority")
			}
			if live, e := Open(path, c, false); e == nil {
				live.Close()
				t.Fatal("audit recovery enabled runtime")
			}
		})
	}
}
