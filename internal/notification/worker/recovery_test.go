package worker

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func recoveryCandidate(t *testing.T, path string, j snapshotRecovery) *sql.DB {
	t.Helper()
	dir, err := safefs.Open(filepath.Join(path, "recovery", j.Operation))
	must(t, err)
	t.Cleanup(func() { dir.Close() })
	db, err := snapshotDB(dir, "candidate.db", true)
	must(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSnapshotRecoveryDoesNotResendPostSnapshotAcceptance(t *testing.T) {
	s, now, path := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	m := report(*now, "was-queued", 1, 1, 1)
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	manifest, err := s.Snapshot(ctx, artifact)
	must(t, err)
	job, granted, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !granted {
		t.Fatal("missing fixture grant")
	}
	must(t, s.Finish(ctx, job, Result{Accepted: true}))
	if _, err = prepareSnapshotRecovery(ctx, path, artifact, s.config, nil); err == nil {
		t.Fatal("live worker lease bypassed")
	}
	c := s.config
	old := s.Identity()
	must(t, s.Close())
	j, err := prepareSnapshotRecovery(ctx, path, artifact, c, nil)
	must(t, err)
	if j.Phase != "quarantined" || j.Before != old || j.After.Worker != old.Worker || j.After.Recovery == old.Recovery {
		t.Fatal(j)
	}
	db := recoveryCandidate(t, path, j)
	var state, original string
	must(t, db.QueryRow("SELECT state FROM messages WHERE id=?", m.ID).Scan(&state))
	must(t, db.QueryRow("SELECT state FROM recovery_messages WHERE id=?", m.ID).Scan(&original))
	if state != "ambiguous" || original != "queued" {
		t.Fatal("restored queued copy was trusted", state, original)
	}
	if r, err := Open(path, c, false); err == nil {
		r.Close()
		t.Fatal("quarantined restore activated")
	}
	resumed, err := resumeSnapshotRecovery(ctx, path, c, nil)
	must(t, err)
	if resumed != j {
		t.Fatal("recovery retry rotated identity")
	}
	verified, err := VerifySnapshot(ctx, artifact, c)
	must(t, err)
	if verified != manifest {
		t.Fatal("source artifact mutated")
	}
	// Independent canonical ownership and the original live database survive.
	dir, err := safefs.Open(path)
	must(t, err)
	defer dir.Close()
	var owner ownership
	must(t, dir.ReadJSON(ownershipName(producer)+".json", &owner))
	if owner.Identity != old || owner.Generations[m.Generation] != "active" {
		t.Fatal("ownership history rewritten")
	}
	live, err := snapshotDB(dir, "state.db", true)
	must(t, err)
	defer live.Close()
	must(t, live.QueryRow("SELECT state FROM messages WHERE id=?", m.ID).Scan(&state))
	if state != "provider_accepted" {
		t.Fatal("live acceptance evidence lost")
	}
}

func TestSnapshotRecoveryQuarantinesEveryNonterminalAndOldReceiptStream(t *testing.T) {
	s, now, path := fixture(t)
	ctx := context.Background()
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	states := []string{"queued", "waiting_status", "sending", "retry_wait", "blocked", "fenced", "ambiguous", "provider_accepted", "expired", "cancelled", "superseded"}
	for i, state := range states {
		m := report(*now, state, uint64(i+1), 1, 1)
		m.ReportKey = "daily-" + state
		_, err := s.Enqueue(ctx, m)
		must(t, err)
		_, err = s.db.Exec("UPDATE messages SET state=? WHERE id=?", state, state)
		must(t, err)
	}
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		return s.receipt(tx, producer.ID, protocol.Receipt{EventID: "provider_accepted", Sequence: 8, State: "provider_accepted", At: *now})
	}))
	_, err := s.db.Exec("UPDATE producers SET acked=1,listed=1")
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	c := s.config
	must(t, s.Close())
	j, err := prepareSnapshotRecovery(ctx, path, artifact, c, nil)
	must(t, err)
	db := recoveryCandidate(t, path, j)
	for _, before := range states {
		var after string
		must(t, db.QueryRow("SELECT state FROM messages WHERE id=?", before).Scan(&after))
		want := "ambiguous"
		if terminal(before) {
			want = before
		}
		if after != want {
			t.Fatalf("%s became %s", before, after)
		}
	}
	var n, acked, listed int
	must(t, db.QueryRow("SELECT count(*) FROM receipts").Scan(&n))
	if n != 0 {
		t.Fatal("old receipts relabeled")
	}
	must(t, db.QueryRow("SELECT count(*) FROM recovery_receipts").Scan(&n))
	if n != 1 {
		t.Fatal("receipt evidence lost")
	}
	must(t, db.QueryRow("SELECT acked,listed FROM producers").Scan(&acked, &listed))
	if acked != 0 || listed != 0 {
		t.Fatal("old cursor retained")
	}
	must(t, db.QueryRow("SELECT acked,listed FROM recovery_producers").Scan(&acked, &listed))
	if acked != 1 || listed != 1 {
		t.Fatal("old checkpoint lost")
	}
}

