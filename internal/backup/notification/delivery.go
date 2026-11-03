package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

type DeliveryAttempt struct {
	Provider  string    `json:"provider"`
	At        time.Time `json:"at"`
	Accepted  bool      `json:"accepted"`
	Ambiguous bool      `json:"ambiguous"`
}

type NotificationEvent struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	IncidentID string            `json:"incident_id,omitempty"`
	Message    string            `json:"message"`
	Members    []string          `json:"members,omitempty"`
	Attempts   []DeliveryAttempt `json:"provider_attempts"`
}

func ReadWorkerSecret(file string) (string, error) {
	dir, err := safefs.Open(filepath.Dir(file))
	if err != nil {
		return "", fmt.Errorf("provider credential unavailable")
	}
	defer dir.Close()
	f, err := dir.Open(filepath.Base(file), os.O_RDONLY, 0)
	if err != nil {
		return "", fmt.Errorf("provider credential unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("provider credential must be a private regular file")
	}
	body, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(body) > 4096 {
		return "", fmt.Errorf("provider credential exceeds bounds")
	}
	value := strings.TrimSpace(string(body))
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("provider credential is invalid")
	}
	return value, nil
}

func notificationHTTPClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("provider redirect refused") }, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, MaxResponseHeaderBytes: 16 << 10}}
}

func Send(ctx context.Context, provider string, policy config.WorkerNotifications, event NotificationEvent) (DeliveryAttempt, error) {
	event = boundedNotificationEvent(event)
	attempt := DeliveryAttempt{Provider: provider, At: time.Now().UTC()}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var endpoint string
	var payload any
	client := notificationHTTPClient()
	switch provider {
	case "telegram":
		if policy.Telegram == nil {
			return attempt, fmt.Errorf("telegram is not configured")
		}
		token, err := ReadWorkerSecret(policy.Telegram.TokenFile)
		if err != nil {
			return attempt, err
		}
		chat, err := ReadWorkerSecret(policy.Telegram.ChatIDFile)
		if err != nil {
			return attempt, err
		}
		for _, r := range token {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ':' || r == '_' || r == '-') {
				return attempt, fmt.Errorf("invalid telegram token format")
			}
		}
		endpoint = "https://api.telegram.org/bot" + token + "/sendMessage"
		payload = struct {
			ChatID string `json:"chat_id"`
			Text   string `json:"text"`
		}{chat, event.Message}
	case "signal":
		if policy.Signal == nil {
			return attempt, fmt.Errorf("signal is not configured")
		}
		endpoint = "http://relay/send"
		payload = struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}{event.ID, event.Message}
		client.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", "/relay/signal.sock")
		}, ResponseHeaderTimeout: 9 * time.Second, MaxResponseHeaderBytes: 16 << 10}
	default:
		return attempt, fmt.Errorf("unsupported notification provider")
	}
	defer client.CloseIdleConnections()
	body, err := json.Marshal(payload)
	if err != nil {
		return attempt, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return attempt, fmt.Errorf("invalid provider endpoint")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		attempt.Ambiguous = true
		return attempt, fmt.Errorf("provider delivery outcome is unknown")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<10+1))
	if err != nil || len(data) > 16<<10 {
		attempt.Ambiguous = true
		return attempt, fmt.Errorf("provider response exceeds bounds")
	}
	var accepted struct {
		OK       bool `json:"ok"`
		Accepted bool `json:"accepted"`
	}
	if response.StatusCode/100 != 2 || json.Unmarshal(data, &accepted) != nil || !(provider == "telegram" && accepted.OK || provider == "signal" && accepted.Accepted) {
		attempt.Ambiguous = response.StatusCode >= 500
		return attempt, fmt.Errorf("provider did not confirm acceptance")
	}
	attempt.Accepted = true
	return attempt, nil
}

func Heartbeat(ctx context.Context, secretFile string, failed bool) error {
	secret, err := ReadWorkerSecret(secretFile)
	if err != nil {
		return err
	}
	endpoint, err := url.Parse(secret)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return fmt.Errorf("heartbeat requires a valid HTTPS URL")
	}
	if failed {
		endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/fail"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("invalid heartbeat request")
	}
	client := notificationHTTPClient()
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("heartbeat delivery failed")
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("heartbeat not accepted")
	}
	_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return err
}

type Sender func(context.Context, string, config.WorkerNotifications, NotificationEvent) (DeliveryAttempt, error)
