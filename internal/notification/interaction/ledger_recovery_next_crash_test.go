package interaction

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

type nextCommandCrashInput struct {
	Before    protocol.Identity
	Challenge protocol.RecoveryChallenge
}

func TestCommandLedgerNextCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_COMMAND_NEXT_DB")
	if path == "" {
		t.Skip("child helper")
	}
	raw, err := os.ReadFile(os.Getenv("DEPLOY_COMMAND_NEXT_INPUT"))
	testutil.Must(t, err)
	var input nextCommandCrashInput
	testutil.Must(t, json.Unmarshal(raw, &input))
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	testutil.Must(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	ch := input.Challenge
	_, err = openNextRecoveryLedger(context.Background(), db, ch.Producer, input.Before, ch.Identity, ch.Operation, func(at string) error {
		if at == os.Getenv("DEPLOY_COMMAND_NEXT_BOUNDARY") {
			os.Stdout.WriteString("READY\n")
			select {}
		}
		return nil
	})
	testutil.Must(t, err)
	t.Fatal("boundary not reached")
}
func TestCommandLedgerNextSIGKILL(t *testing.T) {
	for _, boundary := range []string{"command_next_transaction", "command_next_commit"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			l, db, path, gateway := commandCommitFixture(t)
			_, err := CommitCommandRecovery(ctx, l, gateway)
			testutil.Must(t, err)
			before := gateway.proof.Summary.Challenge.Identity
			ch := recoveryChallenge(t, &Ledger{db: db, producer: l.producer, identity: before})
			ch.Identity.Recovery = "third-epoch"
			raw, err := json.Marshal(nextCommandCrashInput{before, ch})
			testutil.Must(t, err)
			input := filepath.Join(t.TempDir(), "input.json")
			testutil.Must(t, os.WriteFile(input, raw, 0600))
			testutil.Must(t, db.Close())
			childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestCommandLedgerNextCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_COMMAND_NEXT_DB="+path, "DEPLOY_COMMAND_NEXT_INPUT="+input, "DEPLOY_COMMAND_NEXT_BOUNDARY="+boundary)
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
			db, err = sql.Open("sqlite", "file:"+path+"?mode=rw&_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
			testutil.Must(t, err)
			defer db.Close()
			db.SetMaxOpenConns(1)
			next, err := OpenNextRecoveryLedger(ctx, db, l.producer, before, ch.Identity, ch.Operation)
			testutil.Must(t, err)
			if _, err = next.PendingResults(ctx); err == nil {
				t.Fatal("crash enabled old results")
			}
			_, err = next.CaptureRecovery(ctx, ch)
			testutil.Must(t, err)
			page, err := next.RecoveryPage(ctx, ch, 0)
			testutil.Must(t, err)
			if len(page.Records) != 1 || page.Records[0].HistoricalIdentity == nil || *page.Records[0].HistoricalIdentity != l.identity {
				t.Fatal("crash lost original result epoch")
			}
		})
	}
}
