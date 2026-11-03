package worker

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type commandRecoveryFixture struct {
	r              *recoveryReconciler
	ledger         *interaction.Ledger
	db             *sql.DB
	first, pending interaction.Command
	grant          interaction.Grant
	path           string
}

func newCommandRecoveryFixture(t *testing.T, running bool) commandRecoveryFixture {
	t.Helper()
	return newCommandRecoverySnapshotFixture(t, running, false)
}
func newCommandRecoverySnapshotFixture(t *testing.T, running, afterResult bool) commandRecoveryFixture {
	t.Helper()
	ctx := context.Background()
	s, now, path, in := commandFixture(t)
	*now = time.Now().UTC()
	in.SentAt = *now
	in.Text = "backup/storage retry subject=a target=incident-one"
	first, err := s.AdmitCommand(ctx, in)
	must(t, err)
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "specialist.db")+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	must(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ledger, err := interaction.OpenDomainLedger(ctx, db, producer, s.Identity(), true, func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE domain_calls(value INTEGER);INSERT INTO domain_calls VALUES(0)")
		return err
	})
	must(t, err)
	must(t, ledger.Import(ctx, first.Command))
	must(t, s.ImportCommand(ctx, producer, first.Command.ID, protocol.Hash(first.Command)))
	g, err := s.GrantCommand(ctx, producer, first.Command.ID)
	must(t, err)
	if g.ID == "" {
		t.Fatal("grant missing")
	}
	in.NativeID = "missing-specialist"
	in.Text = "backup/storage status"
	_, err = s.AdmitCommand(ctx, in)
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	if !afterResult {
		_, err = s.Snapshot(ctx, artifact)
		must(t, err)
	}
	result, err := ledger.Execute(ctx, first.Command, g, "", *now, func(tx *sql.Tx, c interaction.Command) (interaction.Result, error) {
		_, err := tx.Exec("UPDATE domain_calls SET value=value+1")
		r := interaction.Result{State: "succeeded", Scope: c.Scope, Text: "Applied once"}
		if running {
			r.State = "running"
			r.JobID = "durable-job"
		}
		return r, err
	})
	must(t, err)
	receipt, err := s.SubmitCommandResult(ctx, producer, result)
	must(t, err)
	must(t, ledger.ImportReceipt(ctx, receipt))
	must(t, s.AckCommandResult(ctx, producer, receipt))
	must(t, ledger.Acknowledged(ctx, receipt))
	if afterResult {
		_, err = s.Snapshot(ctx, artifact)
		must(t, err)
	}
	in.NativeID = "post-snapshot"
	in.Text = "backup/storage accept-risk subject=a target=finding-one"
	pending, err := s.AdmitCommand(ctx, in)
	must(t, err)
	must(t, ledger.Import(ctx, pending.Command))
	_, err = ledger.Prepare(ctx, pending.Command, interaction.Preparation{Hash: protocol.Hash("proposal"), Summary: "Review exact finding", ExpiresAt: now.Add(5 * time.Minute)}, *now)
	must(t, err)
	c := s.config
	must(t, s.Close())
	must(t, PrepareRecoveryQuarantine(ctx, path, artifact, c))
	session, err := OpenCommandRecoverySession(ctx, path, c)
	must(t, err)
	r := session.(*recoveryReconciler)
	t.Cleanup(func() { r.Close() })
	return commandRecoveryFixture{r, ledger, db, first.Command, pending.Command, g, path}
}
func TestCommandRecoveryRetainsExecutedOutcomesAndOriginalRequests(t *testing.T) {
	f := newCommandRecoveryFixture(t, false)
	ctx := context.Background()
	summary, err := interaction.ReconcileCommands(ctx, f.ledger, f.r)
	must(t, err)
	if summary.Terminal != 1 || summary.Uncertain != 1 || summary.MissingRestored != 1 || summary.ExecutionEnabled || summary.Manifest.Commands != 2 || summary.Manifest.Results != 2 {
		t.Fatal(summary)
	}
	var id, input, state, grant, preparation string
	var confirmed, results int
	must(t, f.r.db.QueryRow("SELECT command_id,input_hash FROM command_inbox WHERE request_key=?", f.pending.RequestKey).Scan(&id, &input))
	if id != f.pending.ID || input != f.pending.InputHash {
		t.Fatal("request was remapped")
	}
	must(t, f.r.db.QueryRow("SELECT state,grant,preparation,confirmed FROM commands WHERE id=?", f.pending.ID).Scan(&state, &grant, &preparation, &confirmed))
	if state != "outcome_unknown" || grant != "" || preparation != "" || confirmed != 0 {
		t.Fatal("pending confirmation revived")
	}
	must(t, f.r.db.QueryRow("SELECT count(*) FROM command_results").Scan(&results))
	if results != 0 {
		t.Fatal("old result fabricated new receipt")
	}
	var body string
	must(t, f.r.db.QueryRow("SELECT body FROM reconciliation_command_records WHERE id=?", f.first.ID).Scan(&body))
	var retained interaction.CommandRecoveryRecord
	must(t, protocol.Decode([]byte(body), &retained, protocol.MaxRequest))
	if len(retained.Results) != 1 || retained.Results[0].Receipt == nil || !retained.Results[0].Acknowledged || retained.Results[0].Receipt.Identity != f.grant.Identity {
		t.Fatal("lost old outcome provenance", retained)
	}
	_, err = f.ledger.Execute(ctx, f.first, f.grant, "", time.Now(), func(*sql.Tx, interaction.Command) (interaction.Result, error) {
		t.Fatal("execution repeated")
		return interaction.Result{}, nil
	})
	if err == nil {
		t.Fatal("old handle bypassed recovery fence")
	}
	if _, err = f.ledger.Prepare(ctx, f.pending, interaction.Preparation{}, time.Now()); err == nil {
		t.Fatal("confirmation revived")
	}
	if _, err = f.ledger.RejectPreparation(ctx, f.pending, "changed"); err == nil {
		t.Fatal("history changed during recovery")
	}
	if err = f.ledger.Import(ctx, f.pending); err == nil {
		t.Fatal("normal command import allowed")
	}
	if err = f.ledger.ImportReceipt(ctx, *retained.Results[0].Receipt); err == nil {
		t.Fatal("old receipt imported into recovery")
	}
	var calls int
	must(t, f.db.QueryRow("SELECT value FROM domain_calls").Scan(&calls))
	if calls != 1 {
		t.Fatal("domain operation repeated", calls)
	}
	firstHash := protocol.Hash(summary)
	must(t, f.r.Close())
	session, err := OpenCommandRecoverySession(ctx, f.path, f.r.config)
	must(t, err)
	defer session.Close()
	summary, err = interaction.ReconcileCommands(ctx, f.ledger, session)
	must(t, err)
	if protocol.Hash(summary) != firstHash {
		t.Fatal("restart changed completion")
	}
	if s, err := Open(f.path, f.r.config, false); err == nil {
		s.Close()
		t.Fatal("normal gateway opened")
	}
}
func TestCommandRecoveryKeepsAdmittedJobsUncertain(t *testing.T) {
	f := newCommandRecoveryFixture(t, true)
	summary, err := interaction.ReconcileCommands(context.Background(), f.ledger, f.r)
	must(t, err)
	if summary.Terminal != 0 || summary.Uncertain != 2 {
		t.Fatal(summary)
	}
	_, err = f.ledger.CompleteJob(context.Background(), f.first.ID, "durable-job", func(*sql.Tx) (interaction.Result, error) {
		t.Fatal("completion callback ran through recovery fence")
		return interaction.Result{}, nil
	})
	if err == nil {
		t.Fatal("normal job completion mutated frozen ledger")
	}
}

