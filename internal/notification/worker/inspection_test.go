package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestInspectionDoesNotIssueReceiptAcknowledgementAuthority(t *testing.T) {
	s, now, _ := fixture(t)
	defer s.Close()
	ctx := context.Background()
	for n := uint64(1); n <= 3; n++ {
		m := report(*now, fmt.Sprintf("inspection-%d", n), n, n, n)
		m.Text = "PRIVATE MESSAGE MUST NOT APPEAR IN QUEUE INSPECTION"
		_, err := s.Enqueue(ctx, m)
		must(t, err)
		must(t, s.Cancel(ctx, producer, m.ID, m.Sequence, protocol.Hash(m)))
	}
	var after int64
	var ids []string
	for {
		page, err := s.InspectQueue(ctx, producer, after, 1)
		must(t, err)
		if page.Identity != s.Identity() || page.Producer != producer || page.After != after || len(page.Jobs) != 1 || page.Next <= after {
			t.Fatal(page)
		}
		raw, err := json.Marshal(page)
		must(t, err)
		if strings.Contains(string(raw), "PRIVATE MESSAGE") {
			t.Fatal("inspection disclosed message body")
		}
		ids = append(ids, page.Jobs[0].ID)
		after = page.Next
		if !page.More {
			break
		}
	}
	if len(ids) != 3 {
		t.Fatal(ids)
	}
	page, err := s.InspectReceipts(ctx, producer, 0, 128)
	must(t, err)
	if len(page.Receipts) < 3 || page.ListedThrough != 0 || page.AcknowledgedThrough != 0 || page.More {
		t.Fatal(page)
	}
	// Merely learning the position cannot make a producer acknowledgement valid.
	cursor := protocol.Cursor{Identity: s.Identity(), Producer: producer, Position: page.Next}
	if err = s.Ack(ctx, cursor); err == nil {
		t.Fatal("inspection issued an ack cursor")
	}
	listed, err := s.Receipts(ctx, protocol.Cursor{Identity: s.Identity(), Producer: producer}, 128)
	must(t, err)
	if listed.Cursor.Position != page.Next {
		t.Fatal("inspection changed retained receipt stream")
	}
	must(t, s.Ack(ctx, listed.Cursor))
	page, err = s.InspectReceipts(ctx, producer, 0, 1)
	must(t, err)
	if page.ListedThrough != listed.Cursor.Position || page.AcknowledgedThrough != listed.Cursor.Position || !page.More {
		t.Fatal("inspection cannot view retained acknowledged history", page)
	}
}

func TestInspectionProtocolRejectsCursorAndCrossProducerFields(t *testing.T) {
	s, _, _ := fixture(t)
	defer s.Close()
	for _, op := range []string{"inspection.queue", "inspection.receipts"} {
		good := protocol.Request{Version: protocol.Version, Operation: op, Expected: s.Identity(), Producer: producer, Limit: 1}
		if reply := s.Handle(context.Background(), good); !reply.OK {
			t.Fatal(reply)
		}
		for _, change := range []func(*protocol.Request){
			func(r *protocol.Request) { r.After = -1 }, func(r *protocol.Request) { r.Limit = 129 }, func(r *protocol.Request) { r.Limit = 0 },
			func(r *protocol.Request) { r.Producer.Epoch = "foreign" }, func(r *protocol.Request) { r.Expected.Recovery = "foreign" },
			func(r *protocol.Request) { r.Cursor = &protocol.Cursor{Identity: s.Identity(), Producer: producer} },
			func(r *protocol.Request) { r.Sequence = 1 }, func(r *protocol.Request) { r.Generation = "one" },
		} {
			r := good
			change(&r)
			if reply := s.Handle(context.Background(), r); reply.OK {
				t.Fatal("invalid inspection accepted", r)
			}
		}
	}
}

func TestReceiptInspectionPaginationIsBoundedByEncodedSize(t *testing.T) {
	s, _, _ := fixture(t)
	defer s.Close()
	// Large retained receipts exercise the byte limit independently of row count.
	for n := 1; n <= 128; n++ {
		r := protocol.Receipt{EventID: fmt.Sprint(n), Sequence: uint64(n), State: strings.Repeat("x", 4000)}
		b, err := json.Marshal(r)
		must(t, err)
		_, err = s.db.Exec("INSERT INTO receipts(producer,body) VALUES(?,?)", producer.ID, string(b))
		must(t, err)
	}
	page, err := s.InspectReceipts(context.Background(), producer, 0, 128)
	must(t, err)
	raw, err := json.Marshal(page)
	must(t, err)
	if len(raw) > protocol.MaxResponse-2048 || !page.More || len(page.Receipts) == 0 || len(page.Receipts) == 128 {
		t.Fatal("unbounded inspection", len(raw), len(page.Receipts), page.More)
	}
	next, err := s.InspectReceipts(context.Background(), producer, page.Next, 128)
	must(t, err)
	if len(next.Receipts) == 0 || next.Receipts[0].Position <= page.Next {
		t.Fatal("pagination stalled")
	}
}
