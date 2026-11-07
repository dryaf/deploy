package install

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestManagedCommandGrantLifecycle(t *testing.T) {
	for _, point := range []string{"prepared", "command-admission-committed", "manifest-published", "old-helper"} {
		t.Run(point, func(t *testing.T) {
			ctx := context.Background()
			spec, i, c, m, q := managedCommandsFixture(t)
			i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			m, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(q), "read", false)
			testutil.Must(t, err)
			m, err = i.prepareCommands(ctx, "disable-commands", spec.Root, protocol.Hash(m), "", "", false)
			testutil.Must(t, err)
			m, err = i.prepareCommands(ctx, "narrow-commands", spec.Root, protocol.Hash(m), "", "", false, "[]")
			testutil.Must(t, err)
			next := *c.Worker.Interaction
			next.Revision = "restored"
			raw, err := json.Marshal(next)
			testutil.Must(t, err)
			request, err := ReviewCommandPermissionRestore(ctx, spec.Root, protocol.Hash(m), string(raw))
			testutil.Must(t, err)
			old := m
			stop := errors.New("interrupted")
			if point == "old-helper" {
				i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return stop }
			} else {
				i.fault = func(at string) error {
					if at == point {
						return stop
					}
					return nil
				}
			}
			_, err = i.ensureCommandPolicy(ctx, request)
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if point == "old-helper" {
				s, e := openManagedStore(old, c)
				testutil.Must(t, e)
				testutil.Must(t, s.Close())
			}
			i.fault = nil
			i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			m, err = i.ensureCommandPolicy(ctx, request)
			testutil.Must(t, err)
			observed, done, err := i.inspectCommandPolicy(ctx, request)
			testutil.Must(t, err)
			if !done || protocol.Hash(observed) != protocol.Hash(m) || m.Commands.Mode != "drain" {
				t.Fatal("restoration not retained", m)
			}
			if m.ConfigHash != old.ConfigHash || m.DeliveryOwnerHash != old.DeliveryOwnerHash || *m.DeliveryActivation != *old.DeliveryActivation {
				t.Fatal("restoration changed delivery")
			}
			// Descendants must keep grant-capable helpers even when the latest operation
			// is an ordinary enable.
			i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return stop }
			_, e := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(q), "read", false)
			if !errors.Is(e, stop) {
				t.Fatal("descendant skipped restoration capability", e)
			}
			i.commandPolicyProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(m), protocol.Hash(q), "read", false)
			testutil.Must(t, err)
			s, err := openManagedStore(m, c)
			testutil.Must(t, err)
			defer s.Close()
			testutil.Must(t, s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
			d := next.Destinations[0]
			in := interaction.Inbound{Provider: d.Provider, Account: d.Account, NativeID: "restored-query", Actor: "789", Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Now().UTC(), Text: "worker/local status"}
			_, err = s.AdmitCommand(ctx, in)
			testutil.Must(t, err)
		})
	}
}