type lostCommandRecovery struct {
	interaction.CommandRecoverySession
	op   string
	lost bool
}

func (s *lostCommandRecovery) ImportCommands(ctx context.Context, ch protocol.RecoveryChallenge, after uint64, records []interaction.CommandRecoveryRecord) error {
	err := s.CommandRecoverySession.ImportCommands(ctx, ch, after, records)
	if err == nil && !s.lost && s.op == "import" {
		s.lost = true
		return errors.New("lost import response")
	}
	return err
}
func (s *lostCommandRecovery) FinishCommands(ctx context.Context, ch protocol.RecoveryChallenge) (interaction.CommandRecoverySummary, error) {
	v, err := s.CommandRecoverySession.FinishCommands(ctx, ch)
	if err == nil && !s.lost && s.op == "finish" {
		s.lost = true
		return v, errors.New("lost finish response")
	}
	return v, err
}
func TestCommandRecoveryLostResponses(t *testing.T) {
	for _, op := range []string{"import", "finish"} {
		t.Run(op, func(t *testing.T) {
			f := newCommandRecoveryFixture(t, false)
			broken := &lostCommandRecovery{CommandRecoverySession: f.r, op: op}
			if _, err := interaction.ReconcileCommands(context.Background(), f.ledger, broken); err == nil {
				t.Fatal("missing injected loss")
			}
			summary, err := interaction.ReconcileCommands(context.Background(), f.ledger, f.r)
			must(t, err)
			if summary.Terminal != 1 || summary.Uncertain != 1 {
				t.Fatal(summary)
			}
		})
	}
}
func TestCommandRecoveryRejectsConflictsAndIncompleteHistory(t *testing.T) {
	f := newCommandRecoveryFixture(t, false)
	ctx := context.Background()
	ch, err := f.r.CommandChallenge(ctx, producer)
	must(t, err)
	m, err := f.ledger.CaptureRecovery(ctx, ch)
	must(t, err)
	must(t, f.r.BeginCommands(ctx, ch, m))
	page, err := f.ledger.RecoveryPage(ctx, ch, 0)
	must(t, err)
	if len(page.Records) != 2 {
		t.Fatal(page)
	}
	if _, err = f.r.FinishCommands(ctx, ch); err == nil {
		t.Fatal("missing history completed")
	}
	bad := ch
	bad.Nonce = "changed"
	if err = f.r.ImportCommands(ctx, bad, 0, page.Records); err == nil {
		t.Fatal("changed nonce accepted")
	}
	before := protocol.Hash(page.Records)
	must(t, f.r.ImportCommands(ctx, ch, 0, page.Records))
	page.Records[0].Command.InputHash = "changed"
	if err = f.r.ImportCommands(ctx, ch, 0, page.Records); err == nil {
		t.Fatal("changed evidence replay accepted")
	}
	page, err = f.ledger.RecoveryPage(ctx, ch, 0)
	must(t, err)
	if protocol.Hash(page.Records) != before {
		t.Fatal("immutable specialist evidence changed")
	}
	must(t, f.r.ImportCommands(ctx, ch, 0, page.Records))
	_, err = f.r.FinishCommands(ctx, ch)
	must(t, err)
	_, err = f.r.db.Exec("DROP TABLE reconciliation_command_records")
	must(t, err)
	must(t, f.r.Close())
	if session, err := OpenCommandRecoverySession(ctx, f.path, f.r.config); err == nil {
		session.Close()
		t.Fatal("missing evidence silently recreated")
	}
}

