package workerctl

import (
	"fmt"
	"github.com/dryaf/deploy/internal/config"
)

// Only explicitly operator-local references are read here. Target-private
// references are resolved by the remote helper, including dependency setup.
func prepareOperatorNotificationInputs(r *ControlRequest, artifacts ResolvedArtifacts, arch string) error {
	if !config.WorkerEnabled(r.Desired.Notification.Enabled) {
		return nil
	}
	if d := r.Desired.Notification.StatusLEDs; d.Enabled && d.Daemon.Mode == "managed" {
		a, err := ResolveLEDArtifact(d.Daemon, artifacts.LEDs)
		if err != nil {
			return err
		}
		r.LocalLED = a
		r.LEDSHA256 = a.SHA256
	}
	if r.Inputs == nil {
		r.Inputs = map[string][]byte{}
	}
	telegram := r.Desired.Notification.Providers["telegram"]
	files := map[string]string{"token": telegram.TokenInput, "chat_id": telegram.ChatIDInput}
	if signal, ok := r.Desired.Notification.Providers["signal"]; ok {
		files["signal_account"], files["signal_recipient"] = signal.AccountInput, signal.RecipientInput
		if signal.Executable != "" {
			r.SignalSHA256 = signal.ExecutableSHA256
		} else {
			if artifacts.Signal == nil {
				return fmt.Errorf("missing_input: worker bundle requires a supported native Signal CLI")
			}
			if err := VerifySignalArtifact(*artifacts.Signal, arch); err != nil {
				return err
			}
			r.LocalSignal = artifacts.Signal
			r.SignalSHA256 = artifacts.Signal.SHA256
		}
	}
	for field, path := range files {
		if path == "" {
			continue
		}
		raw, err := ReadCredentialInput(path)
		if err != nil {
			return err
		}
		r.Inputs[field] = raw
	}
	return nil
}

// Read target references or explicitly supplied operator-local inputs without
// publishing anything. The compiler performs bounded content validation.
func readDeclaredTelegramInputs(r ControlRequest, paths ResolvedPaths, secrets map[string][]byte) error {
	p, ok := r.Desired.Notification.Providers["telegram"]
	if !ok {
		return nil
	}
	token, chat, err := telegramTargetPaths(p, paths, r.Facts.Home)
	if err != nil {
		return err
	}
	for _, input := range []struct{ field, local, target string }{{"token", p.TokenInput, token}, {"chat_id", p.ChatIDInput, chat}} {
		raw, supplied := r.Inputs[input.field]
		if input.local != "" {
			if !supplied {
				return fmt.Errorf("missing private credential input")
			}
		} else {
			if supplied {
				return fmt.Errorf("unexpected private credential input")
			}
			raw, err = ReadCredentialInput(input.target)
			if err != nil {
				return err
			}
		}
		secrets[input.field] = raw
	}
	return nil
}
