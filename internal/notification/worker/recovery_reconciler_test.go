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
)

type reconcileFixture struct {
	r        *recoveryReconciler
	path     string
	now      time.Time
	records  []producerRecoveryRecord
	manifest producerRecoveryManifest
}

func newReconcileFixture(t *testing.T) reconcileFixture {
	t.Helper()
	ctx := context.Background()
	s, now, path := fixture(t)
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	first := report(*now, "first", 1, 1, 1)
	_, err := s.Enqueue(ctx, first)
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	job, granted, err := s.Grant(ctx, producer, first.ID)
	must(t, err)
	if !granted {
		t.Fatal("fixture grant missing")
	}
	must(t, s.Finish(ctx, job, Result{Accepted: true}))
	second := report(*now, "second", 2, 1, 1)
	second.ReportKey = "other-day"
	_, err = s.Enqueue(ctx, second)
	must(t, err)
	page, err := s.Receipts(ctx, protocol.Cursor{Identity: s.Identity(), Producer: producer}, 128)
	must(t, err)
	var receipt protocol.Receipt
	for _, v := range page.Receipts {
		if v.State == "provider_accepted" {
			receipt = v
		}
	}
	if receipt.Position == 0 {
		t.Fatal("missing acceptance")
	}
	old := s.Identity()
	c := s.config
	must(t, s.Close())
	_, err = prepareSnapshotRecovery(ctx, path, artifact, c, nil)
	must(t, err)
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	must(t, err)
	f := reconcileFixture{r: r, path: path, now: now.Add(time.Minute),
		records:  []producerRecoveryRecord{{Message: first, Receipt: &retainedRecoveryReceipt{Identity: old, Producer: producer, Receipt: receipt}}, {Message: second}},
		manifest: producerRecoveryManifest{HighestIssued: 2, HighestAcknowledged: 2, ImportedReceipts: page.Cursor}}
	r.now = func() time.Time { return f.now }
	t.Cleanup(func() { r.Close() })
	return f
}

func beginReconciliation(t *testing.T, f reconcileFixture) reconciliationChallenge {
	t.Helper()
	ch, err := f.r.Challenge(context.Background(), producer)
	must(t, err)
	must(t, f.r.Begin(context.Background(), ch, f.manifest))
	return ch
}

func TestRecoveryReconciliationPreservesAcceptanceAndReconstructsAmbiguity(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	ch := beginReconciliation(t, f)
	must(t, f.r.Import(ctx, ch, f.records[:1]))
	must(t, f.r.Close())
	r, err := openRecoveryReconciler(ctx, f.path, configuration(), nil)
	must(t, err)
	defer r.Close()
	r.now = func() time.Time { return f.now }
	must(t, r.Import(ctx, ch, f.records)) // Lost page response is idempotent.
	status := snapshot(f.now, 2, 1)
	summary, err := r.Finish(ctx, ch, status)
	must(t, err)
	if summary.DeliveryEnabled || summary.Ambiguous != 1 || summary.UnknownLast != 0 {
		t.Fatal(summary)
	}
	var state string
	var n int
	must(t, r.db.QueryRow("SELECT state FROM messages WHERE id='first'").Scan(&state))
	if state != "provider_accepted" {
		t.Fatal(state)
	}
	must(t, r.db.QueryRow("SELECT state FROM messages WHERE id='second'").Scan(&state))
	if state != "ambiguous" {
		t.Fatal("reconstruction granted delivery")
	}
	must(t, r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&n))
	if n != 0 {
		t.Fatal("old receipt republished in new epoch")
	}
	var proof string
	must(t, r.db.QueryRow("SELECT body FROM reconciliation_receipts").Scan(&proof))
	var retained retainedRecoveryReceipt
	must(t, protocol.Decode([]byte(proof), &retained, 8192))
	if protocol.Hash(retained) != protocol.Hash(*f.records[0].Receipt) {
		t.Fatal("acceptance provenance or eligibility rewritten")
	}
	var submitted uint64
	must(t, r.db.QueryRow("SELECT submitted FROM producers WHERE id=?", producer.ID).Scan(&submitted))
	if submitted != 2 {
		t.Fatal(submitted)
	}
	var seq uint64
	must(t, r.db.QueryRow("SELECT seq FROM status").Scan(&seq))
	if seq != 1 {
		t.Fatal("reconciliation published fresh health")
	}
	again, err := r.Summary(ctx, producer)
	must(t, err)
	if protocol.Hash(again) != protocol.Hash(summary) {
		t.Fatal("summary changed")
	}
	must(t, r.Close())
	_, err = resumeSnapshotRecovery(ctx, f.path, configuration(), nil)
	must(t, err) // Candidate still immutable.
	if s, err := Open(f.path, configuration(), false); err == nil {
		s.Close()
		t.Fatal("reconciliation activated worker")
	}
}

