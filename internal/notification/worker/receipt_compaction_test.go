package worker

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func receiptCompactionFixture(t *testing.T) (*Store, *time.Time, string, protocol.Message, protocol.Cursor) {
	t.Helper()
	s, now, path := fixture(t)
	ctx := context.Background()
	m := report(*now, "compact-event", 1, 1, 1)
	must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
	_, err := s.Enqueue(ctx, m)
	must(t, err)
	j, ok, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !ok {
		t.Fatal("no fixture grant")
	}
	must(t, s.Finish(ctx, j, Result{}))
	*now = now.Add(3 * time.Second)
	j, ok, err = s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !ok {
		t.Fatal("no fixture retry")
	}
	must(t, s.Finish(ctx, j, Result{Accepted: true}))
	page, err := s.Receipts(ctx, protocol.Cursor{Identity: s.identity, Producer: producer}, 128)
	must(t, err)
	must(t, s.Ack(ctx, page.Cursor))
	*now = now.Add(91 * 24 * time.Hour)
	return s, now, path, m, page.Cursor
}

func TestReceiptCompactionKeepsIndependentChatReferences(t *testing.T) {
	s, now, _, in, old := deliveredContext(t, "single")
	ctx := context.Background()
	page, err := s.Receipts(ctx, protocol.Cursor{Identity: s.identity, Producer: producer}, 128)
	must(t, err)
	must(t, s.Ack(ctx, page.Cursor))
	*now = now.Add(91 * 24 * time.Hour)
	// A separate fresh delivery must remain addressable while old receipt history
	// is compacted. No context deadlines are extended by retention work.
	m := old
	m.ID = "fresh-reference"
	m.Sequence = 2
	m.ReportRevision = 2
	m.ObservedAt = *now
	m.ExpiresAt = now.Add(5 * time.Minute)
	must(t, s.Replace(ctx, snapshot(*now, 2, 1)))
	_, err = s.Enqueue(ctx, m)
	must(t, err)
	j, ok, err := s.Grant(ctx, producer, m.ID)
	must(t, err)
	if !ok {
		t.Fatal("fresh reference not granted")
	}
	dest := s.config.Interaction.Destinations[0]
	must(t, s.Finish(ctx, j, Result{Accepted: true, Binding: &interaction.DeliveryBinding{Destination: dest, Native: interaction.NativeReply{ID: "43", Sender: dest.Account}}}))
	var before string
	must(t, s.db.QueryRow("SELECT json_group_array(json_array(destination,native_id,sender,reference,expires,conflict)) FROM notification_deliveries").Scan(&before))
	out, err := s.CompactReceipts(ctx, producer, 128, true)
	must(t, err)
	if !out.Applied || len(out.Candidates) != 1 {
		t.Fatal(out)
	}
	var after string
	must(t, s.db.QueryRow("SELECT json_group_array(json_array(destination,native_id,sender,reference,expires,conflict)) FROM notification_deliveries").Scan(&after))
	if before != after {
		t.Fatal("compaction changed native reference history")
	}
	in.SentAt = *now
	if _, err = s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("old expired context revived")
	}
	in.NativeID = "fresh-inbound"
	in.Reply.ID = "43"
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	if v.Command.Context == nil || v.Command.Context.Context.Targets[0].Occurrence != "original-occurrence" {
		t.Fatal("fresh typed reference lost")
	}
}

