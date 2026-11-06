package relay

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/testutil"
)

func TestLegacySignalBootstrapCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_LEGACY_SIGNAL_INBOX")
	if path == "" {
		t.Skip("child helper")
	}
	_, err := bootstrapLegacyInbox(context.Background(), path, fixtureAccount, os.Getenv("DEPLOY_LEGACY_SIGNAL_OPERATION"), func(at string) error {
		if at == os.Getenv("DEPLOY_LEGACY_SIGNAL_BOUNDARY") {
			os.Stdout.WriteString("READY\n")
			select {}
		}
		return nil
	})
	testutil.Must(t, err)
	t.Fatal("boundary not reached")
}
func TestLegacySignalBootstrapSIGKILL(t *testing.T) {
	for _, at := range []string{"legacy_inbox_intent", "legacy_inbox_identity", "legacy_inbox_transaction", "legacy_inbox_commit", "legacy_inbox_complete"} {
		t.Run(at, func(t *testing.T) {
			path := t.TempDir()
			testutil.Must(t, os.Chmod(path, 0700))
			operation := testutil.ID(t, "n")
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLegacySignalBootstrapCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_LEGACY_SIGNAL_INBOX="+path, "DEPLOY_LEGACY_SIGNAL_OPERATION="+operation, "DEPLOY_LEGACY_SIGNAL_BOUNDARY="+at)
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
			scan := bufio.NewScanner(pipe)
			if !scan.Scan() || scan.Text() != "READY" {
				t.Fatal("boundary not reached", stderr.String())
			}
			testutil.Must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("child survived")
			}
			result, err := BootstrapLegacyInbox(ctx, path, fixtureAccount, operation)
			testutil.Must(t, err)
			testutil.Must(t, VerifyLegacyInboxBootstrap(ctx, path, result))
		})
	}
}
