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
)

type telegramCursorCrashInput struct {
	Path, Account string
	Config        Config
}

func TestTelegramCursorCrashChild(t *testing.T) {
	file := os.Getenv("TELEGRAM_CURSOR_INPUT")
	if file == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(file)
	must(t, err)
	var in telegramCursorCrashInput
	must(t, json.Unmarshal(raw, &in))
	session, err := OpenTelegramRecoveryCursorSession(context.Background(), in.Path, in.Config)
	must(t, err)
	defer session.Close()
	r := session.(*recoveryReconciler)
	r.fault = func(at string) error {
		if at == os.Getenv("TELEGRAM_CURSOR_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	_, err = r.CommitTelegramRecoveryCursor(context.Background(), in.Account)
	must(t, err)
	t.Fatal("missing crash boundary")
}
func TestTelegramCursorSIGKILL(t *testing.T) {
	for _, stage := range []string{"telegram_cursor_intent", "telegram_cursor_transaction", "telegram_cursor_commit"} {
		t.Run(stage, func(t *testing.T) {
			f, r, _ := telegramCursorFixture(t)
			must(t, r.Close())
			file := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(telegramCursorCrashInput{f.Path, f.Account, f.Config})
			must(t, err)
			must(t, os.WriteFile(file, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTelegramCursorCrashChild$")
			cmd.Env = append(os.Environ(), "TELEGRAM_CURSOR_INPUT="+file, "TELEGRAM_CURSOR_STAGE="+stage)
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
			session, err := OpenTelegramRecoveryCursorSession(ctx, f.Path, f.Config)
			must(t, err)
			defer session.Close()
			resumed := session.(*recoveryReconciler)
			var count, offset int
			must(t, resumed.db.QueryRow("SELECT count(*) FROM reconciliation_telegram_cursors").Scan(&count))
			must(t, resumed.db.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", f.Account).Scan(&offset))
			if stage == "telegram_cursor_commit" {
				if count != 1 || offset != 0 {
					t.Fatal("committed handoff lost", count, offset)
				}
			} else if count != 0 || offset != 6 {
				t.Fatal("partial handoff visible", count, offset)
			}
			a, err := session.CommitTelegramRecoveryCursor(ctx, f.Account)
			must(t, err)
			b, err := session.CommitTelegramRecoveryCursor(ctx, f.Account)
			must(t, err)
			if a != b || a.Offset != 0 {
				t.Fatal("restart changed handoff")
			}
		})
	}
}
