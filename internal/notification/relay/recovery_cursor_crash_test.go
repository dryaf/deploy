package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type relayCursorCrashInput struct {
	Path     string
	Restored bool
	Proof    interaction.SignalRecoveryHandoff
}

func TestSignalCursorRelayCrashChild(t *testing.T) {
	file := os.Getenv("SIGNAL_RELAY_CURSOR_INPUT")
	if file == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var in relayCursorCrashInput
	if err = json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	source, err := reopenHandoffRelay(context.Background(), in.Path, in.Proof.Target.Account, in.Restored)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	fault := func(at string) error {
		if at == os.Getenv("SIGNAL_RELAY_CURSOR_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	switch s := source.(type) {
	case *RecoveryInbox:
		s.fault = fault
	case *RestoredRecoveryInbox:
		s.fault = fault
	}
	if _, err = source.(interaction.SignalCursorSource).StageSignalCursor(context.Background(), in.Proof); err != nil {
		t.Fatal(err)
	}
	t.Fatal("missing fault")
}
func TestSignalCursorRelaySIGKILL(t *testing.T) {
	for _, baseline := range []bool{false, true} {
		for _, restored := range []bool{false, true} {
			for _, stage := range []string{"signal_cursor_source_intent", "signal_cursor_source_transaction", "signal_cursor_source_commit", "signal_cursor_source_manifest", "signal_cursor_source_published"} {
				t.Run(fmt.Sprintf("baseline_%t/%t/%s", baseline, restored, stage), func(t *testing.T) {
					fixture := handoffRelayFixture
					if baseline {
						fixture = baselineHandoffRelayFixture
					}
					source, path, p := fixture(t, restored)
					if err := source.Close(); err != nil {
						t.Fatal(err)
					}
					raw, err := json.Marshal(relayCursorCrashInput{path, restored, p})
					if err != nil {
						t.Fatal(err)
					}
					file := filepath.Join(t.TempDir(), "input.json")
					if err = os.WriteFile(file, raw, 0600); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalCursorRelayCrashChild$")
					cmd.Env = append(os.Environ(), "SIGNAL_RELAY_CURSOR_INPUT="+file, "SIGNAL_RELAY_CURSOR_STAGE="+stage)
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
						t.Fatal("missing fault", stderr.String())
					}
					if err = cmd.Process.Kill(); err != nil {
						t.Fatal(err)
					}
					if cmd.Wait() == nil {
						t.Fatal("expected SIGKILL")
					}
					source, err = reopenHandoffRelay(ctx, path, p.Target.Account, restored)
					if err != nil {
						t.Fatal(err)
					}
					defer source.Close()
					committed := stage == "signal_cursor_source_published"
					switch s := source.(type) {
					case *RecoveryInbox:
						var raw string
						if err = s.inbox.db.QueryRow("SELECT body FROM recovery_exports WHERE operation=?", p.Summary.Challenge.Operation).Scan(&raw); err != nil {
							t.Fatal(err)
						}
						var export recoveryExport
						if err = protocol.Decode([]byte(raw), &export, 16384); err != nil {
							t.Fatal(err)
						}
						if (export.Cursor != nil) != committed {
							t.Fatal("relay checkpoint was not atomic")
						}
					case *RestoredRecoveryInbox:
						if (s.journal.GatewayCursor != "") != committed {
							t.Fatal("restored relay checkpoint was not atomic")
						}
					}
					got, err := source.(interaction.SignalCursorSource).StageSignalCursor(ctx, p)
					if err != nil {
						t.Fatal(err)
					}
					again, err := source.(interaction.SignalCursorSource).StageSignalCursor(ctx, p)
					if err != nil {
						t.Fatal(err)
					}
					if protocol.Hash(got.Handoff) != protocol.Hash(p) || protocol.Hash(again) != protocol.Hash(got) {
						t.Fatal("restart changed proof")
					}
				})
			}
		}
	}

}
