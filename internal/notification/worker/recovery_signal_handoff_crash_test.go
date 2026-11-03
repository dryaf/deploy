package worker

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

type signalHandoffCrashInput struct {
	Path   string
	Config Config
	Proof  interaction.SignalRecoveryHandoff
}

func TestSignalHandoffCrashChild(t *testing.T) {
	file := os.Getenv("SIGNAL_HANDOFF_INPUT")
	if file == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(file)
	must(t, err)
	var in signalHandoffCrashInput
	must(t, json.Unmarshal(raw, &in))
	session, err := OpenSignalHandoffSession(context.Background(), in.Path, in.Config)
	must(t, err)
	defer session.Close()
	r := session.(*recoveryReconciler)
	r.fault = func(at string) error {
		if at == os.Getenv("SIGNAL_HANDOFF_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	must(t, r.RecordSignalHandoff(context.Background(), in.Proof))
	t.Fatal("missing fault")
}
func TestSignalHandoffSIGKILL(t *testing.T) {
	for _, stage := range []string{"signal_handoff_intent", "signal_handoff_transaction", "signal_handoff_commit"} {
		t.Run(stage, func(t *testing.T) {
			f, p := signalHandoffFixture(t)
			ctx := context.Background()
			_, err := f.source.PrepareSignalHandoff(ctx, p)
			must(t, err)
			must(t, f.r.Close())
			raw, err := json.Marshal(signalHandoffCrashInput{f.path, f.r.config, p})
			must(t, err)
			file := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(file, raw, 0600))
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalHandoffCrashChild$")
			cmd.Env = append(os.Environ(), "SIGNAL_HANDOFF_INPUT="+file, "SIGNAL_HANDOFF_STAGE="+stage)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			must(t, cmd.Start())
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
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			session, err := OpenSignalHandoffSession(ctx, f.path, f.r.config)
			must(t, err)
			defer session.Close()
			r := session.(*recoveryReconciler)
			var count int
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_signal_handoffs").Scan(&count))
			expected := 0
			if stage == "signal_handoff_commit" {
				expected = 1
			}
			if count != expected {
				t.Fatal("partial agreement visible", count)
			}
			assertSignalHandoffQuarantined(t, r, f.path, p)
			again, err := interaction.PrepareSignalRecoveryHandoff(ctx, f.source, session)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(p) {
				t.Fatal("restart changed proof")
			}
		})
	}
}
