package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type SignalAccount struct {
	Account       string            `json:"account"`
	Socket        string            `json:"socket"`
	InboxIdentity protocol.Identity `json:"inbox_identity"`
}

func (p SignalAccount) Validate() error {
	if !ValidSignalAccount(p.Account) || !filepath.IsAbs(p.Socket) || !p.InboxIdentity.Valid() {
		return fmt.Errorf("invalid interactive Signal account")
	}
	return nil
}

type SignalInboxClient struct {
	Profile   SignalAccount
	Transport http.RoundTripper
}

func (c SignalInboxClient) request(ctx context.Context, path string, input any) ([]byte, int, error) {
	if c.Profile.Validate() != nil {
		return nil, 0, fmt.Errorf("invalid Signal relay configuration")
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://relay"+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	transport := c.Transport
	if transport == nil {
		t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", c.Profile.Socket)
		}, ResponseHeaderTimeout: 9 * time.Second, MaxResponseHeaderBytes: 4096}
		defer t.CloseIdleConnections()
		transport = t
	}
	client := http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("Signal relay unavailable")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxResponse+1))
	if err != nil || len(raw) > protocol.MaxResponse {
		return nil, resp.StatusCode, fmt.Errorf("Signal response exceeds budget")
	}
	return raw, resp.StatusCode, nil
}
func (c SignalInboxClient) List(ctx context.Context, cursor interaction.SpoolCursor) (page interaction.SpoolPage, err error) {
	if cursor.Account != c.Profile.Account || cursor.Identity != c.Profile.InboxIdentity {
		return page, fmt.Errorf("Signal cursor pin mismatch")
	}
	raw, status, err := c.request(ctx, "/inbound/list", cursor)
	if err != nil {
		return page, err
	}
	if status != 200 {
		return page, fmt.Errorf("Signal relay refused cursor")
	}
	if err = protocol.Decode(raw, &page, protocol.MaxResponse); err != nil {
		return page, err
	}
	if page.Cursor.Identity != cursor.Identity || page.Cursor.Account != cursor.Account || page.Cursor.Position < cursor.Position || len(page.Events) > 16 {
		return page, fmt.Errorf("invalid Signal relay page")
	}
	return
}
func (c SignalInboxClient) Ack(ctx context.Context, cursor interaction.SpoolCursor) error {
	if cursor.Account != c.Profile.Account || cursor.Identity != c.Profile.InboxIdentity {
		return fmt.Errorf("Signal ACK pin mismatch")
	}
	raw, status, err := c.request(ctx, "/inbound/ack", cursor)
	if err != nil {
		return err
	}
	var response struct {
		OK bool `json:"ok"`
	}
	if status != 200 || protocol.Decode(raw, &response, 4096) != nil || !response.OK {
		return fmt.Errorf("Signal ACK unconfirmed")
	}
	return nil
}
func (c SignalInboxClient) SendReply(ctx context.Context, d interaction.Destination, text string) ReplyResult {
	if d.Provider != "signal" || d.Account != c.Profile.Account || !ValidConversationText(text) {
		return ReplyResult{Result: Result{Permanent: true}}
	}
	raw, status, err := c.request(ctx, "/reply", struct {
		Identity    protocol.Identity `json:"identity"`
		Account     string            `json:"account"`
		Destination string            `json:"destination"`
		Fingerprint string            `json:"fingerprint"`
		Text        string            `json:"text"`
	}{c.Profile.InboxIdentity, d.Account, d.ID, d.Fingerprint(), text})
	if err != nil {
		return ReplyResult{Result: Result{Ambiguous: true}}
	}
	if status == 503 {
		return ReplyResult{Result: Result{RetryAfter: time.Second}}
	}
	if status == 400 || status == 404 || status == 409 {
		return ReplyResult{Result: Result{Permanent: true}}
	}
	var response struct {
		Accepted bool   `json:"accepted"`
		NativeID string `json:"native_id"`
	}
	if status != 200 || protocol.Decode(raw, &response, 4096) != nil || !response.Accepted || !protocol.ValidToken(response.NativeID) {
		return ReplyResult{Result: Result{Ambiguous: true}}
	}
	return ReplyResult{Result: Result{Accepted: true}, NativeID: response.NativeID, Sender: c.Profile.Account}
}
