package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type notificationLEDLifecycle struct {
	inspect func(context.Context, string) (install.InstallationObservation, error)
	set     func(context.Context, string, string, bool) (install.ManagedManifest, error)
}

// A boolean output change is a separate successor transaction. Runtime bindings,
// daemon identity and fault history remain retained execution state. YAML still
// owns the desired permission; no new desired value is inferred from that state.
func reconcileDeclaredLEDOutput(ctx context.Context, r ControlRequest, paths ResolvedPaths, lifecycle notificationLEDLifecycle) (ControlResult, bool, error) {
	result := ControlResult{Version: 1}
	d, err := safefs.Open(paths.Control)
	if errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	defer d.Close()
	var old OperationJournal
	if err = d.ReadJSON("operation.json", &old); errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if err = validateOperationJournal(old); err != nil {
		return result, false, err
	}
	isOutput := len(old.Children) == 1 && old.Children[0].Name == "notification-led-output"
	if !isOutput && old.Pins.DesiredHash == r.DesiredHash {
		return result, false, nil
	}
	if old.Pins.Action != "apply" || old.Pins.UID != r.Facts.UID || old.Pins.Home != r.Facts.Home || old.Pins.Artifacts["helper"] != r.HelperSHA256 || old.Pins.Installations["notification-worker"] != paths.Installation || old.Pins.Installations["state"] != paths.State || old.Pins.Installations["runtime"] != paths.Runtime || len(old.Children) != 1 {
		return result, true, fmt.Errorf("changed declaration does not match retained worker authority")
	}
	starting := !(isOutput && old.Pins.DesiredHash == r.DesiredHash)
	predecessor := old
	pins := old.Pins
	if isOutput && old.Pins.DesiredHash == r.DesiredHash {
		predecessor = OperationJournal{}
		if err = d.ReadJSON("history/"+old.Pins.PreviousOperation+".json", &predecessor); err != nil {
			return result, true, err
		}
		if validateOperationJournal(predecessor) != nil || predecessor.Digest() != old.Pins.Previous || predecessor.ID != old.Pins.PreviousOperation {
			return result, true, fmt.Errorf("LED output predecessor changed")
		}
	} else {
		if !old.Complete {
			return result, true, fmt.Errorf("finish the retained operation before changing YAML")
		}
		pins.Previous, pins.PreviousOperation = old.Digest(), old.ID
		pins.DesiredHash = r.DesiredHash
	}
	if !predecessor.Complete || !unboundPredecessor(predecessor) {
		return result, true, fmt.Errorf("LED output change requires a completed verification installation")
	}
	before, err := readDeclaration(d, predecessor.Pins.DesiredHash, predecessor.Pins.DesiredScope)
	if err != nil {
		return result, true, err
	}
	if before.Notification == nil || r.Desired.Notification == nil {
		return result, true, fmt.Errorf("notification declaration missing")
	}
	next := before
	notification := *before.Notification
	next.Notification = &notification
	next.Notification.StatusLEDs.Enabled = r.Desired.Notification.StatusLEDs.Enabled
	hash, err := declarationFingerprint(next, r.DesiredScope)
	if err != nil || hash != r.DesiredHash || before.Notification.StatusLEDs.Enabled == r.Desired.Notification.StatusLEDs.Enabled {
		return result, true, fmt.Errorf("changed policy requires its domain migration; LED output reconciliation accepts only status_leds.enabled")
	}
	if starting {
		if err = requireUnreservedNotificationPolicy(r, paths, before, predecessor); err != nil {
			return result, true, err
		}
		observed, e := lifecycle.inspect(ctx, paths.Installation)
		if e != nil {
			return result, true, e
		}
		m := observed.Manifest
		if err = verifyActivityReceipt(d, predecessor, m); err != nil {
			return result, true, err
		}
		if err = verifyReleaseReceipt(d, predecessor, m); err != nil {
			return result, true, err
		}
		if !observed.Unbound || observed.Phase != "complete" || protocol.Hash(m) != predecessor.Children[0].Receipt || m.Mode != "verification" || m.DeliveryOwnerHash != "" || m.RecoveryCommitHash != "" {
			return result, true, fmt.Errorf("LED toggle requires an unchanged unbound verification gateway; shared consumers need coordinated policy migration")
		}
	}
	// Validate declared credentials as well as retained files. A hidden same-bot
	// token rotation cannot hitch a ride on an unrelated LED switch.
	if err = verifyLEDOutputInputs(r, paths, d, before); err != nil {
		return result, true, err
	}
	step := &notificationLEDStep{directory: d, paths: paths, expected: predecessor.Children[0].Receipt, disabled: !r.Desired.Notification.StatusLEDs.Enabled, desired: r.Desired, desiredHash: r.DesiredHash, desiredScope: r.DesiredScope, lifecycle: lifecycle}
	if _, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step}); err != nil {
		var pending OperationJournal
		if e := d.ReadJSON("operation.json", &pending); e == nil && validateOperationJournal(pending) == nil && !pending.Complete && pending.Pins.DesiredHash == r.DesiredHash && len(pending.Children) == 1 && pending.Children[0].Name == step.Name() {
			return result, true, ledOutputPendingError{Server: r.Server, Cause: err}
		}
		return result, true, err
	}
	result.State, result.Code = "verification", "ok"
	result.Identity, result.LEDOutput = &step.manifest.Identity, step.manifest.LEDOutput
	if step.disabled {
		result.Next = "LED output disabled; unresolved incidents and daemon configuration retained"
	} else {
		result.Next = "LED output re-enabled with retained identities; fresh observations and qualification remain required"
	}
	return result, true, nil
}