func TestSnapshotRecoveryFencesCommandsInboxAndLEDClears(t *testing.T) {
	s, _, path, in := commandFixture(t)
	ctx := context.Background()
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	_, err = s.db.Exec("UPDATE commands SET state='prepared',grant='old-grant',token_hash='old-token',confirmed=1,preparation='old-preparation' WHERE id=?", v.Command.ID)
	must(t, err)
	in.NativeID = "update-2"
	done, err := s.AdmitCommand(ctx, in)
	must(t, err)
	_, err = s.db.Exec("UPDATE commands SET state='succeeded' WHERE id=?", done.Command.ID)
	must(t, err)
	_, err = s.db.Exec("INSERT INTO provider_inbox VALUES('account',1,'hash','{}','pending',0);INSERT INTO led_states(id,condition,obsolete) VALUES('owned-fault','failure',1)")
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	c := s.config
	must(t, s.Close())
	j, err := prepareSnapshotRecovery(ctx, path, artifact, c, nil)
	must(t, err)
	db := recoveryCandidate(t, path, j)
	var state, grant, prepared string
	var token sql.NullString
	var confirmed int
	must(t, db.QueryRow("SELECT state,grant,preparation,token_hash,confirmed FROM commands WHERE id=?", v.Command.ID).Scan(&state, &grant, &prepared, &token, &confirmed))
	if state != "outcome_unknown" || grant != "" || prepared != "" || token.Valid || confirmed != 0 {
		t.Fatal("restored command retained authority")
	}
	must(t, db.QueryRow("SELECT state FROM commands WHERE id=?", done.Command.ID).Scan(&state))
	if state != "succeeded" {
		t.Fatal("terminal result changed")
	}
	must(t, db.QueryRow("SELECT state FROM provider_inbox").Scan(&state))
	if state != "recovery_quarantined" {
		t.Fatal("inbox revived")
	}
	var obsolete int
	must(t, db.QueryRow("SELECT obsolete FROM led_states WHERE id='owned-fault'").Scan(&obsolete))
	if obsolete != 1 {
		t.Fatal("LED evidence discarded")
	}
	if r, err := Open(path, c, false); err == nil {
		r.Close()
		t.Fatal("LED/command loops could start")
	}
}

func TestSnapshotRecoveryCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_SNAPSHOT_RECOVERY_STATE")
	if path == "" {
		t.Skip("subprocess fixture")
	}
	boundary := os.Getenv("DEPLOY_SNAPSHOT_RECOVERY_BOUNDARY")
	_, err := prepareSnapshotRecovery(context.Background(), path, os.Getenv("DEPLOY_SNAPSHOT_RECOVERY_SOURCE"), configuration(), func(phase string) error {
		if phase == boundary {
			fmt.Println("READY")
			for {
				time.Sleep(time.Second)
			}
		}
		return nil
	})
	must(t, err)
	t.Fatal("boundary not reached")
}

