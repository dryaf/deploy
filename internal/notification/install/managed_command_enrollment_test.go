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
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func TestManagedCommandEnrollmentLifecycle(t *testing.T) {
	ctx := context.Background()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	spec, i, c, active, qualification := managedCommandsFixtureUsing(t, func(c *Runtime) {
		d := c.Worker.Interaction.Destinations[0]
		d.ID, d.NativeID, d.Thread = "thread", "-456", "7"
		c.Worker.Interaction.Destinations = append(c.Worker.Interaction.Destinations, d)
		p := c.Worker.Interaction.Permissions[0]
		p.Conversation = d.ID
		c.Worker.Interaction.Permissions = append(c.Worker.Interaction.Permissions, p)
	})
	i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	m, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(qualification), "read", false)
	check(err)
	s, err := openManagedStore(m, c)
	check(err)
	check(s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
	d := c.Worker.Interaction.Destinations[0]
	in := interaction.Inbound{Provider: d.Provider, Account: d.Account, NativeID: "update-1", Actor: "789", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local status"}
	_, err = s.AdmitCommand(ctx, in)
	check(err)
	pending := in
	pending.NativeID = "update-2"
	check(s.PersistTelegram(ctx, provider.IntakePage{Account: in.Account, Next: 3, Events: []provider.IntakeEvent{{ID: 2, Hash: protocol.Hash(pending), Inbound: &pending}}}))
	check(s.Close())
	m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
	check(err)
	var next interaction.Config
	raw, err := json.Marshal(c.Worker.Interaction)
	check(err)
	check(json.Unmarshal(raw, &next))
	next.Revision = "two"
	next.Workers[0].Alias = "worker/renamed"
	next.Destinations = next.Destinations[1:]
	next.Destinations[0].Generation = "two"
	next.Permissions = next.Permissions[1:]
	raw, err = json.Marshal(next)
	check(err)
	old := m
	stop := errors.New("interrupted migration publication")
	i.fault = func(at string) error {
		if at == "manifest-published" {
			return stop
		}
		return nil
	}
	_, err = i.prepareCommands(ctx, "migrate-commands", spec.Root, protocol.Hash(m), "", "", false, string(raw))
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	i.fault = nil
	m, err = i.resume(ctx, spec.Root)
	check(err)
	if m.ConfigHash != old.ConfigHash || m.DeliveryOwnerHash != old.DeliveryOwnerHash || m.DeliveryActivation.Hash != old.DeliveryActivation.Hash {
		t.Fatal("migration replaced original owner or activation")
	}
	if stale, e := openManagedStore(old, c); e == nil {
		stale.Close()
		t.Fatal("old manifest reopened migrated state")
	}
	original, _ := json.Marshal(c.Worker.Interaction)
	if _, err = i.prepareCommands(ctx, "migrate-commands", spec.Root, protocol.Hash(m), "", "", false, string(original)); err == nil {
		t.Fatal("removed enrollment resurrected")
	}
	m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(qualification), "read", false)
	check(err)
	for restart := 0; restart < 2; restart++ {
		s, err = openManagedStore(m, c)
		check(err)
		check(s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
		if got := s.CommandDestinations(); len(got) != 1 || got[0] != next.Destinations[0] {
			t.Fatal("poller would use stale enrollment", got)
		}
		if _, err = s.AdmitCommand(ctx, pending); err == nil {
			t.Fatal("removed pending input admitted")
		}
		check(s.ConvertIntake(ctx))
		check(s.DispatchCommandReplies(ctx, managedReplySender(func(context.Context, interaction.Destination, string) provider.ReplyResult {
			t.Fatal("old reply escaped changed enrollment")
			return provider.ReplyResult{}
		})))
		if restart == 1 {
			fresh := in
			fresh.NativeID = "new-generation"
			fresh.Text = "worker/renamed status"
			fresh.Conversation = next.Destinations[0].ID
			fresh.DestinationFingerprint = next.Destinations[0].Fingerprint()
			v, e := s.AdmitCommand(ctx, fresh)
			check(e)
			if v.Command.Destination != next.Destinations[0] || v.Command.Producer != c.Worker.Producers[0].Producer {
				t.Fatal("new command was redirected")
			}
		}
		check(s.Close())
	}
}

func TestManagedCommandEnrollmentCLIDryRunAndCapability(t *testing.T) {
	ctx := context.Background()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	spec, i, c, m, q := managedCommandsFixture(t)
	var err error
	m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(q), "read", false)
	check(err)
	m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
	check(err)
	next := *c.Worker.Interaction
	next.Revision = "two"
	raw, err := json.Marshal(next)
	check(err)
	unsupported := errors.New("old binary lacks enrollment capability")
	i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return unsupported }
	if _, err = i.prepareCommands(ctx, "migrate-commands", spec.Root, protocol.Hash(m), "", "", false, string(raw)); !errors.Is(err, unsupported) {
		t.Fatal(err)
	}
	s, err := openManagedStore(m, c)
	check(err)
	defer s.Close()
	file := filepath.Join(t.TempDir(), "commands.json")
	check(os.WriteFile(file, raw, 0600))
	previous := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = previous }()
	args := []string{"migrate-commands", "--local", "--root", spec.Root, "--manifest-hash", protocol.Hash(m), "--configuration-file", file}
	var out bytes.Buffer
	check(runManagedCommandsCLI(args, &out))
	for _, extra := range [][]string{{"--mode", "all"}, {"--permissions-file", file}} {
		if runManagedCommandsCLI(append(args, extra...), &out) == nil {
			t.Fatal("conflicting flags accepted")
		}
	}
	check(os.Chmod(file, 0644))
	if runManagedCommandsCLI(args, &out) == nil {
		t.Fatal("public configuration file accepted")
	}
	check(s.RequireManagedCommandHistory(m.CommandHistoryHash))
}
