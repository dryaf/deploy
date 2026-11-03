package interaction

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

type commitGatewayFixture struct {
	CommandRecoveryCommitSession
	proof  CommandRecoveryCommit
	change func(*CommandRecoveryCommit) error
}

func (s commitGatewayFixture) CommandSummary(context.Context, protocol.Producer) (CommandRecoverySummary, error) {
	return s.proof.Summary, nil
}
func (s commitGatewayFixture) PrepareCommandRecoveryCommit(context.Context, protocol.Producer) (CommandRecoveryCommit, error) {
	p := s.proof
	var err error
	if s.change != nil {
		err = s.change(&p)
	}
	return p, err
}

func commandCommitFixture(t *testing.T) (*Ledger, *sql.DB, string, commitGatewayFixture) {
	t.Helper()
	l, db, c, g, path := ledgerFixture(t)
	ctx := context.Background()
	result, err := l.Execute(ctx, c, g, "", c.IssuedAt, func(*sql.Tx, Command) (Result, error) {
		return Result{State: "succeeded", Scope: c.Scope, Text: "Applied once"}, nil
	})
	testutil.Must(t, err)
	testutil.Must(t, l.ImportReceipt(ctx, ResultReceipt{Identity: l.identity, Producer: l.producer, CommandID: c.ID, Revision: result.Revision, Hash: protocol.Hash(result), Disposition: "applied"}))
	ch := recoveryChallenge(t, l)
	manifest, err := l.CaptureRecovery(ctx, ch)
	testutil.Must(t, err)
	page, err := l.RecoveryPage(ctx, ch, 0)
	testutil.Must(t, err)
	summary := CommandRecoverySummary{Challenge: ch, Manifest: manifest, Terminal: 1}
	testutil.Must(t, l.recordRecoverySummary(ctx, summary))
	proof := CommandRecoveryCommit{Version: 1, Summary: summary, ResultFloorsHash: CommandRecoveryDigest([]string{protocol.Hash(RecoveryResultFloor(page.Records[0]))})}
	return l, db, path, commitGatewayFixture{proof: proof}
}

func TestCommandLedgerCommitLostResponses(t *testing.T) {
	for _, boundary := range []string{"gateway", "command_commit_intent_transaction", "command_commit_intent", "command_commit_gateway_prepared", "command_commit_transaction", "command_commit"} {
		t.Run(boundary, func(t *testing.T) {
			l, db, _, gateway := commandCommitFixture(t)
			failing := gateway
			if boundary == "gateway" {
				failing.change = func(*CommandRecoveryCommit) error { return errors.New("lost proof response") }
			}
			_, err := commitCommandRecovery(context.Background(), l, failing, func(at string) error {
				if at == boundary {
					return errors.New("interrupted commit")
				}
				return nil
			})
			if err == nil {
				t.Fatal("injection ignored")
			}
			var raw string
			testutil.Must(t, db.QueryRow("SELECT body FROM interactive_identity").Scan(&raw))
			var pin ledgerIdentityPin
			testutil.Must(t, json.Unmarshal([]byte(raw), &pin))
			want := l.identity
			if boundary == "command_commit" {
				want = gateway.proof.Summary.Challenge.Identity
			}
			if pin.Identity != want {
				t.Fatal("partial identity commit")
			}
			got, err := CommitCommandRecovery(context.Background(), l, gateway)
			testutil.Must(t, err)
			if protocol.Hash(got) != protocol.Hash(gateway.proof) {
				t.Fatal("resumed proof changed")
			}
		})
	}
}