func TestRecoveryReconciliationRejectsConflictsAtomically(t *testing.T) {
	for _, kind := range []string{"epoch", "nonce", "producer", "payload", "duplicate-sequence", "receipt-epoch", "receipt-position", "receipt-event", "provider", "eligibility", "header", "old-watermark", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			f := newReconcileFixture(t)
			ctx := context.Background()
			ch := beginReconciliation(t, f)
			// Clone nested acceptance pointers before modifying evidence.
			data, _ := json.Marshal(f.records)
			var records []producerRecoveryRecord
			must(t, protocol.Decode(data, &records, protocol.MaxRequest))
			var err error
			switch kind {
			case "epoch":
				ch.Identity.Recovery = "wrong"
			case "nonce":
				ch.Nonce = "n00000000000000000000000000000000"
			case "producer":
				ch.Producer.ID = "other"
			case "payload":
				records[0].Message.Text = "changed"
			case "duplicate-sequence":
				records[1].Message.Sequence = 1
			case "receipt-epoch":
				records[0].Receipt.Identity.Recovery = "wrong"
			case "receipt-position":
				records[0].Receipt.Receipt.Position = f.manifest.ImportedReceipts.Position + 1
			case "receipt-event":
				records[0].Receipt.Receipt.EventID = "different"
			case "provider":
				records[0].Receipt.Receipt.Provider = "other"
			case "eligibility":
				records[0].Receipt.Receipt.Acceptance.BasisRevision = 99
			case "header":
				m := f.manifest
				m.HighestIssued++
				err = f.r.Begin(ctx, ch, m)
			case "old-watermark":
				m := f.manifest
				m.HighestAcknowledged = 0
				m.HighestIssued = 0
				err = f.r.Begin(ctx, ch, m)
			case "oversized":
				records = make([]producerRecoveryRecord, 129)
			}
			if kind != "header" && kind != "old-watermark" {
				err = f.r.Import(ctx, ch, records)
			}
			if err == nil {
				t.Fatal("invalid evidence accepted", kind)
			}
			var n int
			must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_records").Scan(&n))
			if n != 0 {
				t.Fatal("partial page committed")
			}
			var state string
			must(t, f.r.db.QueryRow("SELECT state FROM messages WHERE id='first'").Scan(&state))
			if state != "ambiguous" {
				t.Fatal("partial acceptance committed")
			}
		})
	}
}

func TestRecoveryReconciliationRequiresFreshCompleteEvidence(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	ch := beginReconciliation(t, f)
	must(t, f.r.Import(ctx, ch, f.records[:1]))
	if _, err := f.r.Finish(ctx, ch, snapshot(f.now, 2, 1)); err == nil {
		t.Fatal("submission gap ignored")
	}
	must(t, f.r.Import(ctx, ch, f.records[1:]))
	if _, err := f.r.Finish(ctx, ch, snapshot(f.now.Add(-time.Second), 2, 1)); err == nil {
		t.Fatal("pre-challenge status accepted")
	}
	f.r.now = func() time.Time { return f.now.Add(11 * time.Minute) }
	if err := f.r.Import(ctx, ch, f.records); err == nil {
		t.Fatal("expired challenge accepted")
	}
	next, err := f.r.Challenge(ctx, producer)
	must(t, err)
	if next.Nonce == ch.Nonce {
		t.Fatal("expired nonce reused")
	}
	if err = f.r.Import(ctx, ch, f.records); err == nil {
		t.Fatal("old challenge revived")
	}
	_, err = f.r.Finish(ctx, next, snapshot(f.now.Add(11*time.Minute), 2, 1))
	must(t, err)
}

