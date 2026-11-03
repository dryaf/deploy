package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func relayRestoreFixture(t *testing.T) (string, string, InboxSnapshotManifest) {
	t.Helper()
	ctx := context.Background()
	r, path, ch := relayRecoveryFixture(t, 3)
	if _, err := r.Capture(ctx, ch); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	m, err := SnapshotInbox(ctx, path, fixtureAccount, artifact)
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenInbox(path, fixtureAccount, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Enqueue(ctx, interaction.SpoolEvent{Key: "after-snapshot", Hash: protocol.Hash("late update")}); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	return path, artifact, m
}
func TestRelaySnapshotStandaloneAndRestoreQuarantine(t *testing.T) {
	path, artifact, m := relayRestoreFixture(t)
	ctx := context.Background()
	verified, err := VerifyInboxSnapshot(ctx, artifact, fixtureAccount)
	if err != nil || protocol.Hash(verified) != protocol.Hash(m) {
		t.Fatal(verified, err)
	}
	if m.Schema != 2 || m.Highest != 3 || m.Acknowledged != 3 || m.Listed != 3 {
		t.Fatal(m)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err = os.Stat(filepath.Join(artifact, "inbox.db"+suffix)); !os.IsNotExist(err) {
			t.Fatal("snapshot has sidecar", suffix, err)
		}
	}
	live, err := os.ReadFile(filepath.Join(path, "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := PrepareInboxRestore(ctx, path, fixtureAccount, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if j.Phase != "quarantined" || j.Before != m.Identity || j.After == j.Before || j.After.Worker != j.Before.Worker || j.IntakeEnabled || j.HistoryComplete {
		t.Fatal(j)
	}
	after, err := os.ReadFile(filepath.Join(path, "inbox.db"))
	if err != nil || !bytes.Equal(live, after) {
		t.Fatal("live evidence overwritten", err)
	}
	for _, create := range []bool{false, true} {
		if s, err := OpenInbox(path, fixtureAccount, create); err == nil {
			s.Close()
			t.Fatal("ordinary relay started")
		}
	}
	if s, err := OpenRecoveryInbox(path, fixtureAccount); err == nil {
		s.Close()
		t.Fatal("normal history export bypassed restore fence")
	}
	if _, err = SnapshotInbox(ctx, path, fixtureAccount, filepath.Join(t.TempDir(), "forbidden")); err == nil {
		t.Fatal("fenced relay snapshotted as active")
	}
	d, err := privateInboxDirectory(filepath.Join(path, "restore", j.Operation))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = verifyInboxFile(d, "source.db", m.DatabaseBytes, m.DatabaseHash); err != nil {
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
	var exports int
	if err = db.QueryRow("SELECT count(*) FROM recovery_exports").Scan(&exports); err != nil || exports != 1 {
		t.Fatal("prior export audit lost", exports, err)
	}
	again, err := ResumeInboxRestore(ctx, path, fixtureAccount)
	if err != nil || protocol.Hash(again) != protocol.Hash(j) {
		t.Fatal("resume changed completed restore", again, err)
	}
	if _, err = PrepareInboxRestore(ctx, path, fixtureAccount, artifact); err == nil {
		t.Fatal("second prepare reset recovery")
	}
	// Even loss of the external journal cannot reopen an epoch marked in the pin.
	if err = os.Remove(filepath.Join(path, "restore.json")); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenInbox(path, fixtureAccount, false); err == nil {
		s.Close()
		t.Fatal("lost journal reopened relay")
	}
}
func TestRelayRestorePreservesUnavailableLiveDatabase(t *testing.T) {
	for _, kind := range []string{"missing", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			path, artifact, _ := relayRestoreFixture(t)
			if kind == "missing" {
				if err := os.Remove(filepath.Join(path, "inbox.db")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(filepath.Join(path, "inbox.db"), []byte("damaged live evidence"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			j, err := PrepareInboxRestore(context.Background(), path, fixtureAccount, artifact)
			if err != nil || j.Phase != "quarantined" {
				t.Fatal(j, err)
			}
			if kind == "missing" {
				if _, err = os.Stat(filepath.Join(path, "inbox.db")); !os.IsNotExist(err) {
					t.Fatal("missing live database recreated", err)
				}
			} else {
				raw, err := os.ReadFile(filepath.Join(path, "inbox.db"))
				if err != nil || string(raw) != "damaged live evidence" {
					t.Fatal("damaged evidence overwritten", err)
				}
			}
		})
	}
}
func TestRelaySnapshotAndRestoreRejectUnsafeSources(t *testing.T) {
	for _, kind := range []string{"checksum", "sidecar", "permission", "identity", "watermark", "event_gap", "busy", "lock", "symlink", "existing_destination"} {
		t.Run(kind, func(t *testing.T) {
			path, artifact, m := relayRestoreFixture(t)
			ctx := context.Background()
			switch kind {
			case "checksum":
				f, err := os.OpenFile(filepath.Join(artifact, "inbox.db"), os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteAt([]byte("changed"), 100)
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "sidecar":
				if err := os.WriteFile(filepath.Join(artifact, "inbox.db-wal"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "permission":
				if err := os.Chmod(filepath.Join(artifact, "inbox.db"), 0644); err != nil {
					t.Fatal(err)
				}
			case "identity", "watermark":
				if kind == "identity" {
					m.Identity.Recovery = "other-epoch"
				} else {
					m.Acknowledged = 99
				}
				raw, _ := json.Marshal(m)
				if err := os.WriteFile(filepath.Join(artifact, "snapshot.json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "event_gap":
				s, err := OpenInbox(path, fixtureAccount, false)
				if err != nil {
					t.Fatal(err)
				}
				_, err = s.db.Exec("DELETE FROM events WHERE position=2")
				s.Close()
				if err != nil {
					t.Fatal(err)
				}
				if _, err = SnapshotInbox(ctx, path, fixtureAccount, filepath.Join(t.TempDir(), "bad")); err == nil {
					t.Fatal("snapshot accepted missing native history")
				}
				return
			case "busy":
				s, err := OpenInbox(path, fixtureAccount, false)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if _, err = SnapshotInbox(ctx, path, fixtureAccount, filepath.Join(t.TempDir(), "busy")); err == nil {
					t.Fatal("snapshot ignored live lease")
				}
			case "lock":
				if err := os.Remove(filepath.Join(path, "inbox.lock")); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(artifact, alias); err != nil {
					t.Fatal(err)
				}
				artifact = alias
			case "existing_destination":
				if _, err := SnapshotInbox(ctx, path, fixtureAccount, artifact); err == nil {
					t.Fatal("snapshot overwritten")
				}
				return
			}
			if _, err := PrepareInboxRestore(ctx, path, fixtureAccount, artifact); err == nil {
				t.Fatal("unsafe restore source accepted", kind)
			}
		})
	}
}
func TestRelayRestoreDamagedJournalCandidateAndEpochStayFenced(t *testing.T) {
	for _, kind := range []string{"journal", "source", "candidate", "source_wal", "candidate_wal", "epoch", "binding", "missing_candidate"} {
		t.Run(kind, func(t *testing.T) {
			path, artifact, _ := relayRestoreFixture(t)
			ctx := context.Background()
			j, err := PrepareInboxRestore(ctx, path, fixtureAccount, artifact)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "journal":
				err = os.WriteFile(filepath.Join(path, "restore.json"), []byte("{"), 0600)
			case "source", "candidate":
				err = os.WriteFile(filepath.Join(path, "restore", j.Operation, kind+".db"), []byte("changed"), 0600)
			case "source_wal", "candidate_wal":
				err = os.WriteFile(filepath.Join(path, "restore", j.Operation, strings.TrimSuffix(kind, "_wal")+".db-wal"), nil, 0600)
			case "missing_candidate":
				err = os.Remove(filepath.Join(path, "restore", j.Operation, "candidate.db"))
			case "epoch":
				raw, _ := json.Marshal(inboxPin{Identity: j.Before, Account: fixtureAccount})
				err = os.WriteFile(filepath.Join(path, "identity.json"), raw, 0600)
			case "binding":
				j.Phase = "epoch_published"
				j.CandidateHash = ""
				j.CandidateBytes = 0
				raw, _ := json.Marshal(j)
				err = os.WriteFile(filepath.Join(path, "restore.json"), raw, 0600)
				if err != nil {
					t.Fatal(err)
				}
				d, err := privateInboxDirectory(filepath.Join(path, "restore", j.Operation))
				if err != nil {
					t.Fatal(err)
				}
				db, err := inboxArtifactDB(d, "candidate.db", false)
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec("UPDATE restore_binding SET body='{}'")
				db.Close()
				d.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ResumeInboxRestore(ctx, path, fixtureAccount); err == nil {
				t.Fatal("damaged restore resumed")
			}
			if s, err := OpenInbox(path, fixtureAccount, false); err == nil {
				s.Close()
				t.Fatal("damaged restore opened normal inbox")
			}
		})
	}
}
func TestRelayExistingHandlesAndRepliesHonorRestoreFence(t *testing.T) {
	s, path := inboxFixture(t)
	ctx := context.Background()
	d := interaction.Destination{ID: "private", Generation: "one", Provider: "signal", Account: fixtureAccount, NativeID: fixtureActor}
	mux := http.NewServeMux()
	calls := 0
	installInteractiveHandlers(mux, s, InteractiveConfig{Account: fixtureAccount, Destinations: []interaction.Destination{d}}, make(chan struct{}, 1), func(context.Context, io.Writer, ...string) error { calls++; return nil })
	if err := os.WriteFile(filepath.Join(path, "restore.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue(ctx, interaction.SpoolEvent{Key: "one", Hash: protocol.Hash("one")}); err == nil {
		t.Fatal("fenced handle wrote")
	}
	if _, err := s.List(ctx, s.Cursor()); err == nil {
		t.Fatal("fenced handle listed")
	}
	if err := s.Ack(ctx, s.Cursor()); err == nil {
		t.Fatal("fenced handle acknowledged")
	}
	if err := s.CanReceive(ctx); err == nil {
		t.Fatal("fenced handle received")
	}
	raw, _ := json.Marshal(map[string]any{"identity": s.Cursor().Identity, "account": fixtureAccount, "destination": d.ID, "fingerprint": d.Fingerprint(), "text": "must not send"})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("POST", "/reply", strings.NewReader(string(raw))))
	if w.Code != 409 || calls != 0 {
		t.Fatal("fenced reply sent", w.Code, calls)
	}
}

func TestRelayRestoreCrashChild(t *testing.T) {
	path := os.Getenv("RELAY_RESTORE_CRASH_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	_, err := prepareInboxRestore(context.Background(), path, fixtureAccount, os.Getenv("RELAY_RESTORE_CRASH_SNAPSHOT"), func(stage string) error {
		if stage == os.Getenv("RELAY_RESTORE_CRASH_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("fault not reached")
}
func TestRelayRestoreSIGKILL(t *testing.T) {
	for _, stage := range []string{"restore_prepared", "restore_identity", "restore_transaction", "restore_commit", "restore_quarantined"} {
		t.Run(stage, func(t *testing.T) {
			path, artifact, m := relayRestoreFixture(t)
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
			if s, err := OpenInbox(path, fixtureAccount, false); err == nil {
				s.Close()
				t.Fatal("interrupted restore reopened intake")
			}
			j, err := ResumeInboxRestore(ctx, path, fixtureAccount)
			if err != nil || j.Phase != "quarantined" || j.IntakeEnabled || j.HistoryComplete || j.Before != m.Identity {
				t.Fatal(j, err)
			}
			again, err := ResumeInboxRestore(ctx, path, fixtureAccount)
			if err != nil || protocol.Hash(again) != protocol.Hash(j) {
				t.Fatal("resume rotated epoch or changed audit", err)
			}
		})
	}
}
func TestRelayRestoreLostCommitResponse(t *testing.T) {
	path, artifact, _ := relayRestoreFixture(t)
	j, err := prepareInboxRestore(context.Background(), path, fixtureAccount, artifact, func(stage string) error {
		if stage == "restore_commit" {
			return errors.New("lost response")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault not reached")
	}
	// Resume depends on the retained immutable copy, not continued availability
	// of the original backup mount.
	if err = os.Remove(filepath.Join(artifact, "inbox.db")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(artifact, "snapshot.json")); err != nil {
		t.Fatal(err)
	}
	again, err := ResumeInboxRestore(context.Background(), path, fixtureAccount)
	if err != nil || again.After != j.After || again.Operation != j.Operation {
		t.Fatal(again, err)
	}
}

func TestRelayEmptySchemaOneSnapshotAndAliasRejection(t *testing.T) {
	s, path := inboxFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	artifact := filepath.Join(t.TempDir(), "empty")
	m, err := SnapshotInbox(ctx, path, fixtureAccount, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema != 1 || m.Highest != 0 || m.EventsDigest != interaction.RecoveryEvidenceDigest(nil) {
		t.Fatal(m)
	}
	j, err := prepareInboxRestore(ctx, path, fixtureAccount, artifact, func(stage string) error {
		if stage == "restore_prepared" {
			return errors.New("interrupt")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault not reached")
	}
	source := filepath.Join(path, "restore", j.Operation, "source.db")
	candidate := filepath.Join(path, "restore", j.Operation, "candidate.db")
	if err = os.Remove(candidate); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(source, candidate); err != nil {
		t.Fatal(err)
	}
	if _, err = ResumeInboxRestore(ctx, path, fixtureAccount); err == nil {
		t.Fatal("candidate alias could overwrite retained evidence")
	}
	if err = os.Remove(candidate); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(candidate, raw, 0600); err != nil {
		t.Fatal(err)
	}
	finished, err := ResumeInboxRestore(ctx, path, fixtureAccount)
	if err != nil || finished.Phase != "quarantined" || finished.Snapshot.Highest != 0 || finished.HistoryComplete || finished.IntakeEnabled {
		t.Fatal(finished, err)
	}
}
