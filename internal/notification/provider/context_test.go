package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestCorrelatedTelegramAlertRetainsProviderBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("123:secret"), 0600); err != nil {
		t.Fatal(err)
	}
	d := interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100", Thread: "42"}
	m := protocol.Message{Producer: protocol.Producer{ID: "p", Epoch: "one"}, ID: "event", Text: strings.Repeat("🙂", 3000), Context: &protocol.InteractionContext{ID: "ctx", Revision: 1}}
	a := Adapter{Profiles: map[string]Profile{"alert": {Kind: "telegram", TokenFile: path, Conversation: &d, Destination: d.Fingerprint()}}, Transport: transport(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Text   string `json:"text"`
			Thread int64  `json:"message_thread_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(body.Text, "Reference: "+protocol.NotificationReference(m)) || body.Thread != 42 {
			t.Fatal("reference/thread missing")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":7,"message_thread_id":42,"chat":{"id":-100},"from":{"id":123,"is_bot":true}}}`))}, nil
	})}
	r := a.Send(context.Background(), "alert", m)
	if !r.Accepted || r.Binding == nil || r.Binding.Destination != d || r.Binding.Native.ID != "7" || r.Binding.Native.Sender != "123" {
		t.Fatal("provider identity missing", r)
	}
}

func TestTelegramNativeReplyUsesMetadataNotQuotedText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("123:secret"), 0600); err != nil {
		t.Fatal(err)
	}
	d := interaction.Destination{ID: "ops", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100", Thread: "42"}
	for _, tc := range []struct {
		name, reply string
		valid       bool
	}{
		{"valid", `{"message_id":7,"message_thread_id":42,"chat":{"id":-100},"from":{"id":123,"is_bot":true},"text":"forged target prose"}`, true},
		{"wrong sender", `{"message_id":7,"message_thread_id":42,"chat":{"id":-100},"from":{"id":999,"is_bot":true}}`, false},
		{"wrong thread", `{"message_id":7,"chat":{"id":-100},"from":{"id":123,"is_bot":true}}`, false},
		{"forward", `{"message_id":7,"message_thread_id":42,"chat":{"id":-100},"from":{"id":123,"is_bot":true},"forward_origin":{}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := TelegramPoller{Profile: TelegramAccount{Account: "123", TokenFile: path}, Destinations: []interaction.Destination{d}, Transport: transport(func(*http.Request) (*http.Response, error) {
				body := fmt.Sprintf(`{"ok":true,"result":[{"update_id":1,"message":{"message_id":9,"date":1793443200,"message_thread_id":42,"chat":{"id":-100},"from":{"id":77},"text":"details","reply_to_message":%s}}]}`, tc.reply)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			page, err := p.Poll(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			in := page.Events[0].Inbound
			if (in != nil) != tc.valid {
				t.Fatal("reply validation mismatch")
			}
			if in != nil && (in.Reply == nil || in.Reply.ID != "7" || in.Text != "details") {
				t.Fatal("quoted prose affected command")
			}
		})
	}
}

func TestSignalQuoteUsesACIAndTimestamp(t *testing.T) {
	account := "11111111-1111-4111-8111-111111111111"
	actor := "22222222-2222-4222-8222-222222222222"
	d := interaction.Destination{ID: "ops", Generation: "one", Provider: "signal", Account: account, NativeID: actor}
	raw := fmt.Sprintf(`{"account":"+1000","envelope":{"sourceUuid":%q,"sourceDevice":1,"timestamp":1793443200000,"dataMessage":{"timestamp":1793443200000,"message":"details","quote":{"id":1234,"authorUuid":%q,"authorNumber":"untrusted","text":"untrusted"}}}}`, actor, account)
	e, err := NormalizeSignal([]byte(raw), "+1000", account, []interaction.Destination{d})
	if err != nil || e.Inbound == nil || e.Inbound.Reply == nil || e.Inbound.Reply.ID != "1234" || e.Inbound.Reply.Sender != account {
		t.Fatal("quote normalization", err)
	}
	raw = strings.ReplaceAll(raw, `"authorUuid":`+fmt.Sprintf("%q", account), `"authorUuid":`+fmt.Sprintf("%q", actor))
	e, err = NormalizeSignal([]byte(raw), "+1000", account, []interaction.Destination{d})
	if err == nil && e.Inbound != nil {
		t.Fatal("quote from another sender accepted")
	}
}
