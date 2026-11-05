package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func signalRuntimeCLIConfiguration(t *testing.T) (Runtime, string) {
	t.Helper()
	c, path := runtimeFixture(t)
	account := "11111111-1111-4111-8111-111111111111"
	c.Worker.Interaction = &interaction.Config{Revision: "one", Destinations: []interaction.Destination{{ID: "private", Generation: "one", Provider: "signal", Account: account, NativeID: "22222222-2222-4222-8222-222222222222"}}, Workers: []interaction.Worker{{Alias: "fixture/local", Producer: c.Worker.Producers[0].Producer, Generation: "one", Commands: map[string]interaction.Descriptor{"status": {ID: "fixture.status", Version: 1, Permission: "read", Effect: "read", Scoped: true, Help: "fixture"}}}}}
	c.Signal = &provider.SignalAccount{Account: account, Socket: "/private/signal.sock", InboxIdentity: protocol.Identity{Worker: "inbox", Recovery: "epoch"}}
	writeRuntime(t, path, c)
	return c, path
}
func signalRuntimeCLIArguments(t *testing.T) []string { return signalRuntimeCLIArgumentsSetup(t, nil) }
func signalRuntimeCLIArgumentsSetup(t *testing.T, beforeRecovery func(string, string)) []string {
	return signalRuntimeCLIArgumentsProviders(t, beforeRecovery, false)
}
func signalRuntimeCLIArgumentsProviders(t *testing.T, beforeRecovery func(string, string), telegram bool) []string {
	return signalRuntimeCLIArgumentsConfigured(t, beforeRecovery, telegram, nil)
}
func signalRuntimeCLIArgumentsConfigured(t *testing.T, beforeRecovery func(string, string), telegram bool, configure func(*Runtime)) []string {
	t.Helper()
	ctx := context.Background()
	c, config := signalRuntimeCLIConfiguration(t)
	if telegram {
		token := filepath.Join(t.TempDir(), "telegram-token")
		if err := os.WriteFile(token, []byte("123:fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		c.Telegram = &provider.TelegramAccount{Account: "123", TokenFile: token, BotName: "ExampleBot"}
		c.Worker.Interaction.Destinations = append(c.Worker.Interaction.Destinations, interaction.Destination{ID: "telegram", Generation: "one", Provider: "telegram", Account: "123", NativeID: "456"})
	}
	state, source, destination := t.TempDir(), t.TempDir(), t.TempDir()
	for _, path := range []string{state, source, destination} {
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	inbox, err := relay.OpenInbox(source, c.Signal.Account, true)
	if err != nil {
		t.Fatal(err)
	}
	c.Signal.InboxIdentity = inbox.Cursor().Identity
	if configure != nil {
		configure(&c)
	}
	writeRuntime(t, config, c)
	if err = inbox.Close(); err != nil {
		t.Fatal(err)
	}
	c, err = ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(state, c.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	if telegram {
		if err = s.PersistTelegram(ctx, provider.IntakePage{Account: c.Telegram.Account}); err != nil {
			t.Fatal(err)
		}
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	m, err := s.Snapshot(ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if beforeRecovery != nil {
		beforeRecovery(config, state)
	}
	q, err := worker.QuarantineRecovery(ctx, state, artifact, c.Worker, m.ID, m.DatabaseHash)
	if err != nil {
		t.Fatal(err)
	}
	src, err := relay.OpenRecoveryInbox(source, c.Signal.Account)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	session, err := worker.OpenSignalRecoverySession(ctx, state, c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = interaction.ReconcileSignal(ctx, src, session); err != nil {
		t.Fatal(err)
	}
	if err = session.Close(); err != nil {
		t.Fatal(err)
	}
	if telegram {
		if _, err = worker.ReconcileTelegramHistory(ctx, state, artifact, c.Telegram.Account, c.Worker); err != nil {
			t.Fatal(err)
		}
		if _, err = worker.DrainTelegramRecovery(ctx, state, c.Worker, provider.TelegramPoller{Profile: *c.Telegram, Transport: telegramCheckTransport{}}); err != nil {
			t.Fatal(err)
		}
	}
	requests, err := worker.OpenRequestRecoverySession(ctx, state, c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = requests.SealRecoveryRequests(ctx); err != nil {
		t.Fatal(err)
	}
	if err = requests.Close(); err != nil {
		t.Fatal(err)
	}
	cursors, err := worker.OpenSignalCursorSession(ctx, state, c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = interaction.PrepareSignalRecoveryHandoff(ctx, src, cursors); err != nil {
		t.Fatal(err)
	}
	if _, err = interaction.InstallSignalRecoveryCursor(ctx, src, cursors); err != nil {
		t.Fatal(err)
	}
	if err = cursors.Close(); err != nil {
		t.Fatal(err)
	}
	if telegram {
		tc, e := worker.OpenTelegramRecoveryCursorSession(ctx, state, c.Worker)
		if e != nil {
			t.Fatal(e)
		}
		_, e = tc.CommitTelegramRecoveryCursor(ctx, c.Telegram.Account)
		ce := tc.Close()
		if e != nil {
			t.Fatal(e)
		}
		if ce != nil {
			t.Fatal(ce)
		}
	}
	if err = src.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range c.Worker.Producers {
		if _, err = worker.FenceRecoveryOwnership(ctx, state, c.Worker, p.Producer); err != nil {
			t.Fatal(err)
		}
	}
	review, err := worker.ReviewRecovery(ctx, state, c.Worker, q.Operation)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := worker.CheckpointRecovery(ctx, state, c.Worker, q.Operation, review.ReviewHash)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := worker.VerifyRecoveryCheckpoint(ctx, state, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash)
	if err != nil {
		t.Fatal(err)
	}
	image, err := worker.PrepareRecoveryPublication(ctx, state, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash)
	if err != nil {
		t.Fatal(err)
	}
	published, err := worker.PublishRecovery(ctx, state, c.Worker, q.Operation, review.ReviewHash, cp.CheckpointHash, proof.ProofHash, image.ImageHash)
	if err != nil {
		t.Fatal(err)
	}
	return []string{"--local", "--config", config, "--state-dir", state, "--commit-hash", published.Commit.CommitHash, "--relay-source", source, "--relay-destination", destination}
}
func TestSignalRuntimeInstallCLI(t *testing.T) {
	args := signalRuntimeCLIArguments(t)
	var one, two bytes.Buffer
	if err := runRecoverySignalInstallCLI(args, &one); err != nil {
		t.Fatal(err)
	}
	if err := runRecoverySignalInstallCLI(args, &two); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one.Bytes(), two.Bytes()) {
		t.Fatal("CLI replay changed installation")
	}
	var installed relay.SignalRuntimeInstallation
	if err := json.Unmarshal(one.Bytes(), &installed); err != nil {
		t.Fatal(err)
	}
	if installed.IntakeEnabled {
		t.Fatal("CLI enabled intake")
	}
	if _, err := relay.VerifyInstalledSignalRuntime(context.Background(), installed.Destination, installed.InstallationHash); err != nil {
		t.Fatal(err)
	}
}
func TestSignalRuntimeInstallCLIFlagsAndDryRun(t *testing.T) {
	_, config := signalRuntimeCLIConfiguration(t)
	base := t.TempDir()
	args := []string{"--local", "--config", config, "--state-dir", filepath.Join(base, "worker"), "--commit-hash", protocol.Hash("pin"), "--relay-source", filepath.Join(base, "source"), "--relay-destination", filepath.Join(base, "destination")}
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	var out bytes.Buffer
	if err := runRecoverySignalInstallCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("[DRY]")) {
		t.Fatal(out.String())
	}
	for _, kind := range []string{"local", "commit", "overlap", "relative", "flag", "positional"} {
		t.Run(kind, func(t *testing.T) {
			a := append([]string(nil), args...)
			switch kind {
			case "local":
				a = a[1:]
			case "commit":
				a[6] = "wrong"
			case "overlap":
				a[10] = a[4]
			case "relative":
				a[8] = "relative"
			case "flag":
				a = append(a, "--activate")
			case "positional":
				a = append(a, "unexpected")
			}
			if err := runRecoverySignalInstallCLI(a, &bytes.Buffer{}); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	if _, err := os.Stat(args[4]); !os.IsNotExist(err) {
		t.Fatal("dry run opened state", err)
	}
}

func TestSignalRuntimeInstallCLIRejectsInboxIdentityDrift(t *testing.T) {
	args := signalRuntimeCLIArguments(t)
	raw, err := os.ReadFile(args[2])
	if err != nil {
		t.Fatal(err)
	}
	var c Runtime
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c.Signal.InboxIdentity.Recovery = "wrong-epoch"
	writeRuntime(t, args[2], c)
	var out bytes.Buffer
	if err = runRecoverySignalInstallCLI(args, &out); err == nil {
		t.Fatal("configured source identity drift accepted")
	}
	if out.Len() != 0 {
		t.Fatal("failed installation emitted success")
	}
	if _, err = os.Stat(filepath.Join(args[8], "signal-runtime-install.json")); !os.IsNotExist(err) {
		t.Fatal("wrong identity fenced source", err)
	}
}
