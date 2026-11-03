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

type signalCursorCrashInput struct {
	Path   string
	Config Config
	Proof  interaction.SignalRecoveryCursor
}

func TestSignalCursorCrashChild(t *testing.T) {
	file := os.Getenv("SIGNAL_CURSOR_INPUT")
	if file == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(file)
	must(t, err)
	var in signalCursorCrashInput
	must(t, json.Unmarshal(raw, &in))
	session, err := OpenSignalCursorSession(context.Background(), in.Path, in.Config)
	must(t, err)
	defer session.Close()
	r := session.(*recoveryReconciler)
	r.fault = func(at string) error {
		if at == os.Getenv("SIGNAL_CURSOR_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	must(t, r.InstallSignalCursor(context.Background(), in.Proof))
	t.Fatal("missing fault")
}
func TestSignalCursorSIGKILL(t *testing.T) {
	for _, stage := range []string{"signal_cursor_intent", "signal_cursor_transaction", "signal_cursor_commit"} {
		t.Run(stage, func(t *testing.T) {
			f, p := signalCursorFixture(t)
			ctx := context.Background()
			must(t, f.r.Close())
			raw, err := json.Marshal(signalCursorCrashInput{f.path, f.r.config, p})
			must(t, err)
			file := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(file, raw, 0600))
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalCursorCrashChild$")
			cmd.Env = append(os.Environ(), "SIGNAL_CURSOR_INPUT="+file, "SIGNAL_CURSOR_STAGE="+stage)
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
			session, err := OpenSignalCursorSession(ctx, f.path, f.r.config)
			must(t, err)
			defer session.Close()
			r := session.(*recoveryReconciler)
			var count int
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_signal_cursors").Scan(&count))
			expected := 0
			if stage == "signal_cursor_commit" {
				expected = 1
			}
			if count != expected {
				t.Fatal("partial agreement visible", count)
			}
			var cursor interaction.SpoolCursor
			var body string
			must(t, r.db.QueryRow("SELECT cursor FROM signal_imports WHERE account=?", p.Handoff.Target.Account).Scan(&body))
			must(t, protocol.Decode([]byte(body), &cursor, 4096))
			expectedCursor := p.Handoff.Before
			if stage == "signal_cursor_commit" {
				expectedCursor = p.Handoff.Target
			}
			if cursor != expectedCursor {
				t.Fatal("partial cursor installation", cursor)
			}
			again, err := interaction.InstallSignalRecoveryCursor(ctx, f.source, session)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(p) {
				t.Fatal("restart changed proof")
			}
		})
	}
}