type notificationLEDIntent struct {
	Version     int                     `json:"version"`
	Operation   string                  `json:"operation"`
	Before      install.ManagedManifest `json:"before"`
	Disabled    bool                    `json:"disabled"`
	DesiredHash string                  `json:"desired_hash"`
}
type notificationLEDStep struct {
	directory             *safefs.Directory
	paths                 ResolvedPaths
	expected, desiredHash string
	desiredScope          int
	desired               config.DesiredWorkers
	disabled              bool
	production            bool
	enrolled              bool
	lifecycle             notificationLEDLifecycle
	manifest              install.ManagedManifest
}

func (s *notificationLEDStep) Name() string { return "notification-led-output" }
func (s *notificationLEDStep) intent(op string) (notificationLEDIntent, error) {
	var v notificationLEDIntent
	err := s.directory.ReadJSON("led-output-"+op+".json", &v)
	if err == nil && (v.Version != 1 || v.Operation != op || protocol.Hash(v.Before) != s.expected || v.Disabled != s.disabled || v.DesiredHash != s.desiredHash) {
		err = fmt.Errorf("LED output intent changed")
	}
	return v, err
}
func (s *notificationLEDStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	observed, err := s.lifecycle.inspect(ctx, s.paths.Installation)
	if err != nil {
		return "", false, err
	}
	v, intentErr := s.intent(op)
	if intentErr != nil && !errors.Is(intentErr, os.ErrNotExist) {
		return "", false, intentErr
	}
	if protocol.Hash(observed.Manifest) == s.expected && observed.Phase == "complete" {
		return "", false, nil
	}
	if intentErr != nil {
		return "", false, fmt.Errorf("LED output changed without coordinator intent")
	}
	if observed.Phase != "complete" {
		return "", false, nil
	}
	m := observed.Manifest
	if m.LEDOutput == nil || m.LEDOutput.Disabled != s.disabled || !safefs.ValidID(m.LEDOutput.Operation, "n") {
		return "", false, fmt.Errorf("LED output completion differs")
	}
	want := v.Before
	want.LEDOutput = m.LEDOutput
	if protocol.Hash(want) != protocol.Hash(m) {
		return "", false, fmt.Errorf("LED toggle changed gateway identity, configuration or service")
	}
	if _, err = readDeclaration(s.directory, s.desiredHash, s.desiredScope); err != nil {
		return "", false, err
	}
	s.manifest = m
	return protocol.Hash(m), true, nil
}
func (s *notificationLEDStep) Run(ctx context.Context, op string) error {
	_, err := s.intent(op)
	if errors.Is(err, os.ErrNotExist) {
		observed, e := s.lifecycle.inspect(ctx, s.paths.Installation)
		if e != nil {
			return e
		}
		m := observed.Manifest
		validMode := m.Mode == "verification" && m.DeliveryOwnerHash == ""
		if s.production {
			validMode = m.Mode == "delivery" && m.DeliveryActivation != nil
		}
		if observed.Unbound == s.enrolled || observed.Phase != "complete" || protocol.Hash(m) != s.expected || !validMode || m.RecoveryCommitHash != "" {
			return fmt.Errorf("LED toggle requires exact unrecovered installation authority")
		}
		err = s.directory.WriteJSON("led-output-"+op+".json", notificationLEDIntent{1, op, m, s.disabled, s.desiredHash}, false)
	}
	if err != nil {
		return err
	}
	if err = retainDeclaration(s.directory, s.desired, s.desiredHash, s.desiredScope); err != nil {
		return err
	}
	_, err = s.lifecycle.set(ctx, s.paths.Installation, s.expected, s.disabled)
	return err
}

