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

type baselineCrashInput struct {
	Path, Stage string
	Cursor      interaction.SpoolCursor
}

func TestInboxBaselineCrashChild(t *testing.T) {
	file := os.Getenv("SIGNAL_BASELINE_CRASH_INPUT")
	if file == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var in baselineCrashInput
	if err = json.Unmarshal(raw, &in); err != nil {
		t.Fatal(err)
	}
	s, err := OpenInbox(in.Path, in.Cursor.Account, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if in.Stage == "enqueue" {
		err = s.Enqueue(ctx, interaction.SpoolEvent{Key: "new-after-baseline", Hash: protocol.Hash("new")})
	} else {
		var page interaction.SpoolPage
		page, err = s.List(ctx, in.Cursor)
		if err == nil && in.Stage == "ack" {
			err = s.Ack(ctx, page.Cursor)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("READY")
	select {}
}
func TestInboxBaselineSIGKILL(t *testing.T) {
	for _, stage := range []string{"enqueue", "list", "ack"} {
		t.Run(stage, func(t *testing.T) {
			s, path, p := baselineInboxFixture(t, true)
			ctx := context.Background()
			event := interaction.SpoolEvent{Key: "new-after-baseline", Hash: protocol.Hash("new")}
			if stage != "enqueue" {
				if err := s.Enqueue(ctx, event); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(baselineCrashInput{path, stage, p.Handoff.Target})
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(t.TempDir(), "input.json")
			if err = os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInboxBaselineCrashChild$")
			cmd.Env = append(os.Environ(), "SIGNAL_BASELINE_CRASH_INPUT="+file)
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
			s, err = OpenInbox(path, p.Handoff.Target.Account, false)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.Enqueue(ctx, event); err != nil {
				t.Fatal(err)
			}
			start := p.Handoff.Target
			if stage == "ack" {
				start.Position++
				if err = s.Ack(ctx, start); err != nil {
					t.Fatal("lost ACK response was not idempotent", err)
				}
			}
			page, err := s.List(ctx, start)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "ack" {
				if len(page.Events) != 0 {
					t.Fatal("ACKed event reappeared")
				}
			} else if len(page.Events) != 1 || page.Cursor.Position != p.Handoff.Target.Position+1 {
				t.Fatal("committed new event lost or duplicated", page)
			}
			var n int
			if err = s.db.QueryRow("SELECT count(*) FROM events WHERE position>?", p.Handoff.Target.Position).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Fatal("replay allocated another event")
			}
		})
	}
}
