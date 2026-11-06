package install

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

type conversationRotationTransport struct {
	t       *testing.T
	polls   int
	replies map[string]int
	token   string
}

func (tr *conversationRotationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.t.Helper()
	if !strings.HasPrefix(r.URL.Path, "/bot"+tr.token+"/") {
		tr.t.Fatal("wrong credential reached transport")
	}
	body := `{"ok":true,"result":[]}`
	if strings.HasSuffix(r.URL.Path, "/getUpdates") {
		tr.polls++
		if tr.polls == 1 {
			body = fmt.Sprintf(`{"ok":true,"result":[
{"update_id":1,"message":{"message_id":11,"date":%[1]d,"text":"worker/local status","from":{"id":789},"chat":{"id":456}}},
{"update_id":2,"message":{"message_id":12,"date":%[1]d,"text":"/worker@ExampleBot worker/local status","message_thread_id":7,"from":{"id":789},"chat":{"id":-456}}},
{"update_id":3,"message":{"message_id":13,"date":%[1]d,"text":"/worker@ExampleBot worker/local status","message_thread_id":8,"from":{"id":789},"chat":{"id":-456}}},
{"update_id":4,"message":{"message_id":14,"date":%[1]d,"text":"/worker@ExampleBot worker/local status","from":{"id":789},"chat":{"id":-456}}},
{"update_id":5,"message":{"message_id":15,"date":%[1]d,"text":"/worker@ExampleBot worker/local status","message_thread_id":9,"from":{"id":789},"chat":{"id":-456}}}]}`, time.Now().Unix())
		}
	} else if strings.HasSuffix(r.URL.Path, "/sendMessage") {
		var payload struct {
			Chat   string `json:"chat_id"`
			Thread int64  `json:"message_thread_id"`
			Text   string `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			tr.t.Fatal(err)
		}
		key := fmt.Sprintf("%s/%d", payload.Chat, payload.Thread)
		if key != "456/0" && key != "-456/0" && key != "-456/7" && key != "-456/8" {
			tr.t.Fatal("reply escaped enrolled conversation", key)
		}
		tr.replies[key]++
		body = fmt.Sprintf(`{"ok":true,"result":{"message_id":777,"message_thread_id":%d,"from":{"id":123,"is_bot":true},"chat":{"id":%s}}}`, payload.Thread, payload.Chat)
	} else {
		return (telegramCheckTransport{}).RoundTrip(r)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

func TestManagedConversationIsolationThroughCredentialRotation(t *testing.T) {
	ctx := context.Background()
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	spec, i, c, active, record := managedCommandsFixtureUsing(t, func(c *Runtime) {
		for _, thread := range []string{"", "7", "8"} {
			d := interaction.Destination{ID: "group" + thread, Generation: "one", Provider: "telegram", Account: "123", NativeID: "-456", Thread: thread}
			c.Worker.Interaction.Destinations = append(c.Worker.Interaction.Destinations, d)
			c.Worker.Interaction.Permissions = append(c.Worker.Interaction.Permissions, interaction.Permission{Actor: "789", Conversation: d.ID, Worker: c.Worker.Producers[0].Producer.ID, Permissions: []string{"read"}, Scope: interaction.Scope{WholeWorker: true}})
		}
	})
	m, err := i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
	check(err)
	dir, err := privateManagedDir(spec.Root, true)
	check(err)
	defer dir.Close()
	checkAdmission := func(ctx context.Context) error { return checkDeliveryAdmission(dir, m, c, time.Now().UTC()) }
	s, err := openManagedStore(m, c)
	check(err)
	check(s.BindManagedCommandAuthority(checkAdmission))
	raw, err := os.ReadFile(c.Telegram.TokenFile)
	check(err)
	tr := &conversationRotationTransport{t: t, token: strings.TrimSpace(string(raw)), replies: map[string]int{}}
	guarded := admittedTelegramTransport{check: checkAdmission, fail: func(error) {}, tokenFile: c.Telegram.TokenFile, tokenHash: managedDigest(raw), next: tr}
	check(s.PullTelegram(ctx, provider.TelegramPoller{Profile: *c.Telegram, Destinations: c.Worker.Interaction.Destinations, Transport: guarded}))
	check(s.ConvertIntake(ctx))
	commands, err := s.Commands(ctx, c.Worker.Producers[0].Producer)
	check(err)
	if len(commands) != 4 {
		t.Fatal("private/thread intake isolation failed", commands)
	}
	ids := map[string]string{}
	for _, v := range commands {
		ids[v.Command.Destination.ID] = v.Command.ID
	}
	if len(ids) != 4 {
		t.Fatal("conversations collapsed", ids)
	}
	pending, err := s.PendingCommandReplies(ctx)
	check(err)
	if len(pending) != 4 {
		t.Fatal("missing durable replies", pending)
	}
	check(s.Close())

	// A same-account credential change invalidates the old admission before any
	// provider request. Then use the supported deactivate/requalify/reactivate
	// lifecycle; retained command identities and destinations must survive it.
	check(os.WriteFile(c.Telegram.TokenFile, []byte("123:rotated\n"), 0600))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot123:rotated/getUpdates", nil)
	check(err)
	if resp, err := guarded.RoundTrip(req); err == nil || resp != nil || tr.polls != 1 {
		t.Fatal("old qualification allowed rotated credential", err)
	}
	after, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(m), "", false)
	check(err)
	if _, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(after), protocol.Hash(record), false); err == nil {
		t.Fatal("old qualification reactivated rotated credential")
	}
	record = freshDeliveryQualification(t, spec.Root)
	active, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(after), protocol.Hash(record), false)
	check(err)
	m, err = i.prepareCommands(ctx, "enable-commands", spec.Root, protocol.Hash(active), protocol.Hash(record), "read", false)
	check(err)
	tr.token, guarded.tokenHash = "123:rotated", managedDigest([]byte("123:rotated\n"))
	for restart := 0; restart < 2; restart++ {
		s, err = openManagedStore(m, c)
		check(err)
		check(s.BindManagedCommandAuthority(checkAdmission))
		check(s.DispatchCommandReplies(ctx, provider.ConversationAdapter{TelegramAccount: c.Telegram.Account, TelegramTokenFile: c.Telegram.TokenFile, Transport: guarded}))
		commands, err = s.Commands(ctx, c.Worker.Producers[0].Producer)
		check(err)
		if len(commands) != 4 {
			t.Fatal("credential rotation lost commands")
		}
		for _, v := range commands {
			if ids[v.Command.Destination.ID] != v.Command.ID {
				t.Fatal("credential rotation remapped request", v)
			}
		}
		check(s.Close())
	}
	if len(tr.replies) != 4 {
		t.Fatal("missing exact replies", tr.replies)
	}
	for key, count := range tr.replies {
		if count != 1 {
			t.Fatal("reply repeated", key, count)
		}
	}
}
