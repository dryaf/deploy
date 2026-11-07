package workerctl

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/protocol"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

func reconcileNotificationProviders(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationLimitsLifecycle) (ControlResult, bool, error) {
	return reconcileNotificationPolicy(ctx, r, paths, helper, l, "providers")
}

// A rotation selects new target-private references. Existing files are retained
// and verified against their original pins throughout the transaction. In-place
// edits and operator-local reprovisioning cannot prove the previous credential.
func compileProvidersRuntime(r ControlRequest, paths ResolvedPaths, before install.RuntimeInput, pins notificationInputPins) ([]byte, notificationInputPins, error) {
	p, hasTelegram := r.Desired.Notification.Providers["telegram"]
	if (len(r.Desired.Notification.Providers) != 1 && len(r.Desired.Notification.Providers) != 2) || hasTelegram && (p.TokenFile == "" || p.ChatIDFile == "" || p.TokenInput != "" || p.ChatIDInput != "") || len(r.Inputs) != 0 {
		return nil, pins, fmt.Errorf("provider migration requires saved Telegram target-file references; retain existing files and select new files for changed values")
	}
	secrets := map[string][]byte{}
	if hasTelegram {
		if err := readDeclaredTelegramInputs(r, paths, secrets); err != nil {
			return nil, pins, err
		}
	}
	var input install.RuntimeInput
	var err error
	if signal, ok := r.Desired.Notification.Providers["signal"]; ok {
		resolved, e := ResolveSignalPaths(signal, paths, r.Facts.Home, r.SignalSHA256)
		if e != nil {
			return nil, pins, e
		}
		for field, path := range map[string]string{"signal_account": resolved.AccountFile, "signal_recipient": resolved.RecipientFile} {
			secrets[field], err = ReadCredentialInput(path)
			if err != nil {
				return nil, pins, err
			}
		}
		if pins.Version != 2 || inputDigest(secrets["signal_account"]) != pins.SignalAccount || inputDigest(secrets["signal_recipient"]) != pins.SignalRecipient {
			return nil, pins, fmt.Errorf("retained Signal private inputs changed")
		}
		input, err = compileDirectSignalNotification(r.Desired, paths, r.Facts.Home, secrets, resolved)
	} else {
		if r.LocalSignal != nil || r.SignalSHA256 != "" {
			return nil, pins, fmt.Errorf("unrelated native artifact selected")
		}
		input, err = CompileUnboundNotification(r.Desired, paths, r.Facts.Home, secrets)
	}
	if err != nil {
		return nil, pins, err
	}
	input.LEDSocket, input.LEDEndpoint = before.LEDSocket, before.LEDEndpoint
	preserveRuntimeRouteLabels(before, &input)
	raw, err := input.Bytes()
	if err != nil {
		return nil, pins, err
	}
	pins.Runtime, pins.Token, pins.Chat = inputDigest(raw), inputDigest(secrets["token"]), inputDigest(secrets["chat_id"])
	return raw, pins, nil
}

func verifyProviderRuntimeInputs(raw []byte, pins notificationInputPins) error {
	if (pins.Version != 1 && pins.Version != 2) || !safefs.ValidID(pins.Operation, "n") || pins.Runtime != inputDigest(raw) {
		return fmt.Errorf("provider input authority changed")
	}
	var input install.RuntimeInput
	if err := safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return err
	}
	telegram, hasTelegram := input.Providers["telegram"]
	signal, hasSignal := input.Providers["signal"]
	if pins.Version == 1 && (!hasTelegram || hasSignal || pins.SignalAccount != "" || pins.SignalRecipient != "") || pins.Version == 2 && (!hasSignal || signal.Kind != "signal" || !validDigest(pins.SignalAccount) || !validDigest(pins.SignalRecipient)) {
		return fmt.Errorf("provider migration native authority changed")
	}
	expected := 0
	if hasSignal {
		expected++
	}
	if hasTelegram {
		expected++
	}
	if len(input.Providers) != expected || !hasTelegram && (pins.Token != inputDigest(nil) || pins.Chat != inputDigest(nil)) {
		return fmt.Errorf("provider migration contains unreviewed profile authority")
	}
	if hasTelegram {
		if telegram.Kind != "telegram" || !validDigest(pins.Token) || !validDigest(pins.Chat) {
			return fmt.Errorf("provider migration Telegram authority changed")
		}
		for _, v := range []struct{ path, hash string }{{telegram.TokenFile, pins.Token}, {telegram.ChatFile, pins.Chat}} {
			raw, err := ReadCredentialInput(v.path)
			if err != nil || inputDigest(raw) != v.hash {
				return fmt.Errorf("retained or selected Telegram credential changed; restore the reviewed private file before resuming")
			}
		}
	}
	_, err := input.Resolve()
	return err
}

func verifyProviderPredecessor(paths ResolvedPaths, d *safefs.Directory) error {
	var pins notificationInputPins
	if err := d.ReadJSON("notification-inputs.json", &pins); err != nil {
		return err
	}
	raw, err := readLimitsRuntime(paths.Runtime)
	if err != nil {
		return err
	}
	return verifyProviderRuntimeInputs(raw, pins)
}