func TestRecoveryReconciliationRetainsUnknownCompactedFloor(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	f.manifest.HighestIssued = 6
	f.manifest.HighestAcknowledged = 5
	f.manifest.CompactedThrough = 5
	f.manifest.OutcomeCheckpoint = strings.Repeat("a", 64)
	ch := beginReconciliation(t, f)
	if err := f.r.Import(ctx, ch, f.records); err == nil {
		t.Fatal("compacted records revived")
	}
	record := f.records[1]
	record.Message.Sequence = 6
	record.Message.ID = "after-compaction"
	must(t, f.r.Import(ctx, ch, []producerRecoveryRecord{record}))
	summary, err := f.r.Finish(ctx, ch, snapshot(f.now, 2, 1))
	must(t, err)
	if summary.UnknownFirst != 2 || summary.UnknownLast != 5 || summary.Manifest.CompactedThrough != 5 || summary.DeliveryEnabled {
		t.Fatal(summary)
	}
	var n int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM messages WHERE seq BETWEEN 2 AND 5").Scan(&n))
	if n != 0 {
		t.Fatal("compacted outcomes fabricated")
	}
}

func TestRecoveryReconciliationRejectsMissingInitializedDatabase(t *testing.T) {
	f := newReconcileFixture(t)
	path := filepath.Join(f.path, "recovery", f.r.journal.Operation, "reconciled.db")
	must(t, f.r.Close())
	must(t, os.Remove(path))
	if r, err := openRecoveryReconciler(context.Background(), f.path, configuration(), nil); err == nil {
		r.Close()
		t.Fatal("missing reconciliation ledger recreated")
	}
}

func TestRecoveryReconciliationCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_RECONCILIATION_STATE")
	if path == "" {
		t.Skip("subprocess fixture")
	}
	target := os.Getenv("DEPLOY_RECONCILIATION_BOUNDARY")
	fault := func(stage string) error {
		if stage == target {
			fmt.Println("READY")
			for {
				time.Sleep(time.Second)
			}
		}
		return nil
	}
	input := os.Getenv("DEPLOY_RECONCILIATION_INPUT")
	openFault := fault
	if input != "" {
		openFault = nil
	}
	r, err := openRecoveryReconciler(context.Background(), path, configuration(), openFault)
	must(t, err)
	if input != "" {
		var payload reconciliationCrashPage
		body, err := os.ReadFile(input)
		must(t, err)
		must(t, protocol.Decode(body, &payload, protocol.MaxRequest+8192))
		r.now = func() time.Time { return payload.Now }
		r.fault = fault
		err = r.Import(context.Background(), payload.Challenge, payload.Records)
	}
	must(t, err)
	t.Fatal("boundary not reached")
}

type reconciliationCrashPage struct {
	Challenge reconciliationChallenge
	Records   []producerRecoveryRecord
	Now       time.Time
}

