package provider

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTelegramResponseClassification(t *testing.T) {
	dir := t.TempDir()
	token, chat := filepath.Join(dir, "token"), filepath.Join(dir, "chat")
	if err := os.WriteFile(token, []byte("123:secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chat, []byte("456"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		status int
		body   string
		want   Result
	}{
		{"accepted", 200, `{"ok":true,"result":{"message_id":17}}`, Result{Accepted: true}},
		{"not accepted", 200, `{"ok":false}`, Result{}},
		{"rate limit", 429, `{"ok":false,"parameters":{"retry_after":30}}`, Result{RetryAfter: 30 * time.Second}},
		{"bad rate limit", 429, `{"parameters":{"retry_after":-1}}`, Result{Permanent: true}},
		{"redirect", 302, ``, Result{Permanent: true}},
		{"unauthorized", 401, ``, Result{Permanent: true}},
		{"truncated", 200, `{"ok":`, Result{Ambiguous: true}},
		{"missing status", 200, `{}`, Result{Ambiguous: true}},
		{"duplicate", 200, `{"ok":false,"ok":true}`, Result{Ambiguous: true}},
		{"too large", 200, strings.Repeat("x", 16385), Result{Ambiguous: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := Adapter{Profiles: map[string]Profile{"tg": {Kind: "telegram", TokenFile: token, ChatFile: chat, Destination: TelegramIdentity("123:secret", "456")}}, Transport: transport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Scheme != "https" || r.Method != "POST" {
					t.Fatal("unsafe request")
				}
				return &http.Response{StatusCode: test.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body)), Request: r}, nil
			})}
			if got := a.Send(context.Background(), "tg", protocol.Message{ID: "event", Text: "hello"}); got != test.want {
				t.Fatalf("got %+v want %+v", got, test.want)
			}
		})
	}
}

func TestRecipientChangeBlocksWithoutRequest(t *testing.T) {
	dir := t.TempDir()
	token, chat := filepath.Join(dir, "token"), filepath.Join(dir, "chat")
	if err := os.WriteFile(token, []byte("123:secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chat, []byte("new-chat"), 0600); err != nil {
		t.Fatal(err)
	}
	a := Adapter{Profiles: map[string]Profile{"tg": {Kind: "telegram", TokenFile: token, ChatFile: chat, Destination: TelegramIdentity("123:secret", "old-chat")}}, Transport: transport(func(*http.Request) (*http.Response, error) { t.Fatal("redirected queued message"); return nil, nil })}
	if !a.Send(context.Background(), "tg", protocol.Message{Text: "hello"}).Permanent {
		t.Fatal("recipient drift accepted")
	}
}
