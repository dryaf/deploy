package provider

import (
	"fmt"
	"strings"
)

// CheckTelegramAccount checks only local credential syntax and bot identity.
// It performs no provider request and cannot prove webhook/receiver ownership.
func CheckTelegramAccount(p TelegramAccount) error {
	token, err := ReadSecret(p.TokenFile)
	account, _, _ := strings.Cut(token, ":")
	if err != nil || !telegramToken.MatchString(token) || account != p.Account {
		return fmt.Errorf("Telegram credential unavailable or account changed")
	}
	return nil
}

// CheckTelegramProfile resolves the same local destination identity as Send,
// without sending a diagnostic or contacting Telegram.
func CheckTelegramProfile(p Profile) error {
	if p.Kind != "telegram" {
		return fmt.Errorf("not a Telegram profile")
	}
	if p.Conversation != nil {
		if p.Conversation.Provider != "telegram" || p.Destination != p.Conversation.Fingerprint() {
			return fmt.Errorf("Telegram conversation identity changed")
		}
		return CheckTelegramAccount(TelegramAccount{Account: p.Conversation.Account, TokenFile: p.TokenFile})
	}
	token, err := ReadSecret(p.TokenFile)
	if err != nil || !telegramToken.MatchString(token) {
		return fmt.Errorf("Telegram credential unavailable")
	}
	chat, err := ReadSecret(p.ChatFile)
	if err != nil || TelegramIdentity(token, chat) != p.Destination {
		return fmt.Errorf("Telegram destination changed")
	}
	return nil
}
