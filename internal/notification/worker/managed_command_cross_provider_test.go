package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func TestManagedCommandCrossProviderRotationAndRemoval(t *testing.T) {
	ctx := context.Background()
	s := managedCommandHistoryFixture(t)
	clock := s.now
	c := s.config
	c.Interaction.Destinations[0].Account = "123"
	signal := interaction.Destination{ID: "signal-private", Generation: "one", Provider: "signal", Account: "11111111-1111-4111-8111-111111111111", NativeID: "22222222-2222-4222-8222-222222222222"}
	c.Interaction.Destinations = append(c.Interaction.Destinations, signal)
	p := c.Interaction.Permissions[0]
	p.Conversation, p.Actor = signal.ID, signal.NativeID
	c.Interaction.Permissions = append(c.Interaction.Permissions, p)
	must(t, c.Validate())
	s.config = c
	path := s.dir.File().Name()
	read := protocol.Hash("read")
	must(t, s.CommitManagedCommandAdmission("read", read, ""))
	must(t, s.ConfigureManagedCommands("read", read))
	must(t, s.BindManagedCommandAuthority(func(context.Context) error { return nil }))
	telegram := c.Interaction.Destinations[0]
	in := interaction.Inbound{Provider: telegram.Provider, Account: telegram.Account, NativeID: "update-1", Actor: "actor-1", Conversation: telegram.ID, DestinationFingerprint: telegram.Fingerprint(), SentAt: s.now(), Text: "backup/storage status"}
	si := in
	si.Provider, si.Account, si.Actor, si.Conversation, si.DestinationFingerprint = "signal", signal.Account, signal.NativeID, signal.ID, signal.Fingerprint()
	identity := protocol.Identity{Worker: "relay", Recovery: "epoch-one"}
	profile := provider.SignalAccount{Account: signal.Account, Socket: "/private/relay.sock", InboxIdentity: identity}
	must(t, s.PersistTelegram(ctx, provider.IntakePage{Account: in.Account, Next: 2, Events: []provider.IntakeEvent{{ID: 1, Hash: protocol.Hash(in), Inbound: &in}}}))
	must(t, s.PersistSignal(ctx, profile, interaction.SpoolPage{Cursor: interaction.SpoolCursor{Identity: identity, Account: signal.Account, Position: 1}, Events: []interaction.SpoolEvent{{Position: 1, Key: si.NativeID, Hash: protocol.Hash(si), Inbound: &si}}}))
	must(t, s.ConvertIntake(ctx))
	commands, err := s.Commands(ctx, producer)
	must(t, err)
	if len(commands) != 2 || commands[0].Command.ID == commands[1].Command.ID {
		t.Fatal("providers shared request identity", commands)
	}
	token := filepath.Join(t.TempDir(), "token")
	must(t, os.WriteFile(token, []byte("123:original"), 0600))
	must(t, s.Close())
	// A same-account token rotation and gateway reopen must preserve the frozen
	// provider/account/destination on every pending reply.
	must(t, os.WriteFile(token, []byte("123:rotated"), 0600))
	open := func(mode, hash string) *Store {
		t.Helper()
		v, e := Open(path, c, false)
		must(t, e)
		v.now = clock
		v.installationRevision = "n-01234567890123456789012345678901"
		v.deliveryActivationHash = protocol.Hash("delivery")
		must(t, v.ConfigureManagedCommands(mode, hash))
		must(t, v.BindManagedCommandAuthority(func(context.Context) error { return nil }))
		return v
	}
	s = open("read", read)
	counts := map[string]int{}
	adapter := provider.ConversationAdapter{TelegramAccount: "123", TelegramTokenFile: token, Signal: &profile,
		Transport: signalRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "api.telegram.org" || r.URL.Path != "/bot123:rotated/sendMessage" {
				t.Fatal("credential or provider crossed", r.URL)
			}
			var body struct {
				Chat string `json:"chat_id"`
				Text string `json:"text"`
			}
			must(t, json.NewDecoder(r.Body).Decode(&body))
			if body.Chat != telegram.NativeID {
				t.Fatal("Telegram reply redirected", body)
			}
			counts["telegram"]++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":7,"from":{"id":123,"is_bot":true},"chat":{"id":-100}}}`)), Header: http.Header{}}, nil
		}),
		SignalTransport: signalRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "relay" || r.URL.Path != "/reply" {
				t.Fatal("Signal used Telegram transport", r.URL)
			}
			var body struct {
				Identity    protocol.Identity `json:"identity"`
				Account     string            `json:"account"`
				Destination string            `json:"destination"`
				Fingerprint string            `json:"fingerprint"`
				Text        string            `json:"text"`
			}
			must(t, json.NewDecoder(r.Body).Decode(&body))
			if body.Identity != identity || body.Account != signal.Account || body.Destination != signal.ID || body.Fingerprint != signal.Fingerprint() {
				t.Fatal("Signal reply redirected", body)
			}
			counts["signal"]++
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"accepted":true,"native_id":"signal-reply"}`)), Header: http.Header{}}, nil
		}),
	}
	must(t, s.DispatchCommandReplies(ctx, adapter))
	if counts["telegram"] != 1 || counts["signal"] != 1 {
		t.Fatal("missing isolated replies", counts)
	}
	// Queue both providers again, then remove Telegram and rotate the retained
	// Signal conversation generation. No old reply may be forwarded to it.
	in.NativeID, si.NativeID = "update-2", "update-2"
	_, err = s.AdmitCommand(ctx, in)
	must(t, err)
	_, err = s.AdmitCommand(ctx, si)
	must(t, err)
	drain := protocol.Hash("drain")
	must(t, s.CommitManagedCommandAdmission("drain", drain, read))
	must(t, s.Close())
	s = open("drain", drain)
	var next interaction.Config
	must(t, json.Unmarshal([]byte(commandEnrollmentJSON(t, c.Interaction)), &next))
	next.Revision = "two"
	next.Destinations = next.Destinations[1:]
	next.Destinations[0].Generation = "two"
	next.Permissions = next.Permissions[1:]
	migrated := protocol.Hash("migration")
	must(t, s.CommitManagedCommandPolicy(migrated, drain, commandEnrollmentJSON(t, &next)))
	enabled := protocol.Hash("new-read")
	must(t, s.CommitManagedCommandAdmission("read", enabled, migrated))
	must(t, s.Close())
	for n := 0; n < 2; n++ {
		s = open("read", enabled)
		must(t, s.DispatchCommandReplies(ctx, adapter))
		if counts["telegram"] != 1 || counts["signal"] != 1 {
			t.Fatal("retired reply forwarded", counts)
		}
		if _, err = s.AdmitCommand(ctx, in); err == nil {
			t.Fatal("removed Telegram enrollment admitted")
		}
		if _, err = s.AdmitCommand(ctx, si); err == nil {
			t.Fatal("old Signal generation admitted")
		}
		if n == 1 {
			fresh := si
			fresh.NativeID = "fresh"
			fresh.DestinationFingerprint = next.Destinations[0].Fingerprint()
			v, e := s.AdmitCommand(ctx, fresh)
			must(t, e)
			if v.Command.Destination != next.Destinations[0] {
				t.Fatal(v)
			}
		}
		must(t, s.Close())
	}
}
