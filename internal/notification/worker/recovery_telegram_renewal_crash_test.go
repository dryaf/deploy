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

func TestTelegramRenewalCrashChild(t *testing.T) {
	input := os.Getenv("TELEGRAM_RENEWAL_INPUT")
	if input == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(input)
	must(t, err)
	var f telegramRenewalFixture
	must(t, json.Unmarshal(raw, &f))
	responses := 0
	_, err = f.run(func(at string) error {
		if at == "telegram_renewal_response" {
			responses++
		}
		stage := os.Getenv("TELEGRAM_RENEWAL_STAGE")
		if at == stage || (stage == "response-after-ack" && at == "telegram_renewal_response" && responses == 2) {
			fmt.Println("READY")
			select {}
		}
		return nil
	})
	must(t, err)
	t.Fatal("missing crash boundary")
}

func TestTelegramRenewalSIGKILL(t *testing.T) {
	for _, stage := range []string{"telegram_renewal_intent", "telegram_renewal_directory", "telegram_renewal_before_poll", "telegram_renewal_response", "response-after-ack", "telegram_renewal_page", "telegram_renewal_checkpoint", "telegram_renewal_complete"} {
		t.Run(stage, func(t *testing.T) {
			f := newTelegramRenewalFixture(t)
			f.Poller.File = filepath.Join(t.TempDir(), "provider.json")
			file := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(f)
			must(t, err)
			must(t, os.WriteFile(file, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTelegramRenewalCrashChild$")
			cmd.Env = append(os.Environ(), "TELEGRAM_RENEWAL_INPUT="+file, "TELEGRAM_RENEWAL_STAGE="+stage)
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
			// Restore the fake provider's independently durable ACK watermark.
			raw, err = os.ReadFile(f.Poller.File)
			if err == nil {
				var state struct {
					Ack     int64
					Offsets []int64
				}
				must(t, json.Unmarshal(raw, &state))
				f.Poller.Ack = state.Ack
				if stage == "response-after-ack" && state.Ack == 0 {
					t.Fatal("fixture did not acknowledge provider")
				}
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			f.Poller.File = ""
			result, err := f.run(nil)
			must(t, err)
			if result.IntakeEnabled || result.Cursor.IntakeEnabled {
				t.Fatal("crash retry activated intake")
			}
			if len(f.Poller.Offsets) > 0 && f.Poller.Offsets[0] != 0 {
				t.Fatal("crash retry acknowledged unseen history", f.Poller.Offsets)
			}
			if stage == "telegram_renewal_complete" && len(f.Poller.Offsets) != 0 {
				t.Fatal("completed retry polled")
			}
			f.At = f.At.Add(6 * time.Second)
			f.Up += 6
			_, err = f.verify(result.RenewalHash)
			must(t, err)
		})
	}
}
