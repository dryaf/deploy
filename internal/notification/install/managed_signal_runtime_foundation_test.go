package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
)

// The native command callback is synthetic: no signal-cli or provider is run.
// This exercises the shared account/inbox/runtime contract over its real socket.
func TestManagedSignalRuntimeFoundationReplyAndRevocation(t *testing.T) {
	spec := signalVerificationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := PrepareSignalVerification(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ReadRuntime(filepath.Join(spec.Root, signalVerificationGateway))
	if err != nil {
		t.Fatal(err)
	}
	var admitted atomic.Bool
	admitted.Store(true)
	check := func(context.Context) error {
		if !admitted.Load() {
			return errors.New("withdrawn")
		}
		return nil
	}
	account, err := signalaccount.OpenRecoveryRuntime(spec.AccountState, spec.Recovery, func() error { return check(ctx) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(account.Close)
	inbox, err := relay.OpenManagedSignalInbox(ctx, spec.InboxDestination, m.Installation.InstallationHash, m.receipt(), check)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { inbox.Close() })
	files := t.TempDir()
	accountFile, recipientFile := filepath.Join(files, "account"), filepath.Join(files, "recipient")
	if err = os.WriteFile(accountFile, []byte(c.Signal.Account), 0600); err != nil {
		t.Fatal(err)
	}
	d := c.Worker.Interaction.Destinations[0]
	if err = os.WriteFile(recipientFile, []byte(d.NativeID), 0600); err != nil {
		t.Fatal(err)
	}
	config := relay.Config{StateDir: spec.AccountState, Socket: c.Signal.Socket, AccountFile: accountFile, RecipientFile: recipientFile, Interactive: &relay.InteractiveConfig{Version: "0.14.9", Account: c.Signal.Account, InboxDir: spec.InboxDestination, Destinations: c.Worker.Interaction.Destinations}}
	var sends atomic.Int32
	execution := relay.ManagedExecution{Account: account, Inbox: inbox, Check: check, Command: func(ctx context.Context, lease *os.File, w io.Writer, args ...string) error {
		if lease == nil {
			return errors.New("missing account descriptor")
		}
		if _, err := lease.Stat(); err != nil {
			return err
		}
		if slices.Contains(args, "--version") {
			_, err := fmt.Fprintln(w, "signal-cli 0.14.9")
			return err
		}
		if slices.Contains(args, "listAccounts") {
			_, err := fmt.Fprintf(w, `[{"number":%q,"aci":%q}]`, c.Signal.Account, c.Signal.Account)
			return err
		}
		if slices.Contains(args, "send") {
			sends.Add(1)
			_, err := io.WriteString(w, `{"timestamp":123456789}`)
			return err
		}
		if slices.Contains(args, "receive") {
			return nil
		}
		return errors.New("unexpected synthetic command")
	}}
	done := make(chan error, 1)
	go func() { done <- relay.RunManaged(ctx, config, execution) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("managed relay did not join")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", config.Socket, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("managed socket unavailable", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	adapter := provider.ConversationAdapter{Signal: c.Signal}
	result := adapter.SendReply(ctx, d, "local fixture reply")
	if !result.Accepted || result.NativeID != "123456789" || sends.Load() != 1 {
		t.Fatal(result, sends.Load())
	}
	admitted.Store(false)
	result = adapter.SendReply(ctx, d, "must not execute")
	if result.Accepted || sends.Load() != 1 {
		t.Fatal("revoked runtime executed", result, sends.Load())
	}
	// Cleanup waits for the runtime before releasing either retained resource.
}
