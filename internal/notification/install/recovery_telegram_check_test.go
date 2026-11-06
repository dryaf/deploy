package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type telegramCheckTransport struct{}

func (telegramCheckTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body string
	switch {
	case strings.HasSuffix(r.URL.Path, "/getWebhookInfo"):
		body = `{"ok":true,"result":{"url":""}}`
	case strings.HasSuffix(r.URL.Path, "/getMe"):
		body = `{"ok":true,"result":{"id":123,"is_bot":true,"username":"ExampleBot"}}`
	case strings.HasSuffix(r.URL.Path, "/getUpdates"):
		body = `{"ok":true,"result":[]}`
	default:
		return nil, fmt.Errorf("unexpected test request")
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func telegramCheckCLIArguments(t *testing.T) ([]string, worker.TelegramRecoveryCursor) {
	return telegramCheckCLIArgumentsBeforeRecovery(t, nil)
}
func telegramCheckCLIArgumentsBeforeRecovery(t *testing.T, before func(*string, string)) ([]string, worker.TelegramRecoveryCursor) {
	return telegramCheckCLIArgumentsConfigured(t, before, nil)
}
func telegramCheckCLIArgumentsConfigured(t *testing.T, before func(*string, string), configure func(*Runtime)) ([]string, worker.TelegramRecoveryCursor) {
	t.Helper()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	c, config := telegramCheckCLIConfiguration(t)
	if configure != nil {
		configure(&c)
	}
	token := filepath.Join(t.TempDir(), "token")
	check(os.WriteFile(token, []byte("123:fixture"), 0600))
	c.Telegram.TokenFile = token
	writeRuntime(t, config, c)
	c, err := ReadRuntime(config)
	check(err)
	state := t.TempDir()
	check(os.Chmod(state, 0700))
	s, err := worker.Open(state, c.Worker, true)
	check(err)
	check(s.PersistTelegram(ctx, provider.IntakePage{Account: c.Telegram.Account}))
	snapshot := filepath.Join(t.TempDir(), "snapshot")
	m, err := s.Snapshot(ctx, snapshot)
	check(err)
	check(s.Close())
	if before != nil {
		before(&state, config)
	}
	q, err := worker.QuarantineRecovery(ctx, state, snapshot, c.Worker, m.ID, m.DatabaseHash)
	check(err)
	_, err = worker.ReconcileTelegramHistory(ctx, state, snapshot, c.Telegram.Account, c.Worker)
	check(err)
	_, err = worker.DrainTelegramRecovery(ctx, state, c.Worker, provider.TelegramPoller{Profile: *c.Telegram, Transport: telegramCheckTransport{}})
	check(err)
	// The inspection and built CLI must never need credentials or poll a provider.
	check(os.Remove(token))
	requests, err := worker.OpenRequestRecoverySession(ctx, state, c.Worker)
	check(err)
	_, err = requests.SealRecoveryRequests(ctx)
	check(err)
	check(requests.Close())
	cursors, err := worker.OpenTelegramRecoveryCursorSession(ctx, state, c.Worker)
	check(err)
	cursor, err := cursors.CommitTelegramRecoveryCursor(ctx, c.Telegram.Account)
	check(err)
	check(cursors.Close())
	for _, p := range c.Worker.Producers {
		_, err = worker.FenceRecoveryOwnership(ctx, state, c.Worker, p.Producer)
		check(err)
	}
	review, err := worker.ReviewRecovery(ctx, state, c.Worker, q.Operation)
	check(err)
	cp, err := worker.CheckpointRecovery(ctx, state, c.Worker, q.Operation, review.ReviewHash)
	check(err)
	proof, err := worker.VerifyRecoveryCheckpoint(ctx, state, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash)
	check(err)
	image, err := worker.PrepareRecoveryPublication(ctx, state, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
	check(err)
	published, err := worker.PublishRecovery(ctx, state, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, image.ImageHash)
	check(err)
	return []string{"--local", "--config", config, "--state-dir", state, "--commit-hash", published.Commit.CommitHash}, cursor
}

func TestTelegramHandoffCheckCLI(t *testing.T) {
	args, cursor := telegramCheckCLIArguments(t)
	time.Sleep(time.Until(cursor.NotBefore.Add(10 * time.Millisecond)))
	for attempt := 0; attempt < 2; attempt++ {
		var out bytes.Buffer
		if err := runRecoveryTelegramCheckCLI(args, &out); err != nil {
			t.Fatal(err)
		}
		var got worker.PublishedTelegramHandoff
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Cursor != cursor || got.IntakeEnabled || !got.RequiresActivationRecheck || got.CommitHash != args[6] {
			t.Fatal(got)
		}
	}
	args[6] = protocol.Hash("wrong")
	var out bytes.Buffer
	if err := runRecoveryTelegramCheckCLI(args, &out); err == nil || out.Len() != 0 {
		t.Fatal("wrong commit emitted success", err, out.String())
	}
}

func telegramRenewalCheckCLIArguments(t *testing.T) ([]string, worker.TelegramHandoffRenewal) {
	t.Helper()
	args, cursor := telegramCheckCLIArguments(t)
	c, err := ReadRuntime(args[2])
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.Telegram.TokenFile, []byte("123:fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	renewal, err := worker.RenewPublishedTelegramHandoff(context.Background(), args[4], c.Worker, args[6], worker.TelegramRenewalRequest{ID: id, PreviousHash: protocol.Hash(cursor)}, provider.TelegramPoller{Profile: *c.Telegram, Transport: telegramCheckTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(c.Telegram.TokenFile); err != nil {
		t.Fatal(err)
	}
	return append(args, "--renewal-hash", renewal.RenewalHash), renewal
}

func TestTelegramRenewalCheckCLI(t *testing.T) {
	args, renewal := telegramRenewalCheckCLIArguments(t)
	time.Sleep(time.Until(renewal.Cursor.NotBefore.Add(10 * time.Millisecond)))
	var out bytes.Buffer
	if err := runRecoveryTelegramCheckCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	var got worker.PublishedTelegramHandoff
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RenewalHash != renewal.RenewalHash || got.Cursor != renewal.Cursor || got.IntakeEnabled || !got.RequiresActivationRecheck {
		t.Fatal(got)
	}
	out.Reset()
	if err := runRecoveryTelegramCheckCLI(args[:7], &out); err == nil || out.Len() != 0 {
		t.Fatal("original fallback accepted after renewal", err, out.String())
	}
	args[8] = protocol.Hash("superseded")
	if err := runRecoveryTelegramCheckCLI(args, &out); err == nil || out.Len() != 0 {
		t.Fatal("wrong renewal emitted success", err, out.String())
	}
}

func telegramCheckCLIConfiguration(t *testing.T) (Runtime, string) {
	t.Helper()
	c, path := runtimeFixture(t)
	c.Worker.Interaction = &interaction.Config{Revision: "one", Destinations: []interaction.Destination{{ID: "private", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"}}, Workers: []interaction.Worker{{Alias: "fixture/local", Producer: c.Worker.Producers[0].Producer, Generation: "one", Commands: map[string]interaction.Descriptor{"status": {ID: "fixture.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "fixture"}}}}}
	c.Telegram = &provider.TelegramAccount{Account: "123", TokenFile: "/private/token", BotName: "ExampleBot"}
	writeRuntime(t, path, c)
	return c, path
}

func TestTelegramHandoffCheckCLIFlagsAndDryRun(t *testing.T) {
	_, config := telegramCheckCLIConfiguration(t)
	state := filepath.Join(t.TempDir(), "absent")
	args := []string{"--local", "--config", config, "--state-dir", state, "--commit-hash", protocol.Hash("pin")}
	old := ui.DryRun
	defer func() { ui.DryRun = old }()
	ui.DryRun = true
	var out bytes.Buffer
	if err := runRecoveryTelegramCheckCLI(args, &out); err != nil || !bytes.Contains(out.Bytes(), []byte("[DRY]")) {
		t.Fatal(err, out.String())
	}
	for _, kind := range []string{"local", "commit", "renewal", "relative", "root", "flag", "positional"} {
		t.Run(kind, func(t *testing.T) {
			a := append([]string(nil), args...)
			switch kind {
			case "local":
				a = a[1:]
			case "commit":
				a[6] = "wrong"
			case "renewal":
				a = append(a, "--renewal-hash", "wrong")
			case "relative":
				a[4] = "relative"
			case "root":
				a[4] = "/"
			case "flag":
				a = append(a, "--activate")
			case "positional":
				a = append(a, "extra")
			}
			out.Reset()
			if err := runRecoveryTelegramCheckCLI(a, &out); err == nil || out.Len() != 0 {
				t.Fatal("invalid inputs emitted success", err, out.String())
			}
		})
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("dry run opened state", err)
	}
	ui.DryRun = false
	out.Reset()
	if err := runRecoveryTelegramCheckCLI(args, &out); err == nil || out.Len() != 0 {
		t.Fatal("missing state emitted success", err, out.String())
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("check created missing state", err)
	}
	_, other := runtimeFixture(t)
	args[2] = other
	ui.DryRun = true
	if err := runRecoveryTelegramCheckCLI(args, &out); err == nil {
		t.Fatal("noninteractive gateway accepted")
	}
}