func TestRecoveryReconciliationPageSIGKILL(t *testing.T) {
	for _, boundary := range []string{"reconciliation_transaction", "reconciliation_commit"} {
		t.Run(boundary, func(t *testing.T) {
			f := newReconcileFixture(t)
			ch := beginReconciliation(t, f)
			must(t, f.r.Close())
			input := filepath.Join(t.TempDir(), "page.json")
			body, _ := json.Marshal(reconciliationCrashPage{ch, f.records, f.now})
			must(t, os.WriteFile(input, body, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryReconciliationCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_RECONCILIATION_STATE="+f.path, "DEPLOY_RECONCILIATION_BOUNDARY="+boundary, "DEPLOY_RECONCILIATION_INPUT="+input)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			must(t, cmd.Start())
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				t.Fatalf("boundary not reached: %s %s", scanner.Text(), stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			r, err := openRecoveryReconciler(context.Background(), f.path, configuration(), nil)
			must(t, err)
			defer r.Close()
			r.now = func() time.Time { return f.now }
			var count int
			must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_records").Scan(&count))
			want := 0
			if boundary == "reconciliation_commit" {
				want = 2
			}
			if count != want {
				t.Fatal("partial page or lost commit", count)
			}
			must(t, r.Import(context.Background(), ch, f.records))
			summary, err := r.Finish(context.Background(), ch, snapshot(f.now, 2, 1))
			must(t, err)
			if summary.Ambiguous != 1 || summary.DeliveryEnabled {
				t.Fatal(summary)
			}
		})
	}
}

func TestRecoveryReconciliationCannotForgetReceiptStatusWatermark(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	ch := beginReconciliation(t, f)
	f.records[0].Receipt.Receipt.Acceptance.StatusSequence = 5
	must(t, f.r.Import(ctx, ch, f.records))
	if _, err := f.r.Finish(ctx, ch, snapshot(f.now, 2, 1)); err == nil {
		t.Fatal("producer forgot retained status counter")
	}
	_, err := f.r.Finish(ctx, ch, snapshot(f.now, 6, 1))
	must(t, err)
}

func TestRecoveryReconciliationCompletionIsPerProducer(t *testing.T) {
	c := configuration()
	other := c.Producers[0]
	other.Producer = protocol.Producer{ID: "uptime", Epoch: "epoch-one"}
	c.Producers = append(c.Producers, other)
	path := t.TempDir()
	must(t, os.Chmod(path, 0700))
	s, err := Open(path, c, true)
	must(t, err)
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(context.Background(), artifact)
	must(t, err)
	must(t, s.Close())
	_, err = prepareSnapshotRecovery(context.Background(), path, artifact, c, nil)
	must(t, err)
	r, err := openRecoveryReconciler(context.Background(), path, c, nil)
	must(t, err)
	defer r.Close()
	ch, err := r.Challenge(context.Background(), producer)
	must(t, err)
	m := producerRecoveryManifest{ImportedReceipts: protocol.Cursor{Identity: r.journal.Before, Producer: producer}}
	must(t, r.Begin(context.Background(), ch, m))
	_, err = r.Finish(context.Background(), ch, snapshot(ch.IssuedAt, 1, 1))
	must(t, err)
	if _, err = r.Summary(context.Background(), other.Producer); err == nil {
		t.Fatal("unseen producer marked reconciled")
	}
	if _, err = r.Challenge(context.Background(), other.Producer); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryReconciliationChallengesExpireWithoutWallClockProgress(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	boot, uptime := "boot-one", 100.0
	f.r.clock = func() (string, float64, error) { return boot, uptime, nil }
	ch := beginReconciliation(t, f)
	uptime += 601
	if err := f.r.Import(ctx, ch, f.records); err == nil {
		t.Fatal("stalled wall clock extended challenge")
	}
	next, err := f.r.Challenge(ctx, producer)
	must(t, err)
	if next.Nonce == ch.Nonce {
		t.Fatal("expired nonce reused")
	}
	boot = "boot-two"
	uptime = 10
	if err = f.r.Import(ctx, next, f.records); err == nil {
		t.Fatal("challenge survived reboot")
	}
	last, err := f.r.Challenge(ctx, producer)
	must(t, err)
	must(t, f.r.Import(ctx, last, f.records))
	uptime = 5
	if _, err = f.r.Finish(ctx, last, snapshot(f.now, 2, 1)); err == nil {
		t.Fatal("monotonic clock rollback accepted")
	}
	f.r.clock = func() (string, float64, error) { return "", 0, errors.New("clock unavailable") }
	if _, err = f.r.Challenge(ctx, producer); err == nil {
		t.Fatal("missing host clock ignored")
	}
}

func TestRecoveryReconciliationInitializationSIGKILL(t *testing.T) {
	for _, boundary := range []string{"reconciliation_intent", "reconciliation_copy", "reconciliation_transaction", "reconciliation_commit", "reconciliation_ready"} {
		t.Run(boundary, func(t *testing.T) {
			s, _, path := fixture(t)
			artifact := filepath.Join(t.TempDir(), "snapshot")
			ctx := context.Background()
			_, err := s.Snapshot(ctx, artifact)
			must(t, err)
			must(t, s.Close())
			_, err = prepareSnapshotRecovery(ctx, path, artifact, configuration(), nil)
			must(t, err)
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryReconciliationCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_RECONCILIATION_STATE="+path, "DEPLOY_RECONCILIATION_BOUNDARY="+boundary)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			must(t, cmd.Start())
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				t.Fatalf("boundary not reached: %s %s", scanner.Text(), stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			r, err := openRecoveryReconciler(context.Background(), path, configuration(), nil)
			must(t, err)
			r.Close()
			if s, err := Open(path, configuration(), false); err == nil {
				s.Close()
				t.Fatal("reconciliation initialization activated worker")
			}
		})
	}
}
