package relay

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestBaselineRestoreSIGKILL(t *testing.T) {
	for _, stage := range []string{"restore_prepared", "restore_identity", "restore_transaction", "restore_commit", "restore_quarantined"} {
		t.Run(stage, func(t *testing.T) {
			path, artifact, m, _ := baselineSnapshotFixture(t, true, 2, true)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRelayRestoreCrashChild$")
			cmd.Env = append(os.Environ(), "RELAY_RESTORE_CRASH_PATH="+path, "RELAY_RESTORE_CRASH_SNAPSHOT="+artifact, "RELAY_RESTORE_CRASH_STAGE="+stage)
			pipe, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			scan := bufio.NewScanner(pipe)
			if !scan.Scan() || scan.Text() != "READY" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("child failed", stderr.String())
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			if live, err := OpenInbox(path, m.Account, false); err == nil {
				live.Close()
				t.Fatal("interrupted restore enabled intake")
			}
			// Resume must use durable local evidence even if the backup is unavailable.
			if err = os.Remove(filepath.Join(artifact, "inbox.db")); err != nil {
				t.Fatal(err)
			}
			j, err := ResumeInboxRestore(ctx, path, m.Account)
			if err != nil {
				t.Fatal(err)
			}
			verifyBaselineRestoreForTest(t, path, j)
			again, err := ResumeInboxRestore(ctx, path, m.Account)
			if err != nil || again != j {
				t.Fatal("restart changed quarantine", err)
			}
		})
	}
}

func verifyBaselineRestoreForTest(t *testing.T, path string, j InboxRestoreState) {
	t.Helper()
	ctx := context.Background()
	if j.Phase != "quarantined" || j.IntakeEnabled || j.HistoryComplete || j.Before == j.After || j.Snapshot.Version < 2 {
		t.Fatal(j)
	}
	d, err := privateInboxDirectory(filepath.Join(path, "restore", j.Operation))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = verifyStandaloneInboxFile(d, "source.db", j.Snapshot.DatabaseBytes, j.Snapshot.DatabaseHash); err != nil {
		t.Fatal(err)
	}
	db, err := inboxArtifactDB(d, "candidate.db", true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = verifyQuarantinedInbox(ctx, db, j); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 6 {
		t.Fatal(version, err)
	}
	if live, e := OpenInbox(path, j.Account, false); e == nil {
		live.Close()
		t.Fatal("baseline restore enabled runtime")
	}
	export, e := OpenRestoredRecoveryInbox(ctx, path, j.Account)
	if e != nil {
		t.Fatal(e)
	}
	defer export.Close()
	m := export.manifest()
	if m.BaselineHash != j.Snapshot.BaselineHash || m.EventCount() != j.Snapshot.RetainedEvents {
		t.Fatal("baseline history lost from export")
	}
}
func TestBaselineSnapshotRestoreQuarantine(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, ack := range []bool{false, true} {
			t.Run(fmt.Sprintf("%t/%t", restored, ack), func(t *testing.T) {
				path, artifact, m, p := baselineSnapshotFixture(t, restored, 2, ack)
				ctx := context.Background()
				// Preserve unsnapshotted live data as evidence too; restore never overwrites it.
				live, err := OpenInbox(path, m.Account, false)
				if err != nil {
					t.Fatal(err)
				}
				if err = live.Enqueue(ctx, interaction.SpoolEvent{Key: "after-snapshot", Hash: protocol.Hash("after")}); err != nil {
					t.Fatal(err)
				}
				live.Close()
				before, err := os.ReadFile(filepath.Join(path, "inbox.db"))
				if err != nil {
					t.Fatal(err)
				}
				j, err := PrepareInboxRestore(ctx, path, m.Account, artifact)
				if err != nil {
					t.Fatal(err)
				}
				if j.BeforeBaseline != protocol.Hash(p) || j.Snapshot != m {
					t.Fatal("restore baseline binding changed")
				}
				verifyBaselineRestoreForTest(t, path, j)
				after, err := os.ReadFile(filepath.Join(path, "inbox.db"))
				if err != nil {
					t.Fatal(err)
				}
				if string(before) != string(after) {
					t.Fatal("live evidence overwritten")
				}
				again, err := ResumeInboxRestore(ctx, path, m.Account)
				if err != nil || again != j {
					t.Fatal("restart changed quarantine", err)
				}
			})
		}
	}
}
func TestBaselineRestoreLostResponses(t *testing.T) {
	for _, stage := range []string{"restore_prepared", "restore_identity", "restore_transaction", "restore_commit", "restore_quarantined"} {
		t.Run(stage, func(t *testing.T) {
			path, artifact, m, _ := baselineSnapshotFixture(t, true, 2, true)
			ctx := context.Background()
			j, err := prepareInboxRestore(ctx, path, m.Account, artifact, func(at string) error {
				if at == stage {
					return fmt.Errorf("lost response")
				}
				return nil
			})
			if err == nil {
				t.Fatal("missing fault")
			}
			if err = os.Remove(filepath.Join(artifact, "inbox.db")); err != nil {
				t.Fatal(err)
			}
			again, err := ResumeInboxRestore(ctx, path, m.Account)
			if err != nil {
				t.Fatal(err)
			}
			if again.Operation != j.Operation || again.After != j.After || again.BeforeBaseline != j.BeforeBaseline {
				t.Fatal("resumed another restore")
			}
			verifyBaselineRestoreForTest(t, path, again)
		})
	}
}