func TestCommandLedgerCommitRejectsChangedAuthority(t *testing.T) {
	for _, mode := range []string{"summary", "records", "live_command", "live_result", "live_receipt", "live_ack", "extra_result", "pin", "gateway_floor", "gateway_epoch", "gateway_activation", "midflight_pin", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			l, db, _, gateway := commandCommitFixture(t)
			var err error
			switch mode {
			case "summary":
				_, err = db.Exec("DELETE FROM interactive_recovery_completion")
			case "records":
				_, err = db.Exec("UPDATE interactive_recovery_records SET hash='changed'")
			case "live_command":
				_, err = db.Exec("UPDATE interactive_inbox SET request_key='changed'")
			case "live_result":
				_, err = db.Exec("UPDATE interactive_results SET body=json_set(body,'$.text','changed')")
			case "live_receipt":
				_, err = db.Exec("UPDATE interactive_results SET receipt=''")
			case "live_ack":
				_, err = db.Exec("UPDATE interactive_results SET acked=1")
			case "extra_result":
				_, err = db.Exec("INSERT INTO interactive_results SELECT 'unknown',revision,body,receipt,acked FROM interactive_results")
			case "pin":
				_, err = db.Exec("UPDATE interactive_identity SET body=json_set(body,'$.recovery_operation','changed')")
			}
			testutil.Must(t, err)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			gateway.change = func(p *CommandRecoveryCommit) error {
				switch mode {
				case "gateway_floor":
					p.ResultFloorsHash = protocol.Hash("changed")
				case "gateway_epoch":
					p.Summary.Challenge.Identity.Recovery = "changed"
				case "gateway_activation":
					p.ExecutionEnabled = true
				case "midflight_pin":
					_, err := db.Exec("UPDATE interactive_identity SET body=json_set(body,'$.recovery_operation','changed')")
					testutil.Must(t, err)
				case "cancel":
					cancel()
				}
				return nil
			}
			if _, err = CommitCommandRecovery(ctx, l, gateway); err == nil {
				t.Fatal("changed authority accepted", mode)
			}
			var raw string
			testutil.Must(t, db.QueryRow("SELECT body FROM interactive_identity").Scan(&raw))
			var pin ledgerIdentityPin
			testutil.Must(t, json.Unmarshal([]byte(raw), &pin))
			if pin.Identity != l.identity {
				t.Fatal("failed commit changed epoch")
			}
			if _, err = l.PendingResults(context.Background()); err == nil {
				t.Fatal("failed commit exposed outbox")
			}
		})
	}
}

func TestCommandLedgerCommitRejectsPartialRollback(t *testing.T) {
	for _, mode := range []string{"intent", "proof", "version", "floors", "floor_schema", "floor_revision", "pin", "marker", "record", "receipt", "fence"} {
		t.Run(mode, func(t *testing.T) {
			l, db, _, gateway := commandCommitFixture(t)
			var oldPin string
			testutil.Must(t, db.QueryRow("SELECT body FROM interactive_identity").Scan(&oldPin))
			_, err := CommitCommandRecovery(context.Background(), l, gateway)
			testutil.Must(t, err)
			switch mode {
			case "intent":
				_, err = db.Exec("UPDATE interactive_recovery_commit SET intent='{}'")
			case "proof":
				_, err = db.Exec("UPDATE interactive_recovery_commit SET proof=''")
			case "version":
				_, err = db.Exec("UPDATE interactive_recovery_commit SET version=2")
			case "floors":
				_, err = db.Exec("DELETE FROM interactive_recovery_result_floors")
			case "floor_schema":
				_, err = db.Exec("DROP TABLE interactive_recovery_result_floors")
			case "floor_revision":
				_, err = db.Exec("UPDATE interactive_recovery_result_floors SET revision=0")
			case "pin":
				_, err = db.Exec("UPDATE interactive_identity SET body=?", oldPin)
			case "marker":
				_, err = db.Exec("UPDATE interactive_identity SET body=json_remove(body,'$.recovery_commit')")
			case "record":
				_, err = db.Exec("DELETE FROM interactive_recovery_commit")
			case "receipt":
				_, err = db.Exec("UPDATE interactive_results SET receipt=''")
			case "fence":
				_, err = db.Exec("UPDATE interactive_inbox SET fenced=0")
			}
			testutil.Must(t, err)
			if _, err = CommitCommandRecovery(context.Background(), l, gateway); err == nil {
				t.Fatal("partial rollback accepted")
			}
			if _, err = l.PendingResults(context.Background()); err == nil {
				t.Fatal("partial rollback enabled outbox")
			}
		})
	}
}

func TestCommandLedgerCommitCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_COMMAND_COMMIT_DB")
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(os.Getenv("DEPLOY_COMMAND_COMMIT_INPUT"))
	testutil.Must(t, err)
	var proof CommandRecoveryCommit
	testutil.Must(t, json.Unmarshal(raw, &proof))
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	testutil.Must(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	l, err := OpenLedger(context.Background(), db, proof.Summary.Manifest.Producer, proof.Summary.Manifest.Identity, false)
	testutil.Must(t, err)
	_, err = commitCommandRecovery(context.Background(), l, commitGatewayFixture{proof: proof}, func(at string) error {
		if at == os.Getenv("DEPLOY_COMMAND_COMMIT_BOUNDARY") {
			os.Stdout.WriteString("READY\n")
			select {}
		}
		return nil
	})
	testutil.Must(t, err)
	t.Fatal("boundary not reached")
}

func TestCommandLedgerCommitSIGKILL(t *testing.T) {
	for _, boundary := range []string{"command_commit_intent_transaction", "command_commit_intent", "command_commit_gateway_prepared", "command_commit_transaction", "command_commit"} {
		t.Run(boundary, func(t *testing.T) {
			_, db, path, gateway := commandCommitFixture(t)
			testutil.Must(t, db.Close())
			input := filepath.Join(t.TempDir(), "proof.json")
			raw, err := json.Marshal(gateway.proof)
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(input, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandLedgerCommitCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_COMMAND_COMMIT_DB="+path, "DEPLOY_COMMAND_COMMIT_INPUT="+input, "DEPLOY_COMMAND_COMMIT_BOUNDARY="+boundary)
			pipe, err := cmd.StdoutPipe()
			testutil.Must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			testutil.Must(t, cmd.Start())
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
				t.Fatal("boundary not reached", stderr.String())
			}
			testutil.Must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			reopened, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
			testutil.Must(t, err)
			defer reopened.Close()
			reopened.SetMaxOpenConns(1)
			var pinBody string
			testutil.Must(t, reopened.QueryRow("SELECT body FROM interactive_identity").Scan(&pinBody))
			var pin ledgerIdentityPin
			testutil.Must(t, json.Unmarshal([]byte(pinBody), &pin))
			want := gateway.proof.Summary.Manifest.Identity
			if boundary == "command_commit" {
				want = gateway.proof.Summary.Challenge.Identity
			}
			if pin.Identity != want {
				t.Fatal("incorrect commit durability")
			}
			l, err := OpenLedger(ctx, reopened, pin.Producer, pin.Identity, false)
			testutil.Must(t, err)
			got, err := CommitCommandRecovery(ctx, l, gateway)
			testutil.Must(t, err)
			again, err := CommitCommandRecovery(ctx, l, gateway)
			testutil.Must(t, err)
			if protocol.Hash(got) != protocol.Hash(again) || protocol.Hash(got) != protocol.Hash(gateway.proof) {
				t.Fatal("restart changed checkpoint")
			}
			if _, err = l.PendingResults(ctx); err == nil {
				t.Fatal("restart enabled outbox")
			}
		})
	}
}

func TestCommandLedgerCommitSealsCaptureAndCompletion(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "intent", true: "committed"}[complete], func(t *testing.T) {
			l, db, _, gateway := commandCommitFixture(t)
			_, err := commitCommandRecovery(context.Background(), l, gateway, func(at string) error {
				if !complete && at == "command_commit_intent" {
					return errors.New("interrupted")
				}
				return nil
			})
			if complete {
				testutil.Must(t, err)
			} else if err == nil {
				t.Fatal("fault ignored")
			}
			if _, err = l.CaptureRecovery(context.Background(), gateway.proof.Summary.Challenge); err == nil {
				t.Fatal("capture renewed after commit intent")
			}
			_, err = db.Exec("DROP TABLE interactive_recovery_completion")
			testutil.Must(t, err)
			if err = l.recordRecoverySummary(context.Background(), gateway.proof.Summary); err == nil {
				t.Fatal("missing completion silently recreated")
			}
			var count int
			testutil.Must(t, db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='interactive_recovery_completion'").Scan(&count))
			if count != 0 {
				t.Fatal("failed replay recreated evidence")
			}
		})
	}
}