func TestReceiptCompactionPolicyAndIsolation(t *testing.T) {
	c := configuration()
	for _, days := range []int{-1, 1, 89, 3651} {
		c.ReceiptAuditDays = days
		if c.Validate() == nil {
			t.Fatal("unsafe retention accepted", days)
		}
	}
	for _, days := range []int{0, 90, 3650} {
		c.ReceiptAuditDays = days
		must(t, c.Validate())
	}
	s, _, _, m, _ := receiptCompactionFixture(t)
	// Clone receipt rows as another producer's opaque retained history. Compaction
	// must not depend on or alter this unrelated stream, including its positions.
	_, err := s.db.Exec("INSERT INTO receipts(producer,body) SELECT 'unrelated',body FROM receipts")
	must(t, err)
	var before string
	must(t, s.db.QueryRow("SELECT json_group_array(json_array(position,body)) FROM receipts WHERE producer='unrelated'").Scan(&before))
	_, err = s.CompactReceipts(context.Background(), producer, 128, true)
	must(t, err)
	var after string
	must(t, s.db.QueryRow("SELECT json_group_array(json_array(position,body)) FROM receipts WHERE producer='unrelated'").Scan(&after))
	if before != after {
		t.Fatal("other producer receipt stream changed")
	}
	foreign := producer
	foreign.Epoch = "different"
	if _, err = s.CompactReceipts(context.Background(), foreign, 128, true); err == nil {
		t.Fatal("wrong producer epoch accepted")
	}
	if reply := s.Handle(context.Background(), protocol.Request{Version: 1, Operation: "receipts.compact", Expected: s.identity, Producer: producer, EventID: m.ID}); reply.OK {
		t.Fatal("producer API exposed compaction")
	}
}

func TestReceiptCompactionRejectsCorruptAuditAtStartupAndSnapshot(t *testing.T) {
	s, _, path, _, _ := receiptCompactionFixture(t)
	_, err := s.CompactReceipts(context.Background(), producer, 128, true)
	must(t, err)
	_, err = s.db.Exec("UPDATE metadata SET value=json_set(value,'$.through',999999) WHERE key=?", receiptCompactionKey(s.identity, producer))
	must(t, err)
	if _, err = s.Snapshot(context.Background(), filepath.Join(t.TempDir(), "snapshot")); err == nil {
		t.Fatal("snapshot accepted invalid compaction evidence")
	}
	c := s.config
	must(t, s.Close())
	if reopened, err := Open(path, c, false); err == nil {
		reopened.Close()
		t.Fatal("startup accepted invalid compaction evidence")
	}
}

func TestReceiptCompactionRetentionBoundaryAndFailedCapacity(t *testing.T) {
	s, now, _, _, _ := receiptCompactionFixture(t)
	*now = now.Add(-24 * time.Hour)
	out, err := s.CompactReceipts(context.Background(), producer, 128, true)
	must(t, err)
	if out.Applied || len(out.Candidates) != 0 {
		t.Fatal("exact retention boundary was compacted")
	}
	*now = now.Add(time.Second)
	oldBudget := s.config.MaxDatabaseBytes
	s.config.MaxDatabaseBytes = 1
	if _, err = s.CompactReceipts(context.Background(), producer, 128, true); err == nil {
		t.Fatal("insufficient outcome headroom ignored")
	}
	s.config.MaxDatabaseBytes = oldBudget
	var count int
	must(t, s.db.QueryRow("SELECT count(*) FROM receipts").Scan(&count))
	if count != 4 {
		t.Fatal("capacity failure lost receipts")
	}
	out, err = s.CompactReceipts(context.Background(), producer, 128, true)
	must(t, err)
	if len(out.Candidates) != 3 || !out.Applied {
		t.Fatal(out)
	}
}

func TestReceiptCompactionDoesNotEraseCorruptEvidence(t *testing.T) {
	for _, query := range []string{"UPDATE messages SET hash='changed'", "UPDATE receipts SET body=json_set(body,'$.state','unknown-state') WHERE position=1"} {
		t.Run(query, func(t *testing.T) {
			s, _, _, _, _ := receiptCompactionFixture(t)
			_, err := s.db.Exec(query)
			must(t, err)
			if _, err = s.CompactReceipts(context.Background(), producer, 128, true); err == nil {
				t.Fatal("corrupt evidence silently discarded")
			}
			var count int
			must(t, s.db.QueryRow("SELECT count(*) FROM receipts").Scan(&count))
			if count != 4 {
				t.Fatal("failed compaction modified history")
			}
		})
	}
}

