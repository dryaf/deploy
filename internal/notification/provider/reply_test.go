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

func TestConversationReplyUsesExactThreadAndPinsAccount(t *testing.T) {
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("123:secret"), 0600); err != nil {
		t.Fatal(err)
	}
	d := interaction.Destination{ID: "chat", Generation: "one", Provider: "telegram", Account: "123", NativeID: "-100", Thread: "42"}
	calls := 0
	a := ConversationAdapter{TelegramAccount: "123", TelegramTokenFile: file, Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["chat_id"] != "-100" || payload["message_thread_id"] != float64(42) || payload["parse_mode"] != nil {
			t.Fatal(payload)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"message_id":17,"message_thread_id":42,"chat":{"id":-100}}}`)), Header: make(http.Header)}, nil
	})}
	r := a.SendReply(context.Background(), d, "hello")
	if !r.Accepted || r.NativeID != "17" {
		t.Fatal(r)
	}
	if err := os.WriteFile(file, []byte("456:replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if !a.SendReply(context.Background(), d, "hello").Permanent || calls != 1 {
		t.Fatal("account replacement redirected reply")
	}
	d.Provider = "signal"
	if !a.SendReply(context.Background(), d, "hello").Permanent {
		t.Fatal("unqualified Signal conversation accepted")
	}
}
