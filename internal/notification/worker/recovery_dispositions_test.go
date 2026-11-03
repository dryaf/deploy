package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func dispositionFixture(t *testing.T, action string) (reconcileFixture, RecoveryDisposition) {
	t.Helper()
	f := newReconcileFixture(t)
	ctx := context.Background()
	ch := beginReconciliation(t, f)
	must(t, f.r.Import(ctx, ch, f.records))
	summary, err := f.r.Finish(ctx, ch, snapshot(f.now, 2, 1))
	must(t, err)
	must(t, f.r.initializeDispositions(ctx))
	id, err := safefs.NewID("n")
	must(t, err)
	return f, RecoveryDisposition{
		ID: id, Operation: ch.Operation, Identity: ch.Identity, Producer: producer,
		MessageID: f.records[1].Message.ID, MessageHash: protocol.Hash(f.records[1].Message),
		SummaryHash: protocol.Hash(summary), Action: action, Actor: "local-operator",
		Reason: "Reviewed uncertain delivery", AcceptDuplicateRisk: action == "retry",
	}
}
func TestRecoveryDispositionsPreserveQuarantine(t *testing.T) {
	for _, action := range []string{"retry", "cancel"} {
		t.Run(action, func(t *testing.T) {
			f, decision := dispositionFixture(t, action)
			ctx := context.Background()
			if _, err := f.r.RecoveryMessageDisposition(ctx, producer, decision.MessageID); !errors.Is(err, protocol.ErrRecoveryIncomplete) {
				t.Fatal(err)
			}
			record, err := f.r.DecideRecoveryMessage(ctx, decision)
			must(t, err)
			if record.Decision != decision || record.DeliveryEnabled || !record.RecordedAt.Equal(f.now) {
				t.Fatal(record)
			}
			var state string
			must(t, f.r.db.QueryRow("SELECT state FROM messages WHERE id=?", decision.MessageID).Scan(&state))
			if state != "ambiguous" {
				t.Fatal("decision granted delivery or forged terminal outcome", state)
			}
			var receipts int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts))
			if receipts != 0 {
				t.Fatal("new receipt minted")
			}
			summary, err := f.r.Summary(ctx, producer)
			must(t, err)
			if protocol.Hash(summary) != decision.SummaryHash || summary.Ambiguous != 1 || summary.DeliveryEnabled {
				t.Fatal(summary)
			}
			f.r.now = func() time.Time { return f.now.Add(24 * time.Hour) }
			again, err := f.r.DecideRecoveryMessage(ctx, decision)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(record) {
				t.Fatal("replay changed audit time")
			}
			must(t, f.r.Close())
			session, err := OpenRecoveryDispositionSession(ctx, f.path, configuration())
			must(t, err)
			defer session.Close()
			again, err = session.RecoveryMessageDisposition(ctx, producer, decision.MessageID)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(record) {
				t.Fatal("decision changed on reopen")
			}
			if normal, err := Open(f.path, configuration(), false); err == nil {
				normal.Close()
				t.Fatal("normal service enabled")
			}
		})
	}
}
func TestRecoveryDispositionsRejectInvalidOrConflictingDecisions(t *testing.T) {
	for _, mode := range []string{"epoch", "operation", "producer", "payload", "summary", "actor", "reason", "action", "risk", "accepted", "retained_terminal", "incomplete"} {
		t.Run(mode, func(t *testing.T) {
			f, d := dispositionFixture(t, "retry")
			switch mode {
			case "epoch":
				d.Identity.Recovery = "foreign"
			case "operation":
				d.Operation, _ = safefs.NewID("n")
			case "producer":
				d.Producer.Epoch = "foreign"
			case "payload":
				d.MessageHash = protocol.Hash("changed")
			case "summary":
				d.SummaryHash = protocol.Hash("changed")
			case "actor":
				d.Actor = ""
			case "reason":
				d.Reason = strings.Repeat("x", 1025)
			case "action":
				d.Action = "send"
			case "risk":
				d.AcceptDuplicateRisk = false
			case "accepted", "retained_terminal":
				d.MessageID = f.records[0].Message.ID
				d.MessageHash = protocol.Hash(f.records[0].Message)
				if mode == "retained_terminal" {
					_, err := f.r.db.Exec("UPDATE messages SET state='ambiguous' WHERE id=?", d.MessageID)
					must(t, err)
				}
			case "incomplete":
				_, err := f.r.db.Exec("UPDATE reconciliation_producers SET completion=''")
				must(t, err)
			}
			if _, err := f.r.DecideRecoveryMessage(context.Background(), d); err == nil {
				t.Fatal("invalid decision accepted")
			}
			var n int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_dispositions").Scan(&n))
			if n != 0 {
				t.Fatal("invalid decision committed")
			}
		})
	}
	f, d := dispositionFixture(t, "retry")
	ctx := context.Background()
	_, err := f.r.DecideRecoveryMessage(ctx, d)
	must(t, err)
	for _, mode := range []string{"id", "action", "reason", "actor"} {
		changed := d
		switch mode {
		case "id":
			changed.ID, _ = safefs.NewID("n")
		case "action":
			changed.Action, changed.AcceptDuplicateRisk = "cancel", false
		case "reason":
			changed.Reason = "changed"
		case "actor":
			changed.Actor = "other-operator"
		}
		if _, err = f.r.DecideRecoveryMessage(ctx, changed); err == nil {
			t.Fatal("immutable decision changed", mode)
		}
	}
}
func TestRecoveryDispositionsRejectLostEvidence(t *testing.T) {
	for _, mode := range []string{"file_missing", "file_corrupt", "row_corrupt", "table", "summary", "payload", "evidence"} {
		t.Run(mode, func(t *testing.T) {
			f, d := dispositionFixture(t, "cancel")
			ctx := context.Background()
			_, err := f.r.DecideRecoveryMessage(ctx, d)
			must(t, err)
			name := filepath.Join(f.r.stage.File().Name(), dispositionFile(d.Producer, d.MessageID))
			switch mode {
			case "file_missing":
				err = os.Remove(name)
			case "file_corrupt":
				err = os.WriteFile(name, []byte("{"), 0600)
			case "row_corrupt":
				_, err = f.r.db.Exec("UPDATE reconciliation_dispositions SET hash=?", protocol.Hash("changed"))
			case "table":
				_, err = f.r.db.Exec("DROP TABLE reconciliation_dispositions")
			case "summary":
				_, err = f.r.db.Exec("UPDATE reconciliation_producers SET completion=''")
			case "payload":
				_, err = f.r.db.Exec("UPDATE messages SET hash=? WHERE id=?", protocol.Hash("changed"), d.MessageID)
			case "evidence":
				_, err = f.r.db.Exec("UPDATE reconciliation_records SET evidence_hash=? WHERE id=?", protocol.Hash("changed"), d.MessageID)
			}
			must(t, err)
			if _, err = f.r.RecoveryMessageDisposition(ctx, producer, d.MessageID); err == nil {
				t.Fatal("lost evidence returned a decision")
			}
			if _, err = f.r.DecideRecoveryMessage(ctx, d); err == nil {
				t.Fatal("lost evidence silently recreated")
			}
		})
	}
}

