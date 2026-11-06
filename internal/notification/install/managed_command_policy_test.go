package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func TestManagedCommandPolicyNarrowsQueuedAndRunningWork(t *testing.T) {
	ctx := context.Background()
	spec, i, c, active, record := managedCommandsFixture(t)
	i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	enabled, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "all", false)
	check(err)
	open := func(m ManagedManifest) *worker.Store {
		t.Helper()
		s, e := openManagedStore(m, c)
		check(e)
		check(s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
		return s
	}
	s := open(enabled)
	d := c.Worker.Interaction.Destinations[0]
	in := interaction.Inbound{Provider: "telegram", Account: "123", NativeID: "queued", Actor: "789", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local retry subject=a target=one"}
	queued, err := s.AdmitCommand(ctx, in)
	check(err)
	in.NativeID = "running"
	running, err := s.AdmitCommand(ctx, in)
	check(err)
	p := running.Command.Producer
	check(s.ImportCommand(ctx, p, running.Command.ID, protocol.Hash(running.Command)))
	grant, err := s.GrantCommand(ctx, p, running.Command.ID)
	check(err)
	result := interaction.Result{CommandID: running.Command.ID, CommandHash: protocol.Hash(running.Command), Revision: 1, Phase: "execute", State: "running", JobID: "job-one", Scope: running.Command.Scope, Text: "confidential running job"}
	receipt, err := s.SubmitCommandResult(ctx, p, result)
	check(err)
	check(s.AckCommandResult(ctx, p, receipt))
	in.NativeID = "help"
	in.Text = "worker/local help"
	_, err = s.AdmitGatewayQuery(ctx, in)
	check(err)
	check(s.Close())
	if _, err = i.prepareCommands(ctx, "narrow-commands", spec.Root, protocol.Hash(enabled), "", "", false, "[]"); err == nil {
		t.Fatal("narrowing before disable accepted")
	}
	drained, err := i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(enabled), "", "", false)
	check(err)
	if time.Now().Before(grant.ExpiresAt) {
		t.Fatal("grant admission remained open")
	}
	permission := c.Worker.Interaction.Permissions[0]
	permission.Permissions = []string{"read"}
	permission.Scope = interaction.Scope{Subjects: []string{"a"}}
	raw, err := json.Marshal([]interaction.Permission{permission})
	check(err)
	narrowed, err := i.prepareCommands(ctx, "narrow-commands", spec.Root, protocol.Hash(drained), "", "", false, string(raw))
	check(err)
	original, err := json.Marshal(c.Worker.Interaction.Permissions)
	check(err)
	if _, err = i.prepareCommands(ctx, "narrow-commands", spec.Root, protocol.Hash(narrowed), "", "", false, string(original)); err == nil {
		t.Fatal("restored wider permissions")
	}
	if stale, e := openManagedStore(drained, c); e == nil {
		stale.Close()
		t.Fatal("old installation reopened narrowed history")
	}
	enabled, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(narrowed), protocol.Hash(record), "all", false)
	check(err)
	for n := 0; n < 2; n++ {
		s = open(enabled)
		if err = s.ImportCommand(ctx, p, queued.Command.ID, protocol.Hash(queued.Command)); err == nil {
			if g, e := s.GrantCommand(ctx, p, queued.Command.ID); e == nil && g.ID != "" {
				t.Fatal("queued revoked mutation granted")
			}
		}
		if n == 0 {
			raw, _ := json.Marshal(map[string]string{"id": running.Command.ID})
			reply := s.Handle(ctx, protocol.Request{Version: protocol.Version, Operation: "command.get", Expected: s.Identity(), Producer: p, Interaction: raw})
			var retained interaction.View
			if !reply.OK {
				t.Fatal(reply.Error)
			}
			check(protocol.Decode(reply.Result, &retained, protocol.MaxRequest))
			if retained.State != "running" || protocol.Hash(retained.Command) != protocol.Hash(running.Command) {
				t.Fatal("running work lost locally", retained)
			}
		}
		result.Revision = 2
		result.State = "succeeded"
		result.Text = "confidential completed job"
		receipt, err = s.SubmitCommandResult(ctx, p, result)
		check(err)
		check(s.AckCommandResult(ctx, p, receipt))
		check(s.DispatchCommandReplies(ctx, managedReplySender(func(context.Context, interaction.Destination, string) provider.ReplyResult {
			t.Fatal("old private reply leaked")
			return provider.ReplyResult{}
		})))
		in.NativeID = "new-read"
		in.Text = "worker/local status"
		fresh, e := s.AdmitCommand(ctx, in)
		check(e)
		if fresh.Command.Scope.WholeWorker || !fresh.Command.Scope.Contains("a") || fresh.Command.Scope.Contains("b") {
			t.Fatal("new query did not use narrowed scope")
		}
		// Keep the new read reply from becoming an unrelated send in the next pass.
		check(s.DispatchCommandReplies(ctx, managedReplySender(func(context.Context, interaction.Destination, string) provider.ReplyResult {
			return provider.ReplyResult{Result: provider.Result{Accepted: true}, NativeID: "read-reply"}
		})))
		check(s.Close())
	}
	drained, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(enabled), "", "", false)
	check(err)
	revoked, err := i.prepareCommands(ctx, "narrow-commands", spec.Root, protocol.Hash(drained), "", "", false, "[]")
	check(err)
	enabled, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(revoked), protocol.Hash(record), "all", false)
	check(err)
	s = open(enabled)
	defer s.Close()
	in.NativeID = "revoked-read"
	if _, err = s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("revoked actor retained read access")
	}
}

