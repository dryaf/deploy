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

type relayHandoffCrashInput struct {
	Path     string
	Restored bool
	Proof    interaction.SignalRecoveryHandoff
}

func TestSignalRelayHandoffCrashChild(t *testing.T) {
	file := os.Getenv("SIGNAL_RELAY_HANDOFF_INPUT")
	if file == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var in relayHandoffCrashInput
	if err = json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	source, err := reopenHandoffRelay(context.Background(), in.Path, in.Proof.Target.Account, in.Restored)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	fault := func(at string) error {
		if at == os.Getenv("SIGNAL_RELAY_HANDOFF_STAGE") {
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
	if _, err = source.PrepareSignalHandoff(context.Background(), in.Proof); err != nil {
		t.Fatal(err)
	}
	t.Fatal("missing fault")
}
func TestSignalRelayHandoffSIGKILL(t *testing.T) {
	for _, restored := range []bool{false, true} {
		for _, stage := range []string{"signal_handoff_source_intent", "signal_handoff_source_transaction", "signal_handoff_source_commit"} {
			if restored && stage == "signal_handoff_source_transaction" {
				continue
			}
			t.Run(fmt.Sprintf("%t/%s", restored, stage), func(t *testing.T) {
				source, path, p := handoffRelayFixture(t, restored)
				if err := source.Close(); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(relayHandoffCrashInput{path, restored, p})
				if err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(t.TempDir(), "input.json")
				if err = os.WriteFile(file, raw, 0600); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalRelayHandoffCrashChild$")
				cmd.Env = append(os.Environ(), "SIGNAL_RELAY_HANDOFF_INPUT="+file, "SIGNAL_RELAY_HANDOFF_STAGE="+stage)
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
				committed := stage == "signal_handoff_source_commit"
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
					if (export.Handoff != nil) != committed {
						t.Fatal("relay checkpoint was not atomic")
					}
				case *RestoredRecoveryInbox:
					if (s.journal.GatewayHandoff != "") != committed {
						t.Fatal("restored relay checkpoint was not atomic")
					}
				}
				got, err := source.PrepareSignalHandoff(ctx, p)
				if err != nil {
					t.Fatal(err)
				}
				again, err := source.PrepareSignalHandoff(ctx, p)
				if err != nil {
					t.Fatal(err)
				}
				if protocol.Hash(got) != protocol.Hash(p) || protocol.Hash(again) != protocol.Hash(p) {
					t.Fatal("restart changed proof")
				}
			})
		}
	}
}