type dispositionCrashInput struct {
	Path     string
	Decision RecoveryDisposition
	Now      time.Time
}

func TestRecoveryDispositionCrashChild(t *testing.T) {
	input := os.Getenv("RECOVERY_DISPOSITION_INPUT")
	if input == "" {
		t.Skip("subprocess helper")
	}
	raw, err := os.ReadFile(input)
	must(t, err)
	var v dispositionCrashInput
	must(t, json.Unmarshal(raw, &v))
	session, err := OpenRecoveryDispositionSession(context.Background(), v.Path, configuration())
	must(t, err)
	defer session.Close()
	r := session.(*recoveryReconciler)
	r.now = func() time.Time { return v.Now }
	r.fault = func(stage string) error {
		if stage == os.Getenv("RECOVERY_DISPOSITION_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	_, err = r.DecideRecoveryMessage(context.Background(), v.Decision)
	must(t, err)
	t.Fatal("fault not reached")
}
func TestRecoveryDispositionSIGKILL(t *testing.T) {
	for _, stage := range []string{"disposition_intent", "disposition_transaction", "disposition_commit"} {
		t.Run(stage, func(t *testing.T) {
			f, d := dispositionFixture(t, "retry")
			must(t, f.r.Close())
			raw, err := json.Marshal(dispositionCrashInput{f.path, d, f.now})
			must(t, err)
			input := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(input, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryDispositionCrashChild$")
			cmd.Env = append(os.Environ(), "RECOVERY_DISPOSITION_INPUT="+input, "RECOVERY_DISPOSITION_STAGE="+stage)
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
			session, err := OpenRecoveryDispositionSession(ctx, f.path, configuration())
			must(t, err)
			defer session.Close()
			r := session.(*recoveryReconciler)
			var count int
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_dispositions").Scan(&count))
			want := 0
			if stage == "disposition_commit" {
				want = 1
			}
			if count != want {
				t.Fatal("partial or lost decision", count)
			}
			review, err := session.ReviewRecoveryMessage(ctx, producer, d.MessageID)
			must(t, err)
			expectedState := "intent"
			if want == 1 {
				expectedState = "recorded"
			}
			if review.DecisionState != expectedState || review.RequestID != d.ID ||
				review.Decision == nil || review.Decision.Decision != d || !review.Decision.RecordedAt.Equal(f.now) {
				t.Fatal("interrupted decision cannot be inspected", review)
			}
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_dispositions").Scan(&count))
			if count != want {
				t.Fatal("review changed projection")
			}
			changed := d
			changed.Action, changed.AcceptDuplicateRisk = "cancel", false
			if _, err = session.DecideRecoveryMessage(ctx, changed); err == nil {
				t.Fatal("interrupted intent replaced")
			}
			record, err := session.DecideRecoveryMessage(ctx, d)
			must(t, err)
			if record.Decision != d || !record.RecordedAt.Equal(f.now) || record.DeliveryEnabled {
				t.Fatal(record)
			}
			again, err := session.RecoveryMessageDisposition(ctx, producer, d.MessageID)
			must(t, err)
			if protocol.Hash(again) != protocol.Hash(record) {
				t.Fatal("replayed decision changed")
			}
		})
	}
}
