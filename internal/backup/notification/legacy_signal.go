package notification

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
)

func legacySignalCredentials(policy config.WorkerNotifications, profile *provider.Profile) (hash, telegramPin string, err error) {
	if profile == nil || policy.Signal == nil || profile.Kind != "signal" || profile.Conversation == nil || profile.Conversation.Provider != "signal" || profile.Conversation.Thread != "" || profile.Destination != profile.Conversation.Fingerprint() || !profile.RelayIdentity.Valid() {
		return "", "", fmt.Errorf("pinned legacy Signal conversation required")
	}
	account, err := ReadWorkerSecret(policy.Signal.AccountFile)
	if err != nil {
		return "", "", err
	}
	recipient, err := ReadWorkerSecret(policy.Signal.RecipientFile)
	if err != nil {
		return "", "", err
	}
	if policy.Primary != "signal" && policy.Primary != "telegram" || policy.Fallback != "" && policy.Fallback != "signal" && policy.Fallback != "telegram" || policy.Fallback == policy.Primary || policy.Primary != "signal" && policy.Fallback != "signal" {
		return "", "", fmt.Errorf("invalid legacy provider order")
	}
	if policy.Primary == "telegram" || policy.Fallback == "telegram" {
		if policy.Telegram == nil {
			return "", "", fmt.Errorf("legacy Telegram credentials missing")
		}
		token, err := ReadWorkerSecret(policy.Telegram.TokenFile)
		if err != nil {
			return "", "", err
		}
		chat, err := ReadWorkerSecret(policy.Telegram.ChatIDFile)
		if err != nil {
			return "", "", err
		}
		telegramPin = telegramCredentialHash(policy, token, chat)
	}
	return notify.Hash(struct {
		Policy                       config.WorkerNotifications
		Profile                      provider.Profile
		Account, Recipient, Telegram string
	}{policy, *profile, account, recipient, telegramPin}), telegramPin, nil
}

// LegacySignalSelectors fingerprints the existing selector values, which may be
// phone numbers while the enrolled conversation uses stable provider UUIDs.
func LegacySignalSelectors(policy config.WorkerNotifications) (source, account string, err error) {
	if policy.Signal == nil {
		return "", "", fmt.Errorf("Signal is not configured")
	}
	a, err := ReadWorkerSecret(policy.Signal.AccountFile)
	if err != nil {
		return "", "", err
	}
	r, err := ReadWorkerSecret(policy.Signal.RecipientFile)
	if err != nil {
		return "", "", err
	}
	return notify.Hash(struct {
		Policy             config.WorkerSignal
		Account, Recipient string
	}{*policy.Signal, a, r}), notify.Hash(a), nil
}

// LegacyCredentialHash preserves the old Telegram pin format. Adopted Signal
// delivery additionally pins its exact shared relay identity and conversation.
func LegacyCredentialHash(policy config.WorkerNotifications, profile *provider.Profile) (string, error) {
	if profile == nil {
		return TelegramCredentialHash(policy)
	}
	hash, _, err := legacySignalCredentials(policy, profile)
	return hash, err
}

func PinnedLegacySender(hash string, profile *provider.Profile) Sender {
	return pinnedLegacySender(hash, profile, nil)
}

func pinnedLegacySender(hash string, profile *provider.Profile, transport http.RoundTripper) Sender {
	if profile == nil {
		return PinnedTelegramSender(hash)
	}
	// Copy both levels so later caller mutation cannot redirect an admitted send.
	p := *profile
	if p.Conversation != nil {
		d := *p.Conversation
		p.Conversation = &d
	}
	return func(ctx context.Context, channel string, policy config.WorkerNotifications, event NotificationEvent) (DeliveryAttempt, error) {
		attempt := DeliveryAttempt{Provider: channel, At: time.Now().UTC()}
		pin, telegramPin, err := legacySignalCredentials(policy, &p)
		if err != nil {
			return attempt, err
		}
		if hash == "" || pin != hash || channel != policy.Primary && channel != policy.Fallback {
			return attempt, fmt.Errorf("legacy provider binding changed")
		}
		if channel == "telegram" {
			return sendWithTelegramPin(ctx, channel, policy, event, telegramPin)
		}
		if channel != "signal" {
			return attempt, fmt.Errorf("unsupported legacy provider")
		}
		a := provider.ConversationAdapter{SignalTransport: transport, Signal: &provider.SignalAccount{Account: p.Conversation.Account, Socket: p.Socket, InboxIdentity: p.RelayIdentity}}
		r := a.SendReply(ctx, *p.Conversation, boundedNotificationEvent(event).Message)
		attempt.Accepted, attempt.Ambiguous = r.Accepted, r.Ambiguous
		if !r.Accepted {
			return attempt, fmt.Errorf("shared Signal relay did not confirm legacy acceptance")
		}
		// Legacy history records acceptance only; it does not manufacture a
		// canonical notification receipt or conversational context mapping.
		return attempt, nil
	}
}
