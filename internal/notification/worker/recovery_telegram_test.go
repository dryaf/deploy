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
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

type telegramHistoryFixture struct {
	Path, History, Snapshot, Account          string
	Config                                    Config
	Original, Executed, Confirmation, Pending interaction.Inbound
	Command                                   interaction.Command
}

func newTelegramHistoryFixture(t *testing.T) telegramHistoryFixture {
	t.Helper()
	s, now, path, in := commandFixture(t)
	ctx := context.Background()
	*now = time.Now().UTC()
	in.SentAt = *now
	f := telegramHistoryFixture{Path: path, Account: in.Account, Config: s.config}
	add := func(id int64, text string, convert bool) interaction.Inbound {
		in.NativeID = fmt.Sprintf("update-%d", id)
		in.Text = text
		must(t, s.PersistTelegram(ctx, provider.IntakePage{Account: in.Account, Next: id + 1, Events: []provider.IntakeEvent{{ID: id, Hash: protocol.Hash(in), Inbound: &in}}}))
		if convert {
			must(t, s.ConvertIntake(ctx))
		}
		return in
	}
	f.Original = add(5, "backup/storage status", true)
	f.Snapshot = filepath.Join(t.TempDir(), "restore")
	_, err := s.Snapshot(ctx, f.Snapshot)
	must(t, err)
	f.Executed = add(50, "backup/storage accept-risk subject=a target=finding-one", true)
	v := getCommand(t, s, f.Executed.CommandID())
	f.Command = v.Command
	must(t, s.ImportCommand(ctx, producer, v.Command.ID, protocol.Hash(v.Command)))
	_, err = s.SubmitCommandResult(ctx, producer, interaction.Result{CommandID: v.Command.ID, CommandHash: protocol.Hash(v.Command), Revision: 1, Phase: "prepare", State: "needs_confirmation", Scope: v.Command.Scope, Preparation: &interaction.Preparation{Hash: protocol.Hash("proposal"), Summary: "Accept A", ExpiresAt: now.Add(5 * time.Minute)}})
	must(t, err)
	replies, err := s.PendingCommandReplies(ctx)
	must(t, err)
	var confirm string
	for _, reply := range replies {
		if _, tail, ok := strings.Cut(reply.Text, "\nconfirm "); ok {
			confirm = "confirm " + tail
		}
	}
	if confirm == "" {
		t.Fatal("missing fixture confirmation")
	}
	f.Confirmation = add(51, confirm, true)
	_, err = s.GrantCommand(ctx, producer, v.Command.ID)
	must(t, err)
	_, err = s.SubmitCommandResult(ctx, producer, interaction.Result{CommandID: v.Command.ID, CommandHash: protocol.Hash(v.Command), Revision: 2, Phase: "execute", State: "succeeded", Scope: v.Command.Scope, Text: "Accepted after snapshot"})
	must(t, err)
	add(70, "help", true)
	in.NativeID = "update-95"
	in.Text = "private text discarded"
	in.Actor = "unauthorized"
	must(t, s.PersistTelegram(ctx, provider.IntakePage{Account: in.Account, Next: 96, Events: []provider.IntakeEvent{{ID: 95, Hash: protocol.Hash(in), Inbound: &in}}}))
	in.Actor = f.Original.Actor
	add(300, "unrecognized sensitive command", false)
	permissions := s.config.Interaction.Permissions
	s.config.Interaction.Permissions = nil
	must(t, s.ConvertIntake(ctx))
	s.config.Interaction.Permissions = permissions
	f.Pending = add(400, "backup/storage retry subject=a target=incident-one", false)
	f.History = filepath.Join(t.TempDir(), "history")
	_, err = s.Snapshot(ctx, f.History)
	must(t, err)
	must(t, s.Close())
	must(t, PrepareRecoveryQuarantine(ctx, path, f.Snapshot, f.Config))
	return f
}
func TestTelegramHistoryRecoveryPreservesMappingsWithoutExecution(t *testing.T) {
	f := newTelegramHistoryFixture(t)
	ctx := context.Background()
	summary, err := ReconcileTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config)
	must(t, err)
	if summary.Records != 7 || summary.Mapped != 4 || summary.Rejected != 2 || summary.Unmapped != 1 ||
		summary.RestoredOffset != 6 || summary.RetainedOffset != 401 || summary.IntakeEnabled || summary.HistoryComplete {
		t.Fatal(summary)
	}
	r, err := openRecoveryReconciler(ctx, f.Path, f.Config, nil)
	must(t, err)
	must(t, r.transaction(ctx, func(tx *sql.Tx) error {
		for _, in := range []interaction.Inbound{f.Executed, f.Confirmation} {
			var id string
			if err := tx.QueryRow("SELECT command_id FROM command_inbox WHERE request_key=?", in.RequestKey()).Scan(&id); err != nil {
				return err
			}
			if id != f.Command.ID {
				t.Fatal("confirmation identity reminted")
			}
		}
		v, err := readCommand(tx, f.Command.ID)
		if err != nil {
			return err
		}
		if protocol.Hash(v.Command) != protocol.Hash(f.Command) || v.State != "outcome_unknown" || v.Grant != nil || v.Prepared != nil {
			t.Fatal("command changed or authorized", v)
		}
		var n int
		for _, query := range []string{
			"SELECT count(*) FROM command_inbox WHERE request_key='" + f.Pending.RequestKey() + "'",
			"SELECT count(*) FROM commands WHERE confirmed<>0 OR grant<>'' OR token_hash IS NOT NULL",
			"SELECT count(*) FROM command_results",
			"SELECT count(*) FROM command_replies WHERE state IN ('pending','sending')",
			"SELECT count(*) FROM provider_inbox WHERE state<>'recovery_quarantined'",
		} {
			if err = tx.QueryRow(query).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				t.Fatal("recovery created authority", query, n)
			}
		}
		var offset int64
		if err = tx.QueryRow("SELECT offset FROM telegram_cursors WHERE account=?", f.Account).Scan(&offset); err != nil {
			return err
		}
		if offset != 6 {
			t.Fatal("operational offset advanced")
		}
		var body string
		if err = tx.QueryRow("SELECT body FROM provider_inbox WHERE account=? AND id=95", f.Account).Scan(&body); err != nil {
			return err
		}
		if body != "" {
			t.Fatal("discarded text reconstructed")
		}
		return nil
	}))
	must(t, r.Close())
	if s, err := Open(f.Path, f.Config, false); err == nil {
		s.Close()
		t.Fatal("normal worker started")
	}
	again, err := ReconcileTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config)
	must(t, err)
	if protocol.Hash(again) != protocol.Hash(summary) {
		t.Fatal("completion changed on reopen")
	}
}
func TestTelegramHistoryRecoveryLostResponses(t *testing.T) {
	for _, boundary := range []string{"telegram_import_commit", "telegram_finish_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f := newTelegramHistoryFixture(t)
			ctx := context.Background()
			_, err := reconcileTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config, func(stage string) error {
				if stage == boundary {
					return errors.New("lost response")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault not reached")
			}
			summary, err := ReconcileTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config)
			must(t, err)
			if summary.Mapped != 4 || summary.Unmapped != 1 || summary.IntakeEnabled {
				t.Fatal(summary)
			}
		})
	}
}
func TestTelegramHistoryRecoveryRejectsLostAndConflictingEvidence(t *testing.T) {
	for _, kind := range []string{"wrong_account", "wrong_epoch", "checksum", "restored_hash", "lost_mapping", "lost_schema", "changed_source"} {
		t.Run(kind, func(t *testing.T) {
			f := newTelegramHistoryFixture(t)
			ctx := context.Background()
			switch kind {
			case "wrong_account":
				f.Account = "other"
			case "wrong_epoch":
				s, _, _, _ := commandFixture(t)
				s.config = f.Config
				must(t, s.PersistTelegram(ctx, provider.IntakePage{Account: f.Account}))
				f.History = filepath.Join(t.TempDir(), "foreign")
				_, err := s.Snapshot(ctx, f.History)
				must(t, err)
				must(t, s.Close())
			case "checksum":
				file, err := os.OpenFile(filepath.Join(f.History, "state.db"), os.O_WRONLY, 0)
				must(t, err)
				_, err = file.WriteAt([]byte("changed"), 100)
				must(t, err)
				must(t, file.Close())
			case "restored_hash":
				r, err := openRecoveryReconciler(ctx, f.Path, f.Config, nil)
				must(t, err)
				_, err = r.db.Exec("UPDATE provider_inbox SET hash=? WHERE id=5", protocol.Hash("changed"))
				must(t, err)
				must(t, r.Close())
			case "lost_mapping":
				m, records, err := readTelegramRecovery(ctx, f.History, f.Account, f.Config)
				must(t, err)
				records[0].Mapping = nil
				if records[0].validate(m.Account, m.Offset) == nil {
					t.Fatal("converted request without mapping accepted")
				}
				return
			case "lost_schema", "changed_source":
				_, err := ReconcileTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config)
				must(t, err)
				if kind == "lost_schema" {
					r, err := openRecoveryReconciler(ctx, f.Path, f.Config, nil)
					must(t, err)
					_, err = r.db.Exec("DROP TABLE reconciliation_telegram_records")
					must(t, err)
					must(t, r.Close())
				} else {
					f.History = f.Snapshot
				}
			}
			if _, err := ReconcileTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config); err == nil {
				t.Fatal("invalid retained history accepted", kind)
			}
		})
	}
}
func TestTelegramHistoryRecoveryPageAndDigestGuards(t *testing.T) {
	f := newTelegramHistoryFixture(t)
	ctx := context.Background()
	r, err := openRecoveryReconciler(ctx, f.Path, f.Config, nil)
	must(t, err)
	defer r.Close()
	m, records, err := readTelegramRecovery(ctx, f.History, f.Account, f.Config)
	must(t, err)
	must(t, r.initializeTelegramRecovery(ctx))
	must(t, r.beginTelegramRecovery(ctx, m, records))
	if _, err = r.finishTelegramRecovery(ctx, m, nil); err == nil {
		t.Fatal("missing evidence completed")
	}
	bad := append([]telegramRecoveryRecord(nil), records...)
	bad[len(bad)-1].Hash = "bad"
	if err = r.importTelegramRecovery(ctx, m, 0, bad, nil); err == nil {
		t.Fatal("invalid page accepted")
	}
	var count int
	must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_telegram_records").Scan(&count))
	if count != 0 {
		t.Fatal("partial page committed")
	}
	must(t, r.importTelegramRecovery(ctx, m, 0, records, nil))
	_, err = r.db.Exec("UPDATE reconciliation_telegram_records SET body=json_set(body,'$.State','pending') WHERE ordinal=1")
	must(t, err)
	if _, err = r.finishTelegramRecovery(ctx, m, nil); err == nil {
		t.Fatal("changed evidence completed")
	}
}

