package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type TelegramAccount struct {
	Account   string `json:"account"`
	TokenFile string `json:"token_file"`
	BotName   string `json:"bot_name"`
}
type TelegramPoller struct {
	Profile      TelegramAccount
	Destinations []interaction.Destination
	Transport    http.RoundTripper
}
type IntakeEvent struct {
	ID        int64                `json:"id"`
	Hash      string               `json:"hash"`
	Inbound   *interaction.Inbound `json:"inbound,omitempty"`
	Rejection string               `json:"rejection,omitempty"`
}
type IntakePage struct {
	Account string        `json:"account"`
	Next    int64         `json:"next"`
	Events  []IntakeEvent `json:"events"`
}

// Setup checks are read-only. An existing webhook is a conflict, never deleted.
func (p TelegramPoller) Preflight(ctx context.Context) error {
	var hook struct {
		URL string `json:"url"`
	}
	if err := p.call(ctx, "getWebhookInfo", struct{}{}, &hook); err != nil {
		return err
	}
	if hook.URL != "" {
		return fmt.Errorf("Telegram account has an existing webhook")
	}
	var me struct {
		ID   int64  `json:"id"`
		Bot  bool   `json:"is_bot"`
		Name string `json:"username"`
	}
	if err := p.call(ctx, "getMe", struct{}{}, &me); err != nil {
		return err
	}
	if !me.Bot || strconv.FormatInt(me.ID, 10) != p.Profile.Account || !strings.EqualFold(me.Name, p.Profile.BotName) {
		return fmt.Errorf("Telegram bot identity mismatch")
	}
	return nil
}

func (p TelegramPoller) call(ctx context.Context, method string, input, out any) error {
	token, err := ReadSecret(p.Profile.TokenFile)
	if err != nil || !telegramToken.MatchString(token) {
		return fmt.Errorf("Telegram credential unavailable")
	}
	account, _, _ := strings.Cut(token, ":")
	if account != p.Profile.Account {
		return fmt.Errorf("Telegram account changed")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("Telegram request unavailable")
	}
	req.Header.Set("Content-Type", "application/json")
	transport := p.Transport
	if transport == nil {
		t := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 28 * time.Second, MaxResponseHeaderBytes: 16 << 10}
		defer t.CloseIdleConnections()
		transport = t
	}
	client := http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("Telegram intake unavailable")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxResponse+1))
	if err != nil || len(raw) > protocol.MaxResponse || !utf8.Valid(raw) || safefs.RejectDuplicateJSONKeys(raw) != nil {
		return fmt.Errorf("invalid Telegram envelope")
	}
	if resp.StatusCode == 409 {
		return fmt.Errorf("Telegram receiver conflict")
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if resp.StatusCode/100 != 2 || json.Unmarshal(raw, &env) != nil || !env.OK {
		return fmt.Errorf("Telegram intake rejected")
	}
	if err = json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("invalid Telegram result")
	}
	return nil
}

type telegramMessage struct {
	ID     int64  `json:"message_id"`
	Date   int64  `json:"date"`
	Text   string `json:"text"`
	Thread int64  `json:"message_thread_id"`
	From   *struct {
		ID  int64 `json:"id"`
		Bot bool  `json:"is_bot"`
	} `json:"from"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	SenderChat    json.RawMessage `json:"sender_chat"`
	Forward       json.RawMessage `json:"forward_origin"`
	Reply         json.RawMessage `json:"reply_to_message"`
	ExternalReply json.RawMessage `json:"external_reply"`
}

func (p TelegramPoller) Poll(ctx context.Context, offset int64) (page IntakePage, err error) {
	if offset < 0 {
		return page, fmt.Errorf("invalid intake cursor")
	}
	page.Account = p.Profile.Account
	page.Next = offset
	var updates []json.RawMessage
	err = p.call(ctx, "getUpdates", struct {
		Offset  int64    `json:"offset"`
		Limit   int      `json:"limit"`
		Timeout int      `json:"timeout"`
		Types   []string `json:"allowed_updates"`
	}{offset, 16, 20, []string{"message"}}, &updates)
	if err != nil {
		return page, err
	}
	if len(updates) > 16 {
		return page, fmt.Errorf("Telegram page exceeds limit")
	}
	for _, raw := range updates {
		var update struct {
			ID      *int64           `json:"update_id"`
			Message *telegramMessage `json:"message"`
		}
		if json.Unmarshal(raw, &update) != nil || update.ID == nil || *update.ID < offset || *update.ID >= 1<<63-1 {
			return page, fmt.Errorf("invalid Telegram update identity")
		}
		id := *update.ID
		event := IntakeEvent{ID: id, Hash: protocol.Hash(json.RawMessage(raw)), Rejection: "unsupported_event"}
		m := update.Message
		if m != nil && m.ID > 0 && m.Date > 0 && m.From != nil && m.From.ID > 0 && !m.From.Bot && len(m.SenderChat) == 0 && len(m.Forward) == 0 && len(m.ExternalReply) == 0 && len(m.Text) > 0 && len(m.Text) <= 4096 && utf8.ValidString(m.Text) {
			thread := ""
			if m.Thread > 0 {
				thread = strconv.FormatInt(m.Thread, 10)
			}
			for _, d := range p.Destinations {
				if d.Provider != "telegram" || d.Account != p.Profile.Account || d.NativeID != strconv.FormatInt(m.Chat.ID, 10) || d.Thread != thread {
					continue
				}
				text := m.Text
				first, rest, hasSpace := strings.Cut(text, " ")
				if strings.HasPrefix(first, "/worker") {
					if !hasSpace || (first != "/worker" && !strings.EqualFold(first, "/worker@"+p.Profile.BotName)) {
						break
					}
					text = rest
				}
				event.Rejection = ""
				event.Inbound = &interaction.Inbound{Provider: "telegram", Account: p.Profile.Account, NativeID: "update-" + strconv.FormatInt(id, 10), Actor: strconv.FormatInt(m.From.ID, 10), Conversation: d.ID, DestinationFingerprint: d.Fingerprint(), SentAt: time.Unix(m.Date, 0).UTC(), Text: text}
				if len(m.Reply) > 0 {
					var reply telegramMessage
					if json.Unmarshal(m.Reply, &reply) != nil || reply.ID <= 0 || reply.From == nil || !reply.From.Bot || strconv.FormatInt(reply.From.ID, 10) != p.Profile.Account || reply.Chat.ID != m.Chat.ID || reply.Thread != m.Thread || len(reply.Forward) > 0 || len(reply.SenderChat) > 0 || len(reply.ExternalReply) > 0 {
						event.Inbound = nil
						event.Rejection = "unsupported_reply"
						break
					}
					event.Inbound.Reply = &interaction.NativeReply{ID: strconv.FormatInt(reply.ID, 10), Sender: p.Profile.Account}
				}
				break
			}
		}
		page.Events = append(page.Events, event)
		if id >= page.Next {
			page.Next = id + 1
		}
	}
	return
}
