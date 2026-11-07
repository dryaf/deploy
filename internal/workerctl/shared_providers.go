package workerctl

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"reflect"
)

func reconcileSharedProviders(ctx context.Context, r ControlRequest, p ResolvedPaths, helper string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
	return reconcileSharedPolicy(ctx, r, p, helper, l, "providers")
}
func defaultSharedProviders() notificationRemappingLifecycle {
	l := defaultSharedLimits()
	l.migrate = install.MigrateEnrolledProviders
	l.scope = backup.ReconcileVerificationProvidersScope
	return l
}

// Locate credential authority through validated coordinator ancestry. The scope
// selects the predecessor during an unfinished provider change. Every retained
// file remains required; a missing record cannot fall back to a newer credential.
func retainedSharedProviderInputs(d *safefs.Directory, j OperationJournal, scope string) (*notificationInputPins, []byte, error) {
	selected := false
	for depth := 0; depth < 128; depth++ {
		if validateOperationJournal(j) != nil {
			return nil, nil, fmt.Errorf("provider policy ancestry changed")
		}
		if j.Pins.DesiredHash == scope {
			selected = true
		}
		if selected && deliveryProviderJournal(j) {
			v, err := readDeliveryProviderIntent(d, j)
			return v.AfterInputs, v.AfterRuntime, err
		}
		if selected && sharedLimitsJournal(j) && sharedPolicyKind(j) == "providers" {
			var v notificationSharedLimitsIntent
			if err := d.ReadJSON("shared-limits-"+j.Children[0].Operation+".json", &v); err != nil {
				return nil, nil, err
			}
			if err := validateSharedLimitsIntent(v); err != nil {
				return nil, nil, err
			}
			if v.Operation != j.Children[0].Operation || v.DesiredHash != j.Pins.DesiredHash {
				return nil, nil, fmt.Errorf("provider input receipt changed")
			}
			return v.AfterInputs, v.AfterRuntime, nil
		}
		if j.Pins.PreviousOperation == "" {
			return nil, nil, nil
		}
		var prior OperationJournal
		if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
			return nil, nil, err
		}
		if prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous || !prior.Complete {
			return nil, nil, fmt.Errorf("provider predecessor changed")
		}
		j = prior
	}
	return nil, nil, fmt.Errorf("provider policy ancestry exceeds review bound")
}
func compileSharedProviders(r ControlRequest, paths ResolvedPaths, d *safefs.Directory, prior OperationJournal, input install.RuntimeInput, raw []byte, reservation backup.ProducerReservation) (CompiledNotification, *notificationInputPins, *notificationInputPins, error) {
	var zero CompiledNotification
	old, _, err := retainedSharedProviderInputs(d, prior, prior.Pins.DesiredHash)
	if err != nil {
		return zero, nil, nil, err
	}
	if old == nil {
		old = &notificationInputPins{}
		if err = d.ReadJSON("notification-inputs.json", old); err != nil {
			return zero, nil, nil, err
		}
	}
	before := *old
	before.Runtime = inputDigest(raw)
	if err = verifyProviderRuntimeInputs(raw, before); err != nil {
		return zero, nil, nil, err
	}
	providerRequest := r
	providerRequest.Inputs = map[string][]byte{}
	for field, value := range r.Inputs {
		if (field == "signal_account" || field == "signal_recipient") && before.Version == 2 {
			continue
		}
		providerRequest.Inputs[field] = value
	}
	generated, next, err := compileProvidersRuntime(providerRequest, paths, input, before)
	if err != nil {
		return zero, nil, nil, err
	}
	var candidate install.RuntimeInput
	if err = safefs.DecodeJSON(generated, &candidate, maxControlFrame); err != nil {
		return zero, nil, nil, err
	}
	compiled, err := compileReservedNotification(retainedLEDDeclaration(r.Desired, candidate), r.Facts, paths, candidate, reservation)
	if err != nil {
		return zero, nil, nil, err
	}
	preserveRuntimeRouteLabels(input, &compiled.Input)
	after, err := compiled.Input.Bytes()
	if err != nil {
		return zero, nil, nil, err
	}
	next.Runtime = inputDigest(after)
	return compiled, &before, &next, nil
}
func sharedProviderScope(before config.DesiredWorkers) (string, error) {
	return before.NotificationFingerprint()
}
func validateSharedProviderPins(v notificationSharedLimitsIntent) error {
	if v.Version != 7 {
		if v.BeforeInputs != nil || v.AfterInputs != nil {
			return fmt.Errorf("unrelated policy contains provider input pins")
		}
		return nil
	}
	if v.BeforeInputs == nil || v.AfterInputs == nil {
		return fmt.Errorf("provider policy input pins missing")
	}
	if err := verifyProviderRuntimeInputs(v.BeforeRuntime, *v.BeforeInputs); err != nil {
		return err
	}
	if err := verifyProviderRuntimeInputs(v.AfterRuntime, *v.AfterInputs); err != nil {
		return err
	}
	b, a := *v.BeforeInputs, *v.AfterInputs
	a.Runtime, a.Token, a.Chat = b.Runtime, b.Token, b.Chat
	if protocol.Hash(a) != protocol.Hash(b) {
		return fmt.Errorf("provider update replaced unrelated credential authority")
	}
	return nil
}