func TestSnapshotRecoverySIGKILL(t *testing.T) {
	for _, boundary := range []string{"prepared", "identity", "epoch_published", "binding", "classified", "committed", "quarantined"} {
		t.Run(boundary, func(t *testing.T) {
			s, now, path := fixture(t)
			ctx := context.Background()
			must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
			_, err := s.Enqueue(ctx, report(*now, "pending", 1, 1, 1))
			must(t, err)
			artifact := filepath.Join(t.TempDir(), "snapshot")
			_, err = s.Snapshot(ctx, artifact)
			must(t, err)
			must(t, s.Close())
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSnapshotRecoveryCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_SNAPSHOT_RECOVERY_STATE="+path, "DEPLOY_SNAPSHOT_RECOVERY_SOURCE="+artifact, "DEPLOY_SNAPSHOT_RECOVERY_BOUNDARY="+boundary)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			must(t, cmd.Start())
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				t.Fatalf("boundary not reached: %s %s", scanner.Text(), stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			if r, err := Open(path, configuration(), false); err == nil {
				r.Close()
				t.Fatal("interrupted restore admitted dispatcher")
			}
			j, err := resumeSnapshotRecovery(context.Background(), path, configuration(), nil)
			must(t, err)
			if j.Phase != "quarantined" {
				t.Fatal(j)
			}
			db := recoveryCandidate(t, path, j)
			var state string
			must(t, db.QueryRow("SELECT state FROM messages WHERE id='pending'").Scan(&state))
			if state != "ambiguous" {
				t.Fatal("recovery lost ambiguity fence")
			}
			again, err := resumeSnapshotRecovery(context.Background(), path, configuration(), nil)
			must(t, err)
			if again != j {
				t.Fatal("resume changed identity")
			}
		})
	}
}

func TestSnapshotRecoveryRejectsDifferentInstallationBeforeFencing(t *testing.T) {
	s, _, path := fixture(t)
	other, _, _ := fixture(t)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err := other.Snapshot(context.Background(), artifact)
	must(t, err)
	must(t, s.Close())
	if _, err = prepareSnapshotRecovery(context.Background(), path, artifact, configuration(), nil); err == nil {
		t.Fatal("foreign snapshot accepted")
	}
	if _, err = os.Stat(filepath.Join(path, "recovery.json")); !os.IsNotExist(err) {
		t.Fatal("rejected snapshot fenced installation")
	}
	r, err := Open(path, configuration(), false)
	must(t, err)
	r.Close()
}

func TestSnapshotRecoveryRefusesCorruptCandidateAndReusesOwnedSource(t *testing.T) {
	for _, damage := range []string{"none", "source", "candidate", "identity", "schema"} {
		t.Run(damage, func(t *testing.T) {
			s, _, path := fixture(t)
			artifact := filepath.Join(t.TempDir(), "snapshot")
			_, err := s.Snapshot(context.Background(), artifact)
			must(t, err)
			must(t, s.Close())
			j, err := prepareSnapshotRecovery(context.Background(), path, artifact, configuration(), func(phase string) error {
				if phase == "committed" {
					return errors.New("lost acknowledgement")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault not reached")
			}
			// Resuming depends on the copied source, not an operator's original path.
			must(t, os.RemoveAll(artifact))
			stage := filepath.Join(path, "recovery", j.Operation)
			switch damage {
			case "source":
				must(t, os.WriteFile(filepath.Join(stage, "source.db"), []byte("corrupt"), 0600))
			case "candidate":
				must(t, os.WriteFile(filepath.Join(stage, "candidate.db"), []byte("corrupt"), 0600))
			case "identity", "schema":
				dir, err := safefs.Open(stage)
				must(t, err)
				defer dir.Close()
				db, err := snapshotDB(dir, "candidate.db", false)
				must(t, err)
				if damage == "identity" {
					_, err = db.Exec("UPDATE metadata SET value='{}' WHERE key='identity'")
				} else {
					_, err = db.Exec("PRAGMA user_version=5")
				}
				must(t, err)
				must(t, db.Close())
			}
			got, err := resumeSnapshotRecovery(context.Background(), path, configuration(), nil)
			if damage == "none" {
				must(t, err)
				if got.After != j.After {
					t.Fatal("resume rotated epoch")
				}
			} else if err == nil {
				t.Fatal("corruption accepted")
			}
			if r, err := Open(path, configuration(), false); err == nil {
				r.Close()
				t.Fatal("recovery fence cleared")
			}
		})
	}
}