func TestManagedCommandPolicyInterrupted(t *testing.T) {
	for _, point := range []string{"prepared", "command-admission-committed", "manifest-published"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			spec, i, c, active, record := managedCommandsFixture(t)
			i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			check := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			m, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
			check(err)
			m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
			check(err)
			stop := errors.New("interrupted")
			i.fault = func(at string) error {
				if at == point {
					return stop
				}
				return nil
			}
			_, err = i.prepareCommands(ctx, "narrow-commands", spec.Root, protocol.Hash(m), "", "", false, "[]")
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			i.fault = nil
			m, err = i.resume(ctx, spec.Root)
			check(err)
			if err = worker.ValidateManagedCommandPolicyFile(m.Paths.State, c.Worker.Interaction, m.CommandHistoryHash, "[]"); err != nil {
				t.Fatal(err)
			}
			full, _ := json.Marshal(c.Worker.Interaction.Permissions)
			if worker.ValidateManagedCommandPolicyFile(m.Paths.State, c.Worker.Interaction, m.CommandHistoryHash, string(full)) == nil {
				t.Fatal("resume lost revocation")
			}
		})
	}
}

type managedReplySender func(context.Context, interaction.Destination, string) provider.ReplyResult

func (f managedReplySender) SendReply(c context.Context, d interaction.Destination, s string) provider.ReplyResult {
	return f(c, d, s)
}

func TestManagedCommandPolicyCLIDryRunAndCapability(t *testing.T) {
	ctx := context.Background()
	spec, i, c, active, record := managedCommandsFixture(t)
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	m, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
	check(err)
	m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
	check(err)
	unsupported := errors.New("older helper")
	i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return unsupported }
	if _, err = i.prepareCommands(ctx, "narrow-commands", spec.Root, protocol.Hash(m), "", "", false, "[]"); !errors.Is(err, unsupported) {
		t.Fatal(err)
	}
	s, err := openManagedStore(m, c)
	check(err)
	defer s.Close()
	// Keep the runtime lease open. Dry-run must neither open the database nor stop
	// the service, and must still validate the independent immutable policy chain.
	file := filepath.Join(t.TempDir(), "permissions.json")
	check(os.WriteFile(file, []byte("[]\n"), 0600))
	oldDry := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = oldDry }()
	var out bytes.Buffer
	args := []string{"narrow-commands", "--local", "--root", spec.Root, "--manifest-hash", protocol.Hash(m), "--permissions-file", file}
	check(runManagedCommandsCLI(args, &out))
	if out.Len() == 0 {
		t.Fatal("missing dry-run result")
	}
	if err = runManagedCommandsCLI(append(args, "--mode", "all"), &out); err == nil {
		t.Fatal("narrowing accepted enable mode")
	}
	check(os.WriteFile(file, []byte("null"), 0600))
	if err = runManagedCommandsCLI(args, &out); err == nil {
		t.Fatal("null policy accepted")
	}
	check(s.RequireManagedCommandHistory(m.CommandHistoryHash))
}
