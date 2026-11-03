package worker

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestInteractiveUnixHandoffAndLedger(t *testing.T) {
	s, now, _, in := commandFixture(t)
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0700))
	socket := filepath.Join(dir, "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, socket) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("API did not stop")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case err := <-done:
			done <- err
			t.Fatal(err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("API unavailable")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c := client.Client{Socket: socket, Identity: s.Identity()}
	var view interaction.View
	must(t, c.CommandCall(ctx, protocol.Producer{}, "command.ingest", in, &view))
	queue, err := c.Commands(ctx, producer)
	must(t, err)
	if len(queue) != 1 || queue[0].Command.ID != view.Command.ID {
		t.Fatal("handoff missing")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "domain.db")+"?_txlock=immediate&_pragma=synchronous(FULL)")
	must(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	ledger, err := interaction.OpenLedger(ctx, db, producer, s.Identity(), true)
	must(t, err)
	must(t, ledger.Import(ctx, view.Command))
	must(t, c.CommandCall(ctx, producer, "command.import-ack", commandRef{ID: view.Command.ID, Hash: protocol.Hash(view.Command)}, nil))
	var grant interaction.Grant
	must(t, c.CommandCall(ctx, producer, "command.grant", commandRef{ID: view.Command.ID}, &grant))
	result, err := ledger.Execute(ctx, view.Command, grant, "", *now, func(tx *sql.Tx, cmd interaction.Command) (interaction.Result, error) {
		return interaction.Result{State: "succeeded", Scope: cmd.Scope, Text: "Synthetic subject A: unknown (fixture only)"}, nil
	})
	must(t, err)
	receipt, err := c.SubmitCommandResult(ctx, producer, result)
	must(t, err)
	must(t, ledger.ImportReceipt(ctx, receipt))
	must(t, c.CommandCall(ctx, producer, "command.result-ack", receipt, nil))
	must(t, c.CommandCall(ctx, producer, "command.get", commandRef{ID: view.Command.ID}, &view))
	if view.State != "succeeded" {
		t.Fatal(view.State)
	}
	replies, err := s.PendingCommandReplies(ctx)
	must(t, err)
	if len(replies) != 1 || replies[0].Destination.NativeID != "-100" || !view.Command.Scope.Allows(replies[0].Scope) {
		t.Fatal("reply scope/destination lost")
	}
	// This test deliberately reads the outbox without sending a real message.
	in.NativeID = "update-two"
	must(t, c.CommandCall(ctx, protocol.Producer{}, "command.ingest", in, &view))
	must(t, ledger.Import(ctx, view.Command))
	must(t, c.CommandCall(ctx, producer, "command.import-ack", commandRef{ID: view.Command.ID, Hash: protocol.Hash(view.Command)}, nil))
	must(t, c.CommandCall(ctx, producer, "command.grant", commandRef{ID: view.Command.ID}, &grant))
	*now = view.Command.Deadline.Add(time.Second)
	proof, err := ledger.ProveNonAdmission(ctx, view.Command, grant)
	must(t, err)
	must(t, c.CommandCall(ctx, producer, "command.non-admission", proof, &view))
	if view.State != "expired" || view.NonAdmission == nil {
		t.Fatal("non-admission not reconciled")
	}
	must(t, c.CommandCall(ctx, producer, "command.non-admission", proof, &view))
	_, err = ledger.Execute(ctx, view.Command, grant, "", view.Command.IssuedAt, func(*sql.Tx, interaction.Command) (interaction.Result, error) {
		t.Fatal("delayed grant executed after reconciliation")
		return interaction.Result{}, nil
	})
	if err == nil {
		t.Fatal("reconciled fence lost")
	}
	in.NativeID = "update-three"
	in.SentAt = *now
	must(t, c.CommandCall(ctx, protocol.Producer{}, "command.ingest", in, &view))
	handler := &fixtureCommandHandler{descriptor: view.Command.Descriptor}
	runner := client.Runner{Client: c, Producer: producer, Ledger: ledger, Handlers: map[string]client.CommandHandler{view.Command.Descriptor.ID: handler}, Now: func() time.Time { return *now }}
	must(t, runner.Step(ctx))
	must(t, runner.Step(ctx))
	if handler.calls != 1 {
		t.Fatal("runner repeated execution", handler.calls)
	}
	items, err := ledger.PendingResults(ctx)
	must(t, err)
	if len(items) != 0 {
		t.Fatal("result ACK not completed", len(items))
	}
	must(t, c.CommandCall(ctx, producer, "command.get", commandRef{ID: view.Command.ID}, &view))
	if view.State != "succeeded" {
		t.Fatal(view.State)
	}
}

type fixtureCommandHandler struct {
	descriptor interaction.Descriptor
	calls      int
}

func (h *fixtureCommandHandler) Descriptor() interaction.Descriptor { return h.descriptor }
func (h *fixtureCommandHandler) Prepare(context.Context, interaction.Command) (interaction.Preparation, error) {
	return interaction.Preparation{}, errors.New("read does not prepare")
}
func (h *fixtureCommandHandler) Apply(_ *sql.Tx, c interaction.Command) (interaction.Result, error) {
	h.calls++
	return interaction.Result{State: "succeeded", Scope: c.Scope, Text: "Synthetic subject A only"}, nil
}