func TestTelegramHistoryRecoveryCrashChild(t *testing.T) {
	input := os.Getenv("TELEGRAM_HISTORY_CRASH_INPUT")
	if input == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(input)
	must(t, err)
	var f telegramHistoryFixture
	must(t, json.Unmarshal(raw, &f))
	_, err = reconcileTelegramHistory(context.Background(), f.Path, f.History, f.Account, f.Config, func(stage string) error {
		if stage == os.Getenv("TELEGRAM_HISTORY_CRASH_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	})
	must(t, err)
	t.Fatal("fault not reached")
}
func TestTelegramHistoryRecoverySIGKILL(t *testing.T) {
	for _, stage := range []string{"telegram_import_transaction", "telegram_import_commit", "telegram_finish_transaction", "telegram_finish_commit"} {
		t.Run(stage, func(t *testing.T) {
			f := newTelegramHistoryFixture(t)
			raw, err := json.Marshal(f)
			must(t, err)
			path := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(path, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTelegramHistoryRecoveryCrashChild$")
			cmd.Env = append(os.Environ(), "TELEGRAM_HISTORY_CRASH_INPUT="+path, "TELEGRAM_HISTORY_CRASH_STAGE="+stage)
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
				t.Fatal("child failed", stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			r, err := openRecoveryReconciler(ctx, f.Path, f.Config, nil)
			must(t, err)
			var count int
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_telegram_records").Scan(&count))
			if stage == "telegram_import_transaction" {
				if count != 0 {
					t.Fatal("partial import survived", count)
				}
			} else if count != 7 {
				t.Fatal("committed page lost", count)
			}
			must(t, r.transaction(ctx, func(tx *sql.Tx) error {
				s, err := readTelegramSession(tx, f.Account)
				if err != nil {
					return err
				}
				if (s.Summary != nil) != (stage == "telegram_finish_commit") {
					t.Fatal("completion atomicity violated")
				}
				return nil
			}))
			must(t, r.Close())
			summary, err := ReconcileTelegramHistory(ctx, f.Path, f.History, f.Account, f.Config)
			must(t, err)
			if summary.Mapped != 4 || summary.Unmapped != 1 || summary.IntakeEnabled {
				t.Fatal(summary)
			}
		})
	}
}

func TestRecoveryCapacityCountsUncheckpointedPages(t *testing.T) {
	f := newTelegramHistoryFixture(t)
	ctx := context.Background()
	r, err := openRecoveryReconciler(ctx, f.Path, f.Config, nil)
	must(t, err)
	defer r.Close()
	_, err = r.db.Exec("PRAGMA journal_mode=WAL;PRAGMA wal_autocheckpoint=0;CREATE TABLE quota_probe(body BLOB);INSERT INTO quota_probe VALUES(zeroblob(1048576));")
	must(t, err)
	var pages, size int64
	must(t, r.db.QueryRow("PRAGMA page_count").Scan(&pages))
	must(t, r.db.QueryRow("PRAGMA page_size").Scan(&size))
	file, err := r.stage.Open("reconciled.db", os.O_RDONLY, 0)
	must(t, err)
	info, err := file.Stat()
	must(t, err)
	must(t, file.Close())
	if info.Size() >= pages*size {
		t.Fatal("fixture has no uncheckpointed growth")
	}
	r.config.MaxDatabaseBytes = pages * size
	if err = r.transaction(ctx, func(tx *sql.Tx) error { return r.capacity(tx, 1) }); err == nil {
		t.Fatal("uncheckpointed pages omitted from recovery quota")
	}
}

func TestTelegramHistoryRecoveryEmptyAndMultiplePages(t *testing.T) {
	for _, count := range []int{0, 200} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, _, path, in := commandFixture(t)
			ctx := context.Background()
			must(t, s.PersistTelegram(ctx, provider.IntakePage{Account: in.Account}))
			restore := filepath.Join(t.TempDir(), "restore")
			_, err := s.Snapshot(ctx, restore)
			must(t, err)
			for start := 0; start < count; start += 16 {
				page := provider.IntakePage{Account: in.Account}
				for i := start; i < count && i < start+16; i++ {
					id := int64(i * 17)
					page.Events = append(page.Events, provider.IntakeEvent{ID: id, Hash: protocol.Hash(id), Rejection: "unsupported_event"})
					page.Next = id + 1
				}
				must(t, s.PersistTelegram(ctx, page))
			}
			history := filepath.Join(t.TempDir(), "history")
			_, err = s.Snapshot(ctx, history)
			must(t, err)
			c := s.config
			must(t, s.Close())
			must(t, PrepareRecoveryQuarantine(ctx, path, restore, c))
			pages := 0
			summary, err := reconcileTelegramHistory(ctx, path, history, in.Account, c, func(stage string) error {
				if stage == "telegram_import_commit" {
					pages++
				}
				return nil
			})
			must(t, err)
			if summary.Records != uint64(count) || summary.Rejected != uint64(count) || summary.HistoryComplete || summary.IntakeEnabled {
				t.Fatal(summary)
			}
			if (count == 0 && pages != 0) || (count == 200 && pages != 2) {
				t.Fatal("unexpected page count", pages)
			}
		})
	}
}
