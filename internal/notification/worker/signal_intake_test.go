package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
)

type signalRoundTrip func(*http.Request) (*http.Response, error)

func (f signalRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSignalHandoffCommitsBeforeAckAndRetriesLostAck(t *testing.T) {
	s, _, _, in := commandFixture(t)
	ctx := context.Background()
	account := "11111111-1111-4111-8111-111111111111"
	actor := "22222222-2222-4222-8222-222222222222"
	dest := interaction.Destination{ID: "private", Generation: "one", Provider: "signal", Account: account, NativeID: actor}
	s.config.Interaction.Destinations = []interaction.Destination{dest}
	s.config.Interaction.Permissions[0].Actor = actor
	in.Provider = "signal"
	in.Account = account
	in.Actor = actor
	in.Conversation = dest.ID
	in.DestinationFingerprint = dest.Fingerprint()
	in.NativeID = "signal-native-one"
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0700))
	inbox, err := relay.OpenInbox(dir, account, true)
	must(t, err)
	defer inbox.Close()
	must(t, inbox.Enqueue(ctx, interaction.SpoolEvent{Key: in.NativeID, Hash: protocol.Hash(in), Inbound: &in}))
	p := provider.SignalAccount{Account: account, Socket: "/private/relay.sock", InboxIdentity: inbox.Cursor().Identity}
	lost := false
	c := provider.SignalInboxClient{Profile: p, Transport: signalRoundTrip(func(r *http.Request) (*http.Response, error) {
		var cursor interaction.SpoolCursor
		must(t, json.NewDecoder(r.Body).Decode(&cursor))
		var response any
		switch r.URL.Path {
		case "/inbound/list":
			page, err := inbox.List(ctx, cursor)
			must(t, err)
			response = page
		case "/inbound/ack":
			var stored int
			must(t, s.db.QueryRow("SELECT count(*) FROM provider_inbox WHERE account=?", account).Scan(&stored))
			if stored != 1 {
				t.Fatal("relay acknowledged before gateway commit")
			}
			if !lost {
				lost = true
				return nil, errors.New("lost ACK")
			}
			must(t, inbox.Ack(ctx, cursor))
			response = struct {
				OK bool `json:"ok"`
			}{true}
		default:
			t.Fatal(r.URL.Path)
		}
		raw, _ := json.Marshal(response)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(raw))), Header: make(http.Header)}, nil
	})}
	if err = s.PullSignal(ctx, c); err == nil {
		t.Fatal("simulated ACK loss hidden")
	}
	must(t, s.ConvertIntake(ctx))
	must(t, s.PullSignal(ctx, c))
	must(t, s.ConvertIntake(ctx))
	commands, err := s.Commands(ctx, producer)
	must(t, err)
	if len(commands) != 1 || commands[0].Command.ID != in.CommandID() {
		t.Fatal("relay replay duplicated command", len(commands))
	}
	p.InboxIdentity.Recovery = "replacement"
	if _, err = s.signalCursor(ctx, p); err == nil {
		t.Fatal("replacement relay silently adopted")
	}
}
