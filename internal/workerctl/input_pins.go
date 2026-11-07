package workerctl

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/safefs"
)

// Credential digests belong only in the private installation control store.
// A target-file reference must not make same-account token rotation invisible.
type notificationInputPins struct {
	SignalAccount   string `json:"signal_account_sha256,omitempty"`
	SignalRecipient string `json:"signal_recipient_sha256,omitempty"`
	Version         int    `json:"version"`
	Operation       string `json:"operation"`
	Runtime         string `json:"runtime_sha256"`
	Token           string `json:"token_sha256"`
	Chat            string `json:"chat_sha256"`
}

func inputDigest(raw []byte) string { return fmt.Sprintf("%x", sha256.Sum256(raw)) }

func (s *notificationBootstrapStep) pinInputs(op string, create bool) error {
	raw, err := s.inputBytes()
	if err != nil {
		return err
	}
	want := notificationInputPins{Version: 1, Operation: op, Runtime: inputDigest(raw), Token: inputDigest(s.secrets["token"]), Chat: inputDigest(s.secrets["chat_id"])}
	if _, ok := s.input.Providers["signal"]; ok {
		if len(s.secrets["signal_account"]) == 0 || len(s.secrets["signal_recipient"]) == 0 {
			return fmt.Errorf("Signal input pins require private target values")
		}
		want.Version = 2
		want.SignalAccount = inputDigest(s.secrets["signal_account"])
		want.SignalRecipient = inputDigest(s.secrets["signal_recipient"])
	}
	d, err := safefs.Open(s.paths.Control)
	if err != nil {
		return err
	}
	defer d.Close()
	var actual notificationInputPins
	err = d.ReadJSON("notification-inputs.json", &actual)
	if errors.Is(err, os.ErrNotExist) && create {
		if !safefs.ValidID(op, "n") {
			return fmt.Errorf("credential publication requires its operation")
		}
		for _, path := range []string{s.paths.Runtime, s.paths.State, s.paths.Installation} {
			if _, e := os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
				return fmt.Errorf("credential input pins missing from existing installation; recovery required")
			}
		}
		return d.WriteJSON("notification-inputs.json", want, false)
	}
	if err != nil {
		return fmt.Errorf("private credential input pins unavailable; recovery required")
	}
	if op == "" {
		want.Operation = actual.Operation
	}
	if !safefs.ValidID(actual.Operation, "n") || actual != want {
		return fmt.Errorf("credential or runtime inputs changed; explicit rotation or migration required")
	}
	return nil
}
