package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func TestVerificationCLIFlagScopeAndDryRun(t *testing.T) {
	_, config := runtimeFixture(t)
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	state := filepath.Join(t.TempDir(), "not-created")
	if err := RunCLI([]string{"serve", "--local", "--verification", "--config", config, "--state-dir", state, "--socket", filepath.Join(state, "api.sock")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("dry run created state", err)
	}
	if err := RunCLI([]string{"init", "--local", "--verification", "--config", config, "--state-dir", state}); err == nil {
		t.Fatal("verification flag accepted for unrelated operation")
	}
}

func TestVerificationRuntimeServesOnlyDurableAPI(t *testing.T) {
	c, _ := runtimeFixture(t)
	p := c.Worker.Producers[0].Producer
	c.Worker.Interaction = &interaction.Config{Revision: "one", Workers: []interaction.Worker{{Alias: "fixture/local", Producer: p, Generation: "one", Commands: map[string]interaction.Descriptor{"status": {ID: "fixture.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "fixture"}}}}, Destinations: []interaction.Destination{{ID: "private", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}}}
	// These configured transports must remain unused in verification mode.
	c.Telegram = &provider.TelegramAccount{Account: "123", TokenFile: "/nonexistent/verification-token", BotName: "ExampleBot"}
	dir, err := os.MkdirTemp("", "verify-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s, err := worker.Open(dir, c.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{s.Stage(p, "one"), s.Activate(p, "one")} {
		if err != nil {
			s.Close()
			t.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = worker.OpenVerification(dir, c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	socket := filepath.Join(dir, "api.sock")
	done := make(chan error, 1)
	go func() { done <- serveRuntime(ctx, s, c, socket) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	api := client.Client{Socket: socket, Identity: s.Identity()}
	var health worker.Health
	for {
		if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("verification API unavailable", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	now := time.Now().UTC()
	snapshot := protocol.Snapshot{Producer: p, Sequence: 1, BasisRevision: 1, ConfigRevision: "one", EvaluatedAt: now, Subjects: []protocol.Subject{{Key: "subject", Condition: "ok", Reason: "synthetic verification", ValidUntil: now.Add(time.Minute)}}}
	if _, err = api.Call(ctx, protocol.Request{Operation: "status.replace", Producer: p, Snapshot: &snapshot}, nil); err != nil {
		t.Fatal(err)
	}
	m := protocol.Message{Producer: p, Sequence: 1, ID: "staged-message", Generation: "one", Subject: "subject", Kind: "report", Severity: "info", ObservedAt: now, ExpiresAt: now.Add(time.Minute), Text: "Synthetic", Route: "ops", ReportKey: "day", ReportType: "daily", ReportRevision: 1, BasisRevision: 1}
	if _, err = api.Call(ctx, protocol.Request{Operation: "message.enqueue", Producer: p, Message: &m}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &health); err != nil {
		t.Fatal(err)
	}
	if !health.VerificationOnly || len(health.Loops) != 0 || health.Condition == "ok" {
		t.Fatal(health)
	}
	if err = (userManagedServices{}).Verify(ctx, ManagedManifest{Identity: s.Identity(), Revision: "n-00000000000000000000000000000001", Paths: ServicePaths{Socket: socket}}); err == nil {
		t.Fatal("unmanaged verification process qualified a managed release")
	}
	j, err := s.Get(ctx, p, m.ID)
	if err != nil || j.Tries != 0 || j.Attempt != "" {
		t.Fatal("verification dispatched", j, err)
	}
}
