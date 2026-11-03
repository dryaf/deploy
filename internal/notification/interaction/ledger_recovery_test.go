package interaction

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func recoveryChallenge(t *testing.T, l *Ledger) protocol.RecoveryChallenge {
	t.Helper()
	operation, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	boot, up, err := process.HostClock()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return protocol.RecoveryChallenge{Operation: operation, Nonce: nonce, Identity: protocol.Identity{Worker: l.identity.Worker, Recovery: "new-epoch"}, Producer: l.producer, IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute), BootID: boot, Uptime: up}
}
func TestCommandLedgerRecoveryFencesEveryRuntimeWriter(t *testing.T) {
	l, db, c, g, _ := ledgerFixture(t)
	defer db.Close()
	ctx := context.Background()
	ch := recoveryChallenge(t, l)
	m, err := l.CaptureRecovery(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	if m.Commands != 1 || m.Results != 0 {
		t.Fatal(m)
	}
	other, err := OpenLedger(ctx, db, l.producer, l.identity, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, handle := range []*Ledger{l, other} {
		if err = handle.Import(ctx, c); err == nil {
			t.Fatal("import passed fence")
		}
		if _, err = handle.Execute(ctx, c, g, "", time.Now(), func(*sql.Tx, Command) (Result, error) { t.Fatal("callback ran"); return Result{}, nil }); err == nil {
			t.Fatal("execute passed fence")
		}
		if _, err = handle.Prepare(ctx, c, Preparation{}, time.Now()); err == nil {
			t.Fatal("prepare passed fence")
		}
		if _, err = handle.RejectPreparation(ctx, c, "rejected"); err == nil {
			t.Fatal("rejection passed fence")
		}
		if err = handle.FenceAdmission(ctx, c.ID); err == nil {
			t.Fatal("history changed")
		}
		if _, err = handle.ProveNonAdmission(ctx, c, g); err == nil {
			t.Fatal("new proof passed fence")
		}
		a := ResultReceipt{Identity: l.identity, Producer: l.producer, CommandID: c.ID, Revision: 1}
		if err = handle.ImportReceipt(ctx, a); err == nil {
			t.Fatal("receipt passed fence")
		}
		if err = handle.Acknowledged(ctx, a); err == nil {
			t.Fatal("ack passed fence")
		}
		if _, err = handle.CompleteJob(ctx, c.ID, "job", func(*sql.Tx) (Result, error) { t.Fatal("job callback ran"); return Result{}, nil }); err == nil {
			t.Fatal("job passed fence")
		}
	}
	page, err := l.RecoveryPage(ctx, ch, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !page.Done || len(page.Records) != 1 || protocol.Hash(page.Records[0].Command) != protocol.Hash(c) {
		t.Fatal(page)
	}
	if _, err = OpenLedger(ctx, db, l.producer, ch.Identity, false); err == nil {
		t.Fatal("active identity rotated")
	}
	// Even loss of the session row cannot reopen normal admission.
	if _, err = db.Exec("DELETE FROM interactive_recovery"); err != nil {
		t.Fatal(err)
	}
	if _, err = l.CaptureRecovery(ctx, ch); err == nil {
		t.Fatal("missing session silently recreated")
	}
	if err = l.Import(ctx, c); err == nil {
		t.Fatal("damaged fence reopened ledger")
	}
	if _, err = db.Exec("DROP TABLE interactive_recovery"); err != nil {
		t.Fatal(err)
	}
	if err = l.Import(ctx, c); err == nil {
		t.Fatal("identity pin did not preserve fence after table loss")
	}
	if _, err = OpenLedger(ctx, db, l.producer, l.identity, false); err == nil {
		t.Fatal("missing pinned recovery reopened")
	}
}
func TestCommandLedgerRecoveryRejectsDamagedHistory(t *testing.T) {
	for _, damage := range []string{"command hash", "result gap", "orphan", "foreign epoch"} {
		t.Run(damage, func(t *testing.T) {
			l, db, c, g, _ := ledgerFixture(t)
			defer db.Close()
			ctx := context.Background()
			_, err := l.Execute(ctx, c, g, "", c.IssuedAt, func(*sql.Tx, Command) (Result, error) {
				return Result{State: "succeeded", Scope: c.Scope, Text: "done"}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "command hash":
				_, err = db.Exec("UPDATE interactive_inbox SET hash='changed'")
			case "result gap":
				_, err = db.Exec("DELETE FROM interactive_results")
			case "orphan":
				_, err = db.Exec("INSERT INTO interactive_non_admission VALUES('missing','{}')")
			case "foreign epoch":
				_, err = db.Exec("UPDATE interactive_identity SET body='{}'")
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = l.CaptureRecovery(ctx, recoveryChallenge(t, l)); err == nil {
				t.Fatal("damaged history accepted")
			}
			var count int
			if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='interactive_recovery'").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("failed capture left partial fence")
			}
		})
	}
}
func TestCommandLedgerRecoveryPagesBoundAndRenewal(t *testing.T) {
	l, db, c, _, _ := ledgerFixture(t)
	defer db.Close()
	ctx := context.Background()
	for i := 0; i < 139; i++ {
		copy := c
		copy.ID = fmt.Sprintf("C-%03d", i)
		copy.RequestKey = copy.ID
		copy.Arguments = map[string]string{"reason": string(bytes.Repeat([]byte("界"), 2000))}
		if err := l.Import(ctx, copy); err != nil {
			t.Fatal(err)
		}
	}
	ch := recoveryChallenge(t, l)
	m, err := l.CaptureRecovery(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	var after uint64
	pages := 0
	for {
		page, err := l.RecoveryPage(ctx, ch, after)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(page.Records)
		if len(raw)+4096 > protocol.MaxRequest || len(page.Records) > 128 || page.Next <= after {
			t.Fatal("invalid page bounds")
		}
		for _, v := range page.Records {
			hashes = append(hashes, protocol.Hash(v))
		}
		after = page.Next
		pages++
		if page.Done {
			break
		}
	}
	if after != 140 || pages < 2 || CommandRecoveryDigest(hashes) != m.Digest {
		t.Fatal("export digest/count mismatch")
	}
	newCh := ch
	newCh.Nonce, err = safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	newCh.IssuedAt = time.Now().UTC()
	newCh.ExpiresAt = newCh.IssuedAt.Add(10 * time.Minute)
	got, err := l.CaptureRecovery(ctx, newCh)
	if err != nil {
		t.Fatal(err)
	}
	if got != m {
		t.Fatal("renewal recaptured history")
	}
	if _, err = l.RecoveryPage(ctx, ch, 0); err == nil {
		t.Fatal("old nonce accepted")
	}
	if _, err = db.Exec("UPDATE interactive_recovery_records SET hash='bad' WHERE ordinal=1"); err != nil {
		t.Fatal(err)
	}
	if _, err = l.RecoveryPage(ctx, newCh, 0); err == nil {
		t.Fatal("damaged evidence exported")
	}
}

type commandCaptureInput struct {
	Producer  protocol.Producer
	Identity  protocol.Identity
	Challenge protocol.RecoveryChallenge
}

func TestCommandLedgerRecoveryCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_COMMAND_CAPTURE_DB")
	if path == "" {
		t.Skip("subprocess helper")
	}
	var input commandCaptureInput
	raw, err := os.ReadFile(os.Getenv("DEPLOY_COMMAND_CAPTURE_INPUT"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l, err := OpenLedger(context.Background(), db, input.Producer, input.Identity, false)
	if err != nil {
		t.Fatal(err)
	}
	boot, up, err := process.HostClock()
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.captureRecovery(context.Background(), input.Challenge, time.Now().UTC(), boot, up, func(stage string) error {
		if stage == os.Getenv("DEPLOY_COMMAND_CAPTURE_BOUNDARY") {
			fmt.Println("READY")
			select {}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("boundary missing")
}
func TestCommandLedgerRecoveryCaptureSIGKILL(t *testing.T) {
	for _, boundary := range []string{"command_capture_transaction", "command_capture_commit"} {
		t.Run(boundary, func(t *testing.T) {
			l, db, c, _, path := ledgerFixture(t)
			ch := recoveryChallenge(t, l)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(t.TempDir(), "input.json")
			raw, _ := json.Marshal(commandCaptureInput{l.producer, l.identity, ch})
			if err := os.WriteFile(input, raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandLedgerRecoveryCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_COMMAND_CAPTURE_DB="+path, "DEPLOY_COMMAND_CAPTURE_INPUT="+input, "DEPLOY_COMMAND_CAPTURE_BOUNDARY="+boundary)
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
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("child failed", scanner.Text(), stderr.String())
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = cmd.Wait(); err == nil {
				t.Fatal("expected SIGKILL")
			}
			db, err = sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			l, err = OpenLedger(ctx, db, l.producer, l.identity, false)
			if err != nil {
				t.Fatal(err)
			}
			var tables int
			if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='interactive_recovery'").Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if (tables == 1) != (boundary == "command_capture_commit") {
				t.Fatal("incorrect fence durability", tables)
			}
			m, err := l.CaptureRecovery(ctx, ch)
			if err != nil {
				t.Fatal(err)
			}
			page, err := l.RecoveryPage(ctx, ch, 0)
			if err != nil {
				t.Fatal(err)
			}
			if m.Commands != 1 || !page.Done || page.Records[0].Command.ID != c.ID {
				t.Fatal("recovery lost command")
			}
		})
	}
}