func verifyLEDOutputInputs(r ControlRequest, paths ResolvedPaths, d *safefs.Directory, before config.DesiredWorkers, transitioningRuntime ...string) error {
	var operation OperationJournal
	if err := d.ReadJSON("operation.json", &operation); err != nil {
		return err
	}
	if r.LEDSHA256 != "" && operation.Pins.Artifacts["led-daemon"] != r.LEDSHA256 {
		return fmt.Errorf("selected LED artifact changed; explicit daemon replacement required")
	}
	var pins notificationInputPins
	if err := d.ReadJSON("notification-inputs.json", &pins); err != nil {
		return err
	}
	if (pins.Version != 1 && pins.Version != 2) || !safefs.ValidID(pins.Operation, "n") {
		return fmt.Errorf("private input authority changed")
	}
	info, err := os.Lstat(paths.Runtime)
	if err != nil || info.Size() <= 0 || info.Size() > maxControlFrame {
		return fmt.Errorf("retained runtime unavailable")
	}
	raw, err := readPrivateGeneratedInput(paths.Runtime, int(info.Size()))
	if err != nil || inputDigest(raw) != pins.Runtime && !slices.Contains(transitioningRuntime, inputDigest(raw)) {
		return fmt.Errorf("retained runtime changed")
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return err
	}
	if _, err = input.Resolve(); err != nil {
		return err
	}
	// The original bootstrap remains immutable. Verify its files independently
	// before selecting the current shared provider receipt for declared inputs.
	for _, check := range []struct{ path, hash string }{{input.Providers["telegram"].TokenFile, pins.Token}, {input.Providers["telegram"].ChatFile, pins.Chat}} {
		if check.path == "" {
			continue
		}
		value, e := ReadCredentialInput(check.path)
		if e != nil || inputDigest(value) != check.hash {
			return fmt.Errorf("retained Telegram input changed")
		}
	}
	effectivePins := pins
	scope, e := before.NotificationFingerprint()
	if e != nil {
		return e
	}
	selected, selectedRaw, e := retainedSharedProviderInputs(d, operation, scope)
	if e != nil {
		return e
	}
	if selected != nil {
		effectivePins = *selected
		input = install.RuntimeInput{} // Decoding into populated maps retains removed profiles.
		if e = safefs.DecodeJSON(selectedRaw, &input, maxControlFrame); e != nil {
			return e
		}
	}
	secrets := map[string][]byte{}
	if err = readDeclaredTelegramInputs(r, paths, secrets); err != nil {
		return err
	}
	if _, ok := before.Notification.Providers["signal"]; ok {
		if _, _, err = readRetainedSignalInputs(r, paths, before, secrets); err != nil {
			return err
		}
	}
	if inputDigest(secrets["token"]) != effectivePins.Token || inputDigest(secrets["chat_id"]) != effectivePins.Chat || effectivePins.Version == 2 && (inputDigest(secrets["signal_account"]) != effectivePins.SignalAccount || inputDigest(secrets["signal_recipient"]) != effectivePins.SignalRecipient) {
		return fmt.Errorf("credential differs from retained provider policy")
	}
	if p, ok := input.Providers["telegram"]; ok {
		for field, path := range map[string]string{"token": p.TokenFile, "chat_id": p.ChatFile} {
			value, e := ReadCredentialInput(path)
			if e != nil || inputDigest(value) != inputDigest(secrets[field]) {
				return fmt.Errorf("installed Telegram input changed")
			}
		}
	}

	for field := range r.Inputs {
		if _, ok := secrets[field]; !ok {
			return fmt.Errorf("unexpected credential input")
		}
	}
	return nil
}