func TestReceiptCompactionPreservesOutcomeAndReplayFences(t *testing.T) {
	s, now, path, m, cursor := receiptCompactionFixture(t)
	ctx := context.Background()
	before, err := s.Get(ctx, producer, m.ID)
	must(t, err)
	preview, err := s.CompactReceipts(ctx, producer, 2, false)
	must(t, err)
	if preview.Applied || !preview.More || len(preview.Candidates) != 2 || preview.History != nil {
		t.Fatal(preview)
	}
	var count int
	must(t, s.db.QueryRow("SELECT count(*) FROM receipts").Scan(&count))
	if count != 4 {
		t.Fatal("preview wrote receipts", count)
	}
	result, err := s.CompactReceipts(ctx, producer, 2, true)
	must(t, err)
	if !result.Applied || result.History.Removed != 2 || result.History.Passes != 1 {
		t.Fatal(result)
	}
	result, err = s.CompactReceipts(ctx, producer, 2, true)
	must(t, err)
	if !result.Applied || result.More || result.History.Removed != 3 || result.History.Passes != 2 {
		t.Fatal(result)
	}
	result, err = s.CompactReceipts(ctx, producer, 2, true)
	must(t, err)
	if result.Applied || len(result.Candidates) != 0 || result.History.Removed != 3 {
		t.Fatal("empty pass changed history", result)
	}
	page, err := s.InspectReceipts(ctx, producer, 0, 128)
	must(t, err)
	if len(page.Receipts) != 1 || page.Receipts[0].State != "provider_accepted" || page.Receipts[0].Acceptance == nil || page.Compaction == nil || page.AcknowledgedThrough != cursor.Position || page.ListedThrough != cursor.Position {
		t.Fatal(page)
	}
	must(t, s.Ack(ctx, cursor)) // Lost ACK response remains idempotent.
	if _, err = s.Receipts(ctx, protocol.Cursor{Identity: s.identity, Producer: producer}, 128); err == nil {
		t.Fatal("old producer cursor silently skipped history")
	}
	after, err := s.Enqueue(ctx, m)
	must(t, err)
	if protocol.Hash(before) != protocol.Hash(after) {
		t.Fatal("compaction changed idempotent message outcome")
	}
	if _, ok, err := s.Grant(ctx, producer, m.ID); err != nil || ok {
		t.Fatal("compaction granted old event", err)
	}
	changed := m
	changed.Sequence = 2
	if _, err = s.Enqueue(ctx, changed); err == nil {
		t.Fatal("event ID reuse admitted")
	}
	changed = m
	changed.ID = "different-event"
	if _, err = s.Enqueue(ctx, changed); err == nil {
		t.Fatal("old sequence admitted as new event")
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	c := s.config
	id := s.identity
	must(t, s.Close())
	s, err = OpenManagedVerification(path, c, id)
	must(t, err)
	defer s.Close()
	s.now = func() time.Time { return *now }
	page, err = s.InspectReceipts(ctx, producer, 0, 128)
	must(t, err)
	if page.Compaction == nil || page.Compaction.Removed != 3 || len(page.Receipts) != 1 {
		t.Fatal("restart lost receipt history", page)
	}
}

func TestReceiptCompactionRetainsProtectedHistory(t *testing.T) {
	for _, scenario := range []string{"unacknowledged", "partial_ack", "inflight", "nonterminal", "recent", "retention", "clock", "future_receipt"} {
		t.Run(scenario, func(t *testing.T) {
			s, now, _, _, _ := receiptCompactionFixture(t)
			switch scenario {
			case "unacknowledged":
				_, err := s.db.Exec("UPDATE producers SET acked=0")
				must(t, err)
			case "partial_ack":
				_, err := s.db.Exec("UPDATE producers SET acked=2")
				must(t, err)
			case "inflight":
				_, err := s.db.Exec("UPDATE messages SET attempt='inflight'")
				must(t, err)
			case "nonterminal":
				_, err := s.db.Exec("UPDATE messages SET state='ambiguous'")
				must(t, err)
			case "recent":
				*now = now.Add(-2 * 24 * time.Hour)
			case "retention":
				s.config.ReceiptAuditDays = 180
			case "clock":
				s.clockUncertain = true
			case "future_receipt":
				_, err := s.db.Exec("UPDATE receipts SET body=json_set(body,'$.at',?) WHERE position=4", now.Add(time.Hour).Format(time.RFC3339Nano))
				must(t, err)
			}
			out, err := s.CompactReceipts(context.Background(), producer, 128, true)
			if scenario == "clock" {
				if err == nil {
					t.Fatal("uncertain clock permitted compaction")
				}
			} else {
				must(t, err)
			}
			if out.Applied || len(out.Candidates) != 0 {
				t.Fatal("protected history compacted", out)
			}
			var count int
			must(t, s.db.QueryRow("SELECT count(*) FROM receipts").Scan(&count))
			if count != 4 {
				t.Fatal(count)
			}
		})
	}
}

func TestReceiptCompactionPinsAdministrativeAudit(t *testing.T) {
	s, now, path, d := administrationFixture(t)
	ctx := context.Background()
	record, err := s.DecideMessage(ctx, d)
	must(t, err)
	j, ok, err := s.Grant(ctx, producer, d.Review.Job.Message.ID)
	must(t, err)
	if !ok {
		t.Fatal("no retry")
	}
	must(t, s.Finish(ctx, j, Result{Accepted: true}))
	page, err := s.Receipts(ctx, protocol.Cursor{Identity: s.identity, Producer: producer}, 128)
	must(t, err)
	must(t, s.Ack(ctx, page.Cursor))
	*now = now.Add(91 * 24 * time.Hour)
	out, err := s.CompactReceipts(ctx, producer, 128, true)
	must(t, err)
	if !out.Applied {
		t.Fatal("no candidates")
	}
	for _, pos := range out.Candidates {
		if pos == record.Receipt.Position {
			t.Fatal("administrative receipt discarded")
		}
	}
	got, err := s.MessageDecision(ctx, d.Review.ID)
	must(t, err)
	if protocol.Hash(got) != protocol.Hash(record) {
		t.Fatal("audit changed")
	}
	_, err = s.Snapshot(ctx, filepath.Join(t.TempDir(), "snapshot"))
	must(t, err)
	c := s.config
	must(t, s.Close())
	s, err = Open(path, c, false)
	must(t, err)
	defer s.Close()
	got, err = s.MessageDecision(ctx, d.Review.ID)
	must(t, err)
	if protocol.Hash(got) != protocol.Hash(record) {
		t.Fatal("startup lost administrative audit")
	}
}

func TestReceiptCompactionRollbackAndLostCompletion(t *testing.T) {
	for _, point := range []string{"receipt_compaction_deleted", "receipt_compaction_transaction", "receipt_compaction_commit"} {
		t.Run(point, func(t *testing.T) {
			s, _, _, _, _ := receiptCompactionFixture(t)
			injected := errors.New("interrupted")
			c := receiptCompactor{s: s, fault: func(p string) error {
				if p == point {
					return injected
				}
				return nil
			}}
			_, err := c.run(context.Background(), producer, 128, true)
			if !errors.Is(err, injected) {
				t.Fatal(err)
			}
			must(t, s.transaction(context.Background(), func(tx *sql.Tx) error {
				var count int
				if err := tx.QueryRow("SELECT count(*) FROM receipts").Scan(&count); err != nil {
					return err
				}
				h, err := readReceiptCompaction(context.Background(), tx, s.identity, producer)
				if err != nil {
					return err
				}
				if point == "receipt_compaction_commit" {
					if count != 1 || h == nil || h.Removed != 3 {
						t.Fatal(count, h)
					}
				} else if count != 4 || h != nil {
					t.Fatal("partial deletion survived", count, h)
				}
				return nil
			}))
			_, err = s.CompactReceipts(context.Background(), producer, 128, true)
			must(t, err)
			page, err := s.InspectReceipts(context.Background(), producer, 0, 128)
			must(t, err)
			if page.Compaction.Removed != 3 || len(page.Receipts) != 1 {
				t.Fatal(page)
			}
		})
	}
}