func retainedProviderReviewRequest(r ControlRequest, before config.DesiredWorkers, input install.RuntimeInput) (ControlRequest, error) {
	r.Desired = before
	var err error
	r.DesiredHash, err = before.NotificationFingerprint()
	if err != nil {
		return r, err
	}
	inputs := map[string][]byte{}
	for field, value := range r.Inputs {
		if field != "token" && field != "chat_id" {
			inputs[field] = value
		}
	}
	p := before.Notification.Providers["telegram"]
	for _, v := range []struct{ field, local, path string }{{"token", p.TokenInput, input.Providers["telegram"].TokenFile}, {"chat_id", p.ChatIDInput, input.Providers["telegram"].ChatFile}} {
		if v.local != "" {
			inputs[v.field], err = ReadCredentialInput(v.path)
			if err != nil {
				return r, err
			}
		}
	}
	r.Inputs = inputs
	return r, nil
}

func validateSharedProvidersDeclaration(before, after config.DesiredWorkers) error {
	oldSignal, hadSignal := before.Notification.Providers["signal"]
	newSignal, hasSignal := after.Notification.Providers["signal"]
	if hadSignal != hasSignal || !reflect.DeepEqual(oldSignal, newSignal) {
		return fmt.Errorf("retain the exact owned Signal profile during Telegram changes")
	}
	oldTelegram, hadTelegram := before.Notification.Providers["telegram"]
	next, hasTelegram := after.Notification.Providers["telegram"]
	for _, profiles := range []config.ProviderDeclarations{before.Notification.Providers, after.Notification.Providers} {
		for name := range profiles {
			if name != "telegram" && name != "signal" {
				return fmt.Errorf("unsupported direct provider profile")
			}
		}
	}
	if hadTelegram != hasTelegram && (!hasSignal || before.Notification.Commands.Enabled || after.Notification.Commands.Enabled || oldTelegram.Account != "" || oldTelegram.BotName != "" || oldTelegram.Conversation != "") {
		return fmt.Errorf("Telegram profile addition/removal requires an unchanged owned Signal profile and no commands")
	}
	if hasTelegram {
		if next.TokenFile == "" || next.ChatIDFile == "" || next.TokenInput != "" || next.ChatIDInput != "" {
			return fmt.Errorf("select private Telegram target-file references for shared updates")
		}
		next.TokenFile, next.ChatIDFile, next.TokenInput, next.ChatIDInput = oldTelegram.TokenFile, oldTelegram.ChatIDFile, oldTelegram.TokenInput, oldTelegram.ChatIDInput
		if !reflect.DeepEqual(oldTelegram, next) {
			return fmt.Errorf("change only direct Telegram profiles; retain account/command declarations")
		}
	}
	return nil
}
