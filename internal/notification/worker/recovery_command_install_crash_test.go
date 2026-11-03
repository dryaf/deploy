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

type commandInstallCrashInput struct {
	Path       string
	Config     Config
	Checkpoint interaction.CommandRecoveryCommit
}

func TestCommandRecoveryInstallationCrashChild(t *testing.T) {
	input := os.Getenv("COMMAND_INSTALL_INPUT")
	if input == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(input)
	must(t, err)
	var v commandInstallCrashInput
	must(t, json.Unmarshal(raw, &v))
	session, err := OpenCommandRecoveryInstallSession(context.Background(), v.Path, v.Config)
	must(t, err)
	defer session.Close()
	r := session.(*recoveryReconciler)
	r.fault = func(stage string) error {
		if stage == os.Getenv("COMMAND_INSTALL_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	_, err = r.InstallCommandRecoveryOutcomes(context.Background(), v.Checkpoint)
	must(t, err)
	t.Fatal("fault missing")
}
func TestCommandRecoveryInstallationSIGKILL(t *testing.T) {
	for _, stage := range []string{"command_install_intent", "command_install_transaction", "command_install_commit"} {
		t.Run(stage, func(t *testing.T) {
			f, proof := commandInstallFixture(t, false)
			cfg := f.r.config
			must(t, f.r.Close())
			input := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(commandInstallCrashInput{f.path, cfg, proof})
			must(t, err)
			must(t, os.WriteFile(input, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandRecoveryInstallationCrashChild$")
			cmd.Env = append(os.Environ(), "COMMAND_INSTALL_INPUT="+input, "COMMAND_INSTALL_STAGE="+stage)
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
				t.Fatal("fault missing", stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			session, err := OpenCommandRecoveryInstallSession(ctx, f.path, cfg)
			must(t, err)
			defer session.Close()
			r := session.(*recoveryReconciler)
			var records int
			var state string
			var revision uint64
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_command_installations").Scan(&records))
			must(t, r.db.QueryRow("SELECT state,revision FROM commands WHERE id=?", f.first.ID).Scan(&state, &revision))
			if stage == "command_install_commit" {
				if records != 1 || state != "succeeded" || revision != 1 {
					t.Fatal("committed installation lost", records, state, revision)
				}
			} else if records != 0 || state != "outcome_unknown" || revision != 0 {
				t.Fatal("partial transaction visible", records, state, revision)
			}
			got, err := interaction.InstallCommandRecovery(ctx, f.ledger, session)
			must(t, err)
			again, err := interaction.InstallCommandRecovery(ctx, f.ledger, session)
			must(t, err)
			if protocol.Hash(got) != protocol.Hash(again) {
				t.Fatal("restart changed installation")
			}
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_command_installations").Scan(&records))
			if records != 1 {
				t.Fatal("installation duplicated")
			}
			must(t, session.Close())
			if normal, err := Open(f.path, cfg, false); err == nil {
				normal.Close()
				t.Fatal("restart enabled execution")
			}
		})
	}
}
