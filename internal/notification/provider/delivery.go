// Package provider contains messenger transports, independent of producer policy.
package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type Result struct {
	Accepted, Ambiguous, Permanent bool
	RetryAfter                     time.Duration
	Binding                        *interaction.DeliveryBinding
}

type Profile struct {
	Conversation  *interaction.Destination `json:"conversation,omitempty"`
	RelayIdentity protocol.Identity        `json:"relay_identity,omitempty"`
	Kind          string                   `json:"kind"`
	TokenFile     string                   `json:"token_file,omitempty"`
	ChatFile      string                   `json:"chat_file,omitempty"`
	Socket        string                   `json:"socket,omitempty"`
	// Destination is an enrolled fingerprint, never a plaintext recipient.
	Destination string `json:"destination"`
}

type Adapter struct {
	Profiles        map[string]Profile
	Transport       http.RoundTripper
	SignalTransport http.RoundTripper
}

func ReadSecret(path string) (string, error) {
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("credential unavailable")
	}
	defer d.Close()
	f, err := d.Open(filepath.Base(path), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("credential unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("credential must be private")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return "", fmt.Errorf("credential exceeds bounds")
	}
	v := strings.TrimSpace(string(b))
	if v == "" || strings.ContainsAny(v, "\x00\r\n") {
		return "", fmt.Errorf("invalid credential")
	}
	return v, nil
}

var telegramToken = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)

func TelegramIdentity(token, chat string) string {
	// Telegram's bot identifier precedes ':'. Rotating the secret of that same
	// bot does not redirect queued work to a different account.
	bot, _, _ := strings.Cut(token, ":")
	h := sha256.Sum256([]byte("telegram\x00" + bot + "\x00" + chat))
	return hex.EncodeToString(h[:])
}

func (a Adapter) Send(ctx context.Context, name string, m protocol.Message) Result {
	p, ok := a.Profiles[name]
	if !ok {
		return Result{Permanent: true}
	}
	if p.Conversation != nil {
		d := *p.Conversation
		if d.Provider != p.Kind || d.Fingerprint() != p.Destination {
			return Result{Permanent: true}
		}
		adapter := ConversationAdapter{Transport: a.Transport, SignalTransport: a.SignalTransport, TelegramAccount: d.Account, TelegramTokenFile: p.TokenFile}
		if p.Kind == "signal" {
			adapter.Signal = &SignalAccount{Account: d.Account, Socket: p.Socket, InboxIdentity: p.RelayIdentity}
		}
		text := m.Text
		if m.Context != nil {
			text += "\nReference: " + protocol.NotificationReference(m)
		}
		r := adapter.SendReply(ctx, d, text)
		result := r.Result
		if r.Accepted && r.Sender == d.Account && protocol.ValidToken(r.NativeID) {
			result.Binding = &interaction.DeliveryBinding{Destination: d, Native: interaction.NativeReply{ID: r.NativeID, Sender: r.Sender}}
		}
		return result
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ref := sha256.Sum256([]byte(m.Producer.ID + "\x00" + m.ID))
	message := m.Text + fmt.Sprintf("\n[event %x]", ref[:6])
	var endpoint string
	var payload any
	transport := a.Transport
	switch p.Kind {
	case "telegram":
		token, err := ReadSecret(p.TokenFile)
		if err != nil || !telegramToken.MatchString(token) {
			return Result{Permanent: true}
		}
		chat, err := ReadSecret(p.ChatFile)
		if err != nil || TelegramIdentity(token, chat) != p.Destination {
			return Result{Permanent: true}
		}
		endpoint = "https://api.telegram.org/bot" + token + "/sendMessage"
		payload = struct {
			Chat string `json:"chat_id"`
			Text string `json:"text"`
		}{chat, message}
	case "signal":
		if a.SignalTransport != nil {
			transport = a.SignalTransport
		}
		if !filepath.IsAbs(p.Socket) || p.Destination == "" {
			return Result{Permanent: true}
		}
		endpoint = "http://relay/send"
		payload = struct {
			ID          string `json:"id"`
			Message     string `json:"message"`
			Destination string `json:"destination"`
		}{m.ID, message, p.Destination}
		if transport == nil {
			transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", p.Socket)
			}, ResponseHeaderTimeout: 9 * time.Second, MaxResponseHeaderBytes: 16 << 10}
		}
	default:
		return Result{Permanent: true}
	}
	if transport == nil {
		transport = &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10}
	}
	if a.Transport == nil {
		if t, ok := transport.(*http.Transport); ok {
			defer t.CloseIdleConnections()
		}
	}
	client := http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	body, err := json.Marshal(payload)
	if err != nil {
		return Result{Permanent: true}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{Permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Result{Ambiguous: true}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10+1))
	if err != nil || len(b) > 16<<10 {
		return Result{Ambiguous: true}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return Result{Permanent: true}
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return Result{Permanent: true}
	}
	if p.Kind == "signal" {
		if resp.StatusCode == 503 {
			return Result{RetryAfter: time.Second}
		}
		if resp.StatusCode == 409 {
			return Result{Permanent: true}
		}
		var result struct {
			Accepted bool `json:"accepted"`
		}
		if resp.StatusCode/100 != 2 || protocol.Decode(b, &result, 16<<10) != nil || !result.Accepted {
			return Result{Ambiguous: true}
		}
		return Result{Accepted: true}
	}
	var result struct {
		OK         *bool `json:"ok"`
		ErrorCode  int   `json:"error_code"`
		Parameters struct {
			RetryAfter int64 `json:"retry_after"`
		} `json:"parameters"`
	}
	// Provider envelopes may add fields; duplicate keys and UTF-8 are still checked.
	if !utf8.Valid(b) || safefs.RejectDuplicateJSONKeys(b) != nil || json.Unmarshal(b, &result) != nil {
		return Result{Ambiguous: true}
	}
	if resp.StatusCode == 429 || result.ErrorCode == 429 {
		if result.Parameters.RetryAfter <= 0 || result.Parameters.RetryAfter > 86400 {
			return Result{Permanent: true}
		}
		return Result{RetryAfter: time.Duration(result.Parameters.RetryAfter) * time.Second}
	}
	if result.OK == nil {
		return Result{Ambiguous: true}
	}
	if resp.StatusCode/100 == 2 && *result.OK {
		return Result{Accepted: true}
	}
	if resp.StatusCode == 400 || result.ErrorCode == 400 {
		return Result{Permanent: true}
	}
	return Result{}
}
