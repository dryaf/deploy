//go:build linux

package backup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupNextRecoveryCaptureSIGKILL(t *testing.T) {
	for _, boundary := range []string{"backup_next_ancestor", "backup_recovery_transaction", "backup_recovery_commit"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			f, home, spec, deps, _ := backupRecoveryActivationFixture(t, true)
			testutil.Must(t, runBackupRecoveryActivation(ctx, home, f.config, spec, false, deps))
			var previous backupNotificationRecovery
			var owner backupNotificationOwner
			testutil.Must(t, f.state.get("notification_recovery", &previous))
			testutil.Must(t, f.state.get("notification_owner", &owner))
			clock := reportHostClock()
			operation, err := safefs.NewID("n")
			testutil.Must(t, err)
			nonce, err := safefs.NewID("n")
			testutil.Must(t, err)
			after := owner.Binding.Identity
			after.Recovery, err = safefs.NewID("n")
			testutil.Must(t, err)
			ch := notify.RecoveryChallenge{Operation: operation, Nonce: nonce, Identity: after, Producer: owner.Binding.Producer, IssuedAt: clock.Now, ExpiresAt: clock.Now.Add(10 * time.Minute), BootID: clock.Boot, Uptime: clock.Uptime}
			path := f.state.directory
			testutil.Must(t, f.state.Close())
			input := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(struct {
				Challenge notify.RecoveryChallenge
				Clock     backupEvaluationClock
			}{ch, clock})
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(input, raw, 0600))
			childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestBackupRecoveryCaptureCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_RECOVERY_STATE="+path, "DEPLOY_BACKUP_RECOVERY_INPUT="+input, "DEPLOY_BACKUP_RECOVERY_BOUNDARY="+boundary)
			pipe, err := cmd.StdoutPipe()
			testutil.Must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			testutil.Must(t, cmd.Start())
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				t.Fatal("child did not reach boundary", scanner.Text(), stderr.String())
			}
			testutil.Must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("child survived")
			}
			f.state, err = openBackupState(path, false)
			testutil.Must(t, err)
			defer f.state.Close()
			var saved backupNotificationRecovery
			testutil.Must(t, f.state.get("notification_recovery", &saved))
			if boundary != "backup_recovery_commit" && notify.Hash(saved) != notify.Hash(previous) {
				t.Fatal("uncommitted replacement survived")
			}
			journal, err := beginBackupNotificationRecoveryWith(ctx, f.state, ch, clock, nil)
			testutil.Must(t, err)
			if journal.Version != 2 || journal.Ancestor == "" || journal.Binding != owner.Binding {
				t.Fatal("next epoch lost predecessor")
			}
			testutil.Must(t, requireBackupRecoveryHistory(f.state))
			if requireNotificationRecoveryInactiveTx(f.state) == nil {
				t.Fatal("capture enabled reporting")
			}
			again, err := beginBackupNotificationRecoveryWith(ctx, f.state, ch, clock, nil)
			testutil.Must(t, err)
			if notify.Hash(again) != notify.Hash(journal) {
				t.Fatal("capture resume changed history")
			}
		})
	}
}