func TestBaselineRestorePinsCurrentAndArchivedProofSeparately(t *testing.T) {
	path, artifact, m, _ := baselineSnapshotFixture(t, true, 2, true)
	d, err := privateInboxDirectory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var pin inboxPin
	if err = d.ReadJSON("identity.json", &pin); err != nil {
		t.Fatal(err)
	}
	// Restore must tolerate damaged current runtime data and preserve its pin as
	// evidence, independently of the older snapshot's verified baseline proof.
	pin.Baseline = protocol.Hash("later runtime proof")
	if err = d.WriteJSON("identity.json", pin, true); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	j, err := prepareInboxRestore(ctx, path, m.Account, artifact, func(at string) error {
		if at == "restore_prepared" {
			return fmt.Errorf("lost response")
		}
		return nil
	})
	if err == nil || j.BeforeBaseline != pin.Baseline || j.Snapshot.BaselineHash != m.BaselineHash {
		t.Fatal("restore conflated current and archived proof", j, err)
	}
	changed := pin
	changed.Baseline = protocol.Hash("changed after preparation")
	if err = d.WriteJSON("identity.json", changed, true); err != nil {
		t.Fatal(err)
	}
	if _, err = ResumeInboxRestore(ctx, path, m.Account); err == nil {
		t.Fatal("changed root baseline accepted during resume")
	}
	if err = d.WriteJSON("identity.json", pin, true); err != nil {
		t.Fatal(err)
	}
	j, err = ResumeInboxRestore(ctx, path, m.Account)
	if err != nil {
		t.Fatal(err)
	}
	verifyBaselineRestoreForTest(t, path, j)
}

func TestBaselineRestoreRejectsChangedCandidateEvidence(t *testing.T) {
	for _, kind := range []string{"proof", "old_pin", "old_ack", "new_pin", "event", "sequence", "schema", "binding"} {
		t.Run(kind, func(t *testing.T) {
			path, artifact, m, _ := baselineSnapshotFixture(t, true, 2, true)
			ctx := context.Background()
			j, err := PrepareInboxRestore(ctx, path, m.Account, artifact)
			if err != nil {
				t.Fatal(err)
			}
			d, err := privateInboxDirectory(filepath.Join(path, "restore", j.Operation))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			db, err := inboxArtifactDB(d, "candidate.db", false)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			switch kind {
			case "proof":
				_, err = db.Exec("UPDATE signal_runtime SET body='{}'")
			case "old_pin":
				_, err = db.Exec("UPDATE restore_identity SET body='{}'")
			case "old_ack":
				_, err = db.Exec("UPDATE restore_identity SET acked=0")
			case "new_pin":
				_, err = db.Exec("UPDATE identity SET body='{}'")
			case "event":
				_, err = db.Exec("DELETE FROM events WHERE position=6")
			case "sequence":
				_, err = db.Exec("UPDATE sqlite_sequence SET seq=5")
			case "schema":
				_, err = db.Exec("PRAGMA user_version=3")
			case "binding":
				_, err = db.Exec("UPDATE restore_binding SET body='{}'")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = verifyQuarantinedInbox(ctx, db, j); err == nil {
				t.Fatal("corrupted candidate accepted")
			}
		})
	}
}