func preflightProviderChange(ctx context.Context, r ControlRequest, paths ResolvedPaths, d *safefs.Directory, before config.DesiredWorkers, m install.ManagedManifest, l notificationLimitsLifecycle) error {
	if m.Commands != nil || m.SignalVerification != nil || m.SignalProfiles != nil || m.CommandHistoryHash != "" {
		return fmt.Errorf("provider changes require separate native account or command admission")
	}
	v := notificationLimitsIntent{Version: 3, Kind: "providers", DesiredHash: r.DesiredHash, Before: m}
	if err := d.ReadJSON("notification-inputs.json", &v.Inputs); err != nil {
		return err
	}
	if err := verifyProviderRetainedInputs(ctx, r, paths, d, before, m, v.Inputs, l); err != nil {
		return err
	}
	if v.Inputs.Version == 2 {
		v.Version = 4
	}
	v.Operation = v.Inputs.Operation // Validation only; no operation is allocated here.
	raw, err := readLimitsRuntime(paths.Runtime)
	if err != nil {
		return err
	}
	v.BeforeRuntime = raw
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return err
	}
	after, pins, err := compileProvidersRuntime(r, paths, input, v.Inputs)
	if err != nil {
		return err
	}
	v.AfterRuntime, v.NextInputs = after, &pins
	if err = validateLimitsIntent(v); err != nil {
		return err
	}
	return verifyProviderRuntimeInputs(after, pins)
}

// A Telegram update cannot select a different native account, recipient, artifact
// or child receipt. Checking here and on every retry preserves that authority
// even after the gateway domain has committed but input publication was lost.
func verifyProviderRetainedInputs(ctx context.Context, r ControlRequest, paths ResolvedPaths, d *safefs.Directory, before config.DesiredWorkers, m install.ManagedManifest, pins notificationInputPins, l notificationLimitsLifecycle) error {
	var operation OperationJournal
	if err := d.ReadJSON("operation.json", &operation); err != nil {
		return err
	}
	if r.LEDSHA256 != "" && operation.Pins.Artifacts["led-daemon"] != r.LEDSHA256 {
		return fmt.Errorf("selected LED artifact changed")
	}
	old, hasSignal := before.Notification.Providers["signal"]
	next, wantsSignal := r.Desired.Notification.Providers["signal"]
	if hasSignal != wantsSignal || !reflect.DeepEqual(old, next) {
		return fmt.Errorf("apply native Signal policy changes separately from Telegram references")
	}
	if !hasSignal {
		if m.FreshSignal != nil || pins.Version != 1 {
			return fmt.Errorf("unexpected native provider authority")
		}
		return nil
	}
	// Bootstrap retains the native artifact in the independently owned Signal
	// child. Later parents may also carry that pin; verify both when present.
	if pins.Version != 2 || m.FreshSignal == nil || operation.Pins.Artifacts["signal"] != "" && operation.Pins.Artifacts["signal"] != r.SignalSHA256 {
		return fmt.Errorf("native Signal authority missing or artifact changed")
	}
	secrets := map[string][]byte{}
	spec, receipt, err := readRetainedSignalInputs(r, paths, before, secrets)
	if err != nil {
		return err
	}
	if inputDigest(secrets["signal_account"]) != pins.SignalAccount || inputDigest(secrets["signal_recipient"]) != pins.SignalRecipient {
		return fmt.Errorf("native Signal credential pins changed")
	}
	inspect := l.native
	if inspect == nil {
		inspect = func(ctx context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
			return install.InspectFreshSignal(ctx, ref, false)
		}
	}
	native, err := inspect(ctx, *m.FreshSignal)
	if err != nil {
		return err
	}
	if protocol.Hash(native.Spec) != protocol.Hash(spec) || protocol.Hash(native) != receipt || native.Spec.Root != m.FreshSignal.Root || native.ManifestHash != m.FreshSignal.ManifestHash {
		return fmt.Errorf("retained native Signal service differs from gateway ownership")
	}
	return nil
}

// Explicit resume checks a pending gateway transaction before allowing its
// independently owned native child to resume. This neither commits the gateway
// nor grants a new link; the normal provider coordinator still runs afterward.
func verifyPendingProviderSignal(ctx context.Context, r ControlRequest, paths ResolvedPaths, d *safefs.Directory, j OperationJournal, native signalSetupLifecycle) error {
	var prior OperationJournal
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
		return err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || !unboundPredecessor(prior) || prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous || prior.Pins.Worker != j.Pins.Worker || prior.Pins.Action != "apply" || prior.Pins.UID != j.Pins.UID || prior.Pins.Home != j.Pins.Home || prior.Pins.DesiredScope != j.Pins.DesiredScope || !reflect.DeepEqual(prior.Pins.Installations, j.Pins.Installations) || !reflect.DeepEqual(prior.Pins.Artifacts, j.Pins.Artifacts) {
		return fmt.Errorf("pending provider predecessor changed before native resume")
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
	if err != nil {
		return err
	}
	l := notificationLimitsLifecycle{native: func(ctx context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
		secrets := map[string][]byte{}
		spec, receipt, err := readRetainedSignalInputs(r, paths, before, secrets)
		if err != nil {
			return install.FreshSignalManifest{}, err
		}
		m, complete, err := native.inspect(ctx, spec)
		var needed install.FreshSignalResumeRequired
		if err != nil && (!errors.As(err, &needed) || complete || needed.Reference != ref) {
			return m, err
		}
		if err == nil && !complete || protocol.Hash(m) != receipt || m.Spec.Root != ref.Root || m.ManifestHash != ref.ManifestHash {
			return m, fmt.Errorf("pending provider native receipt changed")
		}
		return m, nil
	}}
	step := notificationLimitsStep{kind: "providers", request: r, paths: paths, directory: d, before: before, expected: prior.Children[0].Receipt, lifecycle: l}
	v, err := step.intent(j.Children[0].Operation)
	if err != nil {
		return err
	}
	if v.Version != 4 {
		return fmt.Errorf("pending provider operation has no native account authority")
	}
	return step.verifyInputs(ctx, v)
}