type commandRecoveryCrashInput struct {
	WorkerPath, LedgerPath string
	Config                 Config
	Identity               protocol.Identity
}
type crashingCommandSession struct{ *recoveryReconciler }

func (s crashingCommandSession) ImportCommands(ctx context.Context, ch protocol.RecoveryChallenge, after uint64, records []interaction.CommandRecoveryRecord) error {
	s.fault = func(stage string) error {
		if stage == os.Getenv("DEPLOY_COMMAND_IMPORT_BOUNDARY") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	return s.recoveryReconciler.ImportCommands(ctx, ch, after, records)
}
func TestCommandRecoveryImportCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_COMMAND_IMPORT_INPUT")
	if path == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	var input commandRecoveryCrashInput
	must(t, json.Unmarshal(raw, &input))
	session, err := OpenCommandRecoverySession(context.Background(), input.WorkerPath, input.Config)
	must(t, err)
	defer session.Close()
	db, err := sql.Open("sqlite", "file:"+input.LedgerPath+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
	must(t, err)
	defer db.Close()
	ledger, err := interaction.OpenLedger(context.Background(), db, producer, input.Identity, false)
	must(t, err)
	_, err = interaction.ReconcileCommands(context.Background(), ledger, crashingCommandSession{session.(*recoveryReconciler)})
	must(t, err)
	t.Fatal("boundary not reached")
}
func TestCommandRecoveryImportSIGKILL(t *testing.T) {
	for _, boundary := range []string{"reconciliation_transaction", "reconciliation_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f := newCommandRecoveryFixture(t, false)
			var seq int
			var name, dbPath string
			must(t, f.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &dbPath))
			input := commandRecoveryCrashInput{WorkerPath: f.path, LedgerPath: dbPath, Config: f.r.config, Identity: f.grant.Identity}
			must(t, f.r.Close())
			must(t, f.db.Close())
			path := filepath.Join(t.TempDir(), "input.json")
			raw, err := json.Marshal(input)
			must(t, err)
			must(t, os.WriteFile(path, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCommandRecoveryImportCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_COMMAND_IMPORT_INPUT="+path, "DEPLOY_COMMAND_IMPORT_BOUNDARY="+boundary)
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
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("child failed", scanner.Text(), stderr.String())
			}
			must(t, cmd.Process.Kill())
			if err = cmd.Wait(); err == nil {
				t.Fatal("expected SIGKILL")
			}
			session, err := OpenCommandRecoverySession(ctx, input.WorkerPath, input.Config)
			must(t, err)
			defer session.Close()
			r := session.(*recoveryReconciler)
			var count int
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_command_records").Scan(&count))
			if (count == 2) != (boundary == "reconciliation_commit") || (count != 0 && count != 2) {
				t.Fatal("partial page survived", count)
			}
			db, err := sql.Open("sqlite", "file:"+input.LedgerPath+"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)")
			must(t, err)
			defer db.Close()
			ledger, err := interaction.OpenLedger(ctx, db, producer, input.Identity, false)
			must(t, err)
			summary, err := interaction.ReconcileCommands(ctx, ledger, session)
			must(t, err)
			if summary.Terminal != 1 || summary.Uncertain != 1 || summary.ExecutionEnabled {
				t.Fatal(summary)
			}
			var calls int
			must(t, db.QueryRow("SELECT value FROM domain_calls").Scan(&calls))
			if calls != 1 {
				t.Fatal("domain effect repeated", calls)
			}
		})
	}
}
