package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/safefs"
)

type ReplyResult struct {
	Result
	NativeID string
	Sender   string
}

func ValidConversationText(text string) bool {
	return len(text) > 0 && len(text) <= 16000 && utf8.ValidString(text) && utf8.RuneCountInString(text) <= 4000
}

// ConversationAdapter resolves accounts separately from alert recipients. The
// gateway supplies the exact enrolled conversation; there is no default chat.
// Signal conversations require a separately pinned relay account and inbox.
type ConversationAdapter struct {
	TelegramAccount   string
	TelegramTokenFile string
	Transport         http.RoundTripper
	Signal            *SignalAccount
}

func (a ConversationAdapter) SendReply(ctx context.Context, d interaction.Destination, text string) ReplyResult {
	if d.Provider == "signal" && a.Signal != nil {
		return (SignalInboxClient{Profile: *a.Signal, Transport: a.Transport}).SendReply(ctx, d, text)
	}
	fail := func() ReplyResult { return ReplyResult{Result: Result{Permanent: true}} }
	if d.Provider != "telegram" || d.Account != a.TelegramAccount || !ValidConversationText(text) {
		return fail()
	}
	chat, err := strconv.ParseInt(d.NativeID, 10, 64)
	if err != nil || chat == 0 || strconv.FormatInt(chat, 10) != d.NativeID {
		return fail()
	}
	var thread int64
	if d.Thread != "" {
		thread, err = strconv.ParseInt(d.Thread, 10, 64)
		if err != nil || thread <= 0 || strconv.FormatInt(thread, 10) != d.Thread {
			return fail()
		}
	}
	token, err := ReadSecret(a.TelegramTokenFile)
	if err != nil || !telegramToken.MatchString(token) {
		return fail()
	}
	account, _, _ := strings.Cut(token, ":")
	if account != d.Account {
		return fail()
	}
	body, _ := json.Marshal(struct {
		Chat   string `json:"chat_id"`
		Thread int64  `json:"message_thread_id,omitempty"`
		Text   string `json:"text"`
	}{d.NativeID, thread, text})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return fail()
	}
	req.Header.Set("Content-Type", "application/json")
	transport := a.Transport
	if transport == nil {
		t := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10}
		defer t.CloseIdleConnections()
		transport = t
	}
	client := http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return ReplyResult{Result: Result{Ambiguous: true}}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return ReplyResult{Result: Result{Ambiguous: true}}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fail()
	}
	var wire struct {
		OK         *bool `json:"ok"`
		ErrorCode  int   `json:"error_code"`
		Parameters struct {
			RetryAfter int64 `json:"retry_after"`
		} `json:"parameters"`
		Result struct {
			From *struct {
				ID  int64 `json:"id"`
				Bot bool  `json:"is_bot"`
			} `json:"from"`
			ID     int64 `json:"message_id"`
			Thread int64 `json:"message_thread_id"`
			Chat   struct {
				ID int64 `json:"id"`
			} `json:"chat"`
		} `json:"result"`
	}
	if !utf8.Valid(raw) || safefs.RejectDuplicateJSONKeys(raw) != nil || json.Unmarshal(raw, &wire) != nil {
		return ReplyResult{Result: Result{Ambiguous: true}}
	}
	if resp.StatusCode == 429 || wire.ErrorCode == 429 {
		if wire.Parameters.RetryAfter <= 0 || wire.Parameters.RetryAfter > 86400 {
			return fail()
		}
		return ReplyResult{Result: Result{RetryAfter: time.Duration(wire.Parameters.RetryAfter) * time.Second}}
	}
	if wire.OK == nil {
		return ReplyResult{Result: Result{Ambiguous: true}}
	}
	if resp.StatusCode/100 == 2 && *wire.OK {
		if wire.Result.ID <= 0 || wire.Result.Chat.ID != chat || wire.Result.Thread != thread {
			return ReplyResult{Result: Result{Ambiguous: true}}
		}
		sender := ""
		if wire.Result.From != nil {
			if !wire.Result.From.Bot || strconv.FormatInt(wire.Result.From.ID, 10) != d.Account {
				return ReplyResult{Result: Result{Ambiguous: true}}
			}
			sender = d.Account
		}
		return ReplyResult{Result: Result{Accepted: true}, NativeID: strconv.FormatInt(wire.Result.ID, 10), Sender: sender}
	}
	if resp.StatusCode == 400 || wire.ErrorCode == 400 {
		return fail()
	}
	return ReplyResult{}
}
