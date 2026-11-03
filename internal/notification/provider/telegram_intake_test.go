package provider

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
)

func TestTelegramIntakeExplicitAddressingAndRejectedReplies(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("123:secret"), 0600); err != nil {
		t.Fatal(err)
	}
	d := interaction.Destination{ID: "test", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100", Thread: "42"}
	p := TelegramPoller{Profile: TelegramAccount{Account: "123", TokenFile: file, BotName: "ExampleBot"}, Destinations: []interaction.Destination{d}, Transport: transport(func(r *http.Request) (*http.Response, error) {
		var request struct {
			Offset int64 `json:"offset"`
			Limit  int   `json:"limit"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Offset != 5 || request.Limit != 16 {
			t.Fatal(request)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":[{"update_id":5,"message":{"message_id":1,"date":1793443200,"message_thread_id":42,"from":{"id":77,"is_bot":false},"chat":{"id":-100},"text":"/worker@ExampleBot backup/storage status"}},{"update_id":6,"message":{"message_id":2,"date":1793443200,"message_thread_id":42,"from":{"id":77},"chat":{"id":-100},"text":"details","reply_to_message":{"message_id":99}}},{"update_id":7,"edited_message":{"text":"never execute edits"}}]}`)), Header: make(http.Header)}, nil
	})}
	page, err := p.Poll(context.Background(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if page.Next != 8 || len(page.Events) != 3 {
		t.Fatal(page)
	}
	in := page.Events[0].Inbound
	if in == nil || in.Text != "backup/storage status" || in.NativeID != "update-5" || in.Actor != "77" || in.DestinationFingerprint != d.Fingerprint() {
		t.Fatal(in)
	}
	for _, e := range page.Events[1:] {
		if e.Inbound != nil || e.Rejection == "" {
			t.Fatal("unsupported update executed", e)
		}
	}
}

func TestTelegramPreflightNeverDeletesWebhook(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("123:secret"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p := TelegramPoller{Profile: TelegramAccount{Account: "123", TokenFile: file, BotName: "ExampleBot"}, Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/getWebhookInfo") {
			t.Fatal("webhook mutated", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"url":"https://existing.invalid/hook"}}`)), Header: make(http.Header)}, nil
	})}
	if p.Preflight(context.Background()) == nil || calls != 1 {
		t.Fatal("existing webhook accepted")
	}
}