// readRetainedSignalInputs validates the completed native child without starting,
// linking or replacing its service. The caller compares its receipt and private
// input pins with the gateway transaction.
func readRetainedSignalInputs(r ControlRequest, paths ResolvedPaths, before config.DesiredWorkers, secrets map[string][]byte) (install.FreshSignalSpec, string, error) {
	resolved, e := ResolveSignalPaths(before.Notification.Providers["signal"], paths, r.Facts.Home, r.SignalSHA256)
	if e != nil {
		return install.FreshSignalSpec{}, "", e
	}
	child, e := safefs.Open(resolved.Control)
	if e != nil {
		return install.FreshSignalSpec{}, "", fmt.Errorf("retained Signal setup authority unavailable: %w", e)
	}
	defer child.Close()
	lock, e := child.Lock("operation.lock", false, false)
	if e != nil {
		return install.FreshSignalSpec{}, "", e
	}
	defer lock.Close()
	var journal OperationJournal
	if e = child.ReadJSON("operation.json", &journal); e != nil {
		return install.FreshSignalSpec{}, "", e
	}
	if validateOperationJournal(journal) != nil || !journal.Complete || len(journal.Children) != 1 || journal.Children[0].Name != "signal-service" || journal.Pins.Action != "apply" || journal.Pins.Worker != r.Worker || journal.Pins.UID != r.Facts.UID || journal.Pins.Home != r.Facts.Home || journal.Pins.Artifacts["signal"] != r.SignalSHA256 || journal.Pins.Installations["signal-service"] != resolved.Service || journal.Pins.Installations["signal-account"] != resolved.Account || journal.Pins.Installations["signal-inbox"] != resolved.Inbox {
		return install.FreshSignalSpec{}, "", fmt.Errorf("selected Signal artifact or ownership changed; explicit service migration required")
	}
	var selected signalSetupPins
	if e = child.ReadJSON("inputs.json", &selected); e != nil {
		return install.FreshSignalSpec{}, "", e
	}
	if selected.Version != 1 || selected.Operation != journal.Children[0].Operation || selected.Paths != resolved || selected.DesiredHash != journal.Pins.DesiredHash || selected.HelperHash != journal.Pins.Artifacts["helper"] || selected.CLIHash != r.SignalSHA256 {
		return install.FreshSignalSpec{}, "", fmt.Errorf("Signal input authority differs from selected native service")
	}
	for field, path := range map[string]string{"signal_account": resolved.AccountFile, "signal_recipient": resolved.RecipientFile} {
		value, e := ReadCredentialInput(path)
		if e != nil {
			return install.FreshSignalSpec{}, "", e
		}
		if supplied, ok := r.Inputs[field]; ok && inputDigest(supplied) != inputDigest(value) {
			return install.FreshSignalSpec{}, "", fmt.Errorf("Signal input changed during policy update")
		}
		secrets[field] = value
	}
	if selected.AccountHash != inputDigest(secrets["signal_account"]) || selected.RecipientHash != inputDigest(secrets["signal_recipient"]) {
		return install.FreshSignalSpec{}, "", fmt.Errorf("Signal input authority changed")
	}
	original := r
	original.DesiredHash, original.DesiredScope = journal.Pins.DesiredHash, journal.Pins.DesiredScope
	original.HelperSHA256 = journal.Pins.Artifacts["helper"]
	step := signalSetupStep{request: original, gateway: paths, paths: resolved, directory: child, provider: before.Notification.Providers["signal"], account: secrets["signal_account"], recipient: secrets["signal_recipient"]}
	record, e := step.record(journal.Children[0].Operation)
	if e != nil {
		return install.FreshSignalSpec{}, "", e
	}
	return record.Spec, journal.Children[0].Receipt, nil
}

// The remote response preserves pending output state rather than turning every
// interrupted domain transition into an undifferentiated failure.
type ledOutputPendingError struct {
	Server string
	Action string
	Cause  error
}

func (e ledOutputPendingError) Error() string { return e.Cause.Error() }
func (e ledOutputPendingError) Unwrap() error { return e.Cause }
