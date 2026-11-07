package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

type notificationLimitsLifecycle struct {
	native   func(context.Context, install.FreshSignalReference) (install.FreshSignalManifest, error)
	inspect  func(context.Context, string) (install.InstallationObservation, error)
	migrate  func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error)
	boundary func(string) error
}

func readLimitsRuntime(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 || info.Size() > maxControlFrame {
		return nil, fmt.Errorf("retained bootstrap policy runtime size changed")
	}
	return readPrivateGeneratedInput(path, int(info.Size()))
}

// Only a retained lifecycle intent may replace a generated input. Missing or
// foreign contents refuse; ordinary credential publication remains no-clobber.
func replaceDeclaredRuntime(path string, before, after []byte) error {
	current, err := readLimitsRuntime(path)
	if err != nil {
		return err
	}
	if string(current) == string(after) {
		return nil
	}
	if string(current) != string(before) {
		return fmt.Errorf("runtime changed before reviewed replacement")
	}
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return safefs.ReplaceFile(d, filepath.Base(path), before, after, 0600)
}

func limitsJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && (j.Children[0].Name == "notification-command-draft" || j.Children[0].Name == "notification-limits" || j.Children[0].Name == "notification-routes" || j.Children[0].Name == "notification-providers")
}

func reconcileNotificationLimits(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationLimitsLifecycle) (ControlResult, bool, error) {
	return reconcileNotificationPolicy(ctx, r, paths, helper, l, "")
}
func reconcileNotificationRoutes(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationLimitsLifecycle) (ControlResult, bool, error) {
	return reconcileNotificationPolicy(ctx, r, paths, helper, l, "routes")
}
func reconcileNotificationPolicy(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationLimitsLifecycle, kind string) (ControlResult, bool, error) {
	result := ControlResult{Version: 1}
	if !config.WorkerEnabled(r.Desired.Notification.Enabled) {
		return result, false, nil
	}
	d, err := safefs.Open(paths.Control)
	if errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	}
	if err != nil {
		return result, true, err
	}
	defer d.Close()
	var old OperationJournal
	if err = d.ReadJSON("operation.json", &old); errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	} else if err != nil {
		return result, true, err
	}
	if err = validateOperationJournal(old); err != nil {
		return result, true, err
	}
	if kind == "command-draft" && !unboundPredecessor(old) {
		return result, false, nil
	}

	retry := limitsJournal(old) && policyJournalKind(old) == kind && old.Pins.DesiredHash == r.DesiredHash
	if !retry && old.Pins.DesiredHash == r.DesiredHash {
		return result, false, nil
	}
	prior := old
	if retry {
		prior = OperationJournal{}
		if err = d.ReadJSON("history/"+old.Pins.PreviousOperation+".json", &prior); err != nil {
			return result, true, err
		}
		if validateOperationJournal(prior) != nil || prior.ID != old.Pins.PreviousOperation || prior.Digest() != old.Pins.Previous {
			return result, true, fmt.Errorf("bootstrap policy predecessor changed")
		}
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
	if err != nil {
		return result, true, err
	}
	if before.Notification == nil {
		return result, true, fmt.Errorf("bootstrap policy predecessor declaration missing")
	}
	same := before.Notification.Limits == r.Desired.Notification.Limits
	if kind == "command-draft" {
		same = reflect.DeepEqual(before.Notification.Commands, r.Desired.Notification.Commands) && reflect.DeepEqual(before.Notification.Providers, r.Desired.Notification.Providers)
	} else if kind == "routes" {
		same = reflect.DeepEqual(before.Notification.Routes, r.Desired.Notification.Routes)
	} else if kind == "providers" {
		same = reflect.DeepEqual(before.Notification.Providers, r.Desired.Notification.Providers)
	}
	if !retry && same {
		return result, false, nil
	}
	if !prior.Complete || !unboundPredecessor(prior) || old.Pins.Worker != r.Worker || old.Pins.UID != r.Facts.UID || old.Pins.Home != r.Facts.Home || old.Pins.Artifacts["helper"] != r.HelperSHA256 || old.Pins.DesiredScope != r.DesiredScope || old.Pins.Installations["notification-worker"] != paths.Installation || old.Pins.Installations["state"] != paths.State || old.Pins.Installations["runtime"] != paths.Runtime {
		return result, true, fmt.Errorf("bootstrap policy migration requires unchanged unbound installation authority")
	}
	if kind == "command-draft" && validateCommandDraftDeclaration(before, r.Desired) != nil {
		if !retry {
			return result, false, nil
		}
		return result, true, fmt.Errorf("resume the exact retained command draft")
	}
	next := before
	n := *before.Notification
	next.Notification = &n
	if kind == "command-draft" {
		next.Notification.Commands = r.Desired.Notification.Commands
		next.Notification.Providers = r.Desired.Notification.Providers
	} else if kind == "routes" {
		next.Notification.Routes = r.Desired.Notification.Routes
	} else if kind == "providers" {
		next.Notification.Providers = r.Desired.Notification.Providers
	} else {
		next.Notification.Limits = r.Desired.Notification.Limits
	}
	hash, err := declarationFingerprint(next, r.DesiredScope)
	if err != nil || hash != r.DesiredHash {
		return result, true, fmt.Errorf("apply notification bootstrap policy separately from other policy changes")
	}
	pins := old.Pins
	if !retry {
		if !old.Complete {
			return result, true, fmt.Errorf("finish pending operation before changing bootstrap policy")
		}
		if err = requireUnreservedNotificationPolicy(r, paths, before, prior); err != nil {
			return result, true, err
		}
		o, e := l.inspect(ctx, paths.Installation)
		if e != nil {
			return result, true, e
		}
		if !o.Unbound || o.Phase != "complete" || o.Manifest.Mode != "verification" || o.Manifest.DeliveryOwnerHash != "" || o.Manifest.RecoveryCommitHash != "" || protocol.Hash(o.Manifest) != prior.Children[0].Receipt {
			return result, true, fmt.Errorf("bootstrap policy require exact unbound verification authority")
		}
		if err = verifyActivityReceipt(d, prior, o.Manifest); err != nil {
			return result, true, err
		}
		if err = verifyReleaseReceipt(d, prior, o.Manifest); err != nil {
			return result, true, err
		}
		if kind == "providers" {
			err = verifyProviderPredecessor(paths, d)
		} else {
			err = verifyLEDOutputInputs(r, paths, d, before)
		}
		if err != nil {
			return result, true, err
		}
		if kind == "providers" {
			if err = preflightProviderChange(ctx, r, paths, d, before, o.Manifest, l); err != nil {
				return result, true, err
			}
		}
		if kind == "command-draft" {
			if err = preflightCommandDraft(r, paths, o.Manifest); err != nil {
				return result, true, err
			}
		}
		pins.Previous, pins.PreviousOperation = old.Digest(), old.ID
		pins.DesiredHash = r.DesiredHash
	}
	s := &notificationLimitsStep{kind: kind, directory: d, request: r, paths: paths, helper: helper, expected: prior.Children[0].Receipt, before: before, lifecycle: l}
	if _, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{s}); err != nil {
		return result, true, err
	}
	result.State, result.Code = "waiting_for_producers", "ok"
	result.Identity = &s.manifest.Identity
	result.LEDOutput = s.manifest.LEDOutput
	result.Next = "notification limits reconciled; gateway and provider identities retained; qualification remains separate"
	if kind == "command-draft" {
		result.Next = "command declaration saved without restarting the gateway; install the local backup to reserve its identity, then qualify before command admission"
	}
	if kind == "routes" {
		result.Next = "notification routes reconciled before producer enrollment; gateway and credentials retained; qualification remains separate"
	}
	if kind == "providers" {
		result.Next = "Telegram references reconciled before producer enrollment; gateway identity and prior credential files retained; run explicit test and qualification for the selected destination"
	}
	return result, true, nil
}

type notificationLimitsIntent struct {
	NextInputs    *notificationInputPins  `json:"next_inputs,omitempty"`
	Kind          string                  `json:"kind,omitempty"`
	Version       int                     `json:"version"`
	Operation     string                  `json:"operation"`
	DesiredHash   string                  `json:"desired_hash"`
	Before        install.ManagedManifest `json:"before"`
	Inputs        notificationInputPins   `json:"inputs"`
	BeforeRuntime []byte                  `json:"before_runtime"`
	AfterRuntime  []byte                  `json:"after_runtime"`
}

func (v notificationLimitsIntent) afterInputs() notificationInputPins {
	if v.Kind == "providers" && v.NextInputs != nil {
		return *v.NextInputs
	}
	pins := v.Inputs
	pins.Runtime = inputDigest(v.AfterRuntime)
	return pins
}
func validateLimitsIntent(v notificationLimitsIntent) error {
	if (v.Version != 1 || v.Kind != "") && (v.Version != 2 || v.Kind != "routes") && ((v.Version != 3 && v.Version != 4) || v.Kind != "providers") && (v.Version != 5 || v.Kind != "command-draft") || !safefs.ValidID(v.Operation, "n") || !validDigest(v.DesiredHash) || !safefs.ValidID(v.Inputs.Operation, "n") || (v.Inputs.Version != 1 && v.Inputs.Version != 2) || v.Inputs.Runtime != inputDigest(v.BeforeRuntime) || v.Before.RuntimeHash != inputDigest(v.BeforeRuntime) {
		return fmt.Errorf("bootstrap policy input authority changed")
	}
	var before, after install.RuntimeInput
	if err := safefs.DecodeJSON(v.BeforeRuntime, &before, maxControlFrame); err != nil {
		return err
	}
	if err := safefs.DecodeJSON(v.AfterRuntime, &after, maxControlFrame); err != nil {
		return err
	}
	b, err := before.Resolve()
	if err != nil {
		return err
	}
	a, err := after.Resolve()
	if err != nil {
		return err
	}
	if !b.Bootstrap || !a.Bootstrap || protocol.Hash(b) != v.Before.ConfigHash {
		return fmt.Errorf("bootstrap policy source runtime changed")
	}
	if v.Kind == "providers" {
		if v.NextInputs == nil || v.Inputs.Version != v.NextInputs.Version || v.NextInputs.Operation != v.Inputs.Operation || v.NextInputs.Runtime != inputDigest(v.AfterRuntime) {
			return fmt.Errorf("provider migration credential pins changed")
		}
		if v.Version == 3 && (v.Inputs.Version != 1 || v.Before.FreshSignal != nil || v.Inputs.SignalAccount != "" || v.Inputs.SignalRecipient != "" || v.NextInputs.SignalAccount != "" || v.NextInputs.SignalRecipient != "") || v.Version == 4 && (v.Inputs.Version != 2 || v.Before.FreshSignal == nil || !validDigest(v.Inputs.SignalAccount) || !validDigest(v.Inputs.SignalRecipient) || v.NextInputs.SignalAccount != v.Inputs.SignalAccount || v.NextInputs.SignalRecipient != v.Inputs.SignalRecipient) {
			return fmt.Errorf("provider migration native authority changed")
		}
		if err := verifyProviderRuntimeInputs(v.BeforeRuntime, v.Inputs); err != nil {
			return err
		}
		if err := verifyProviderRuntimeInputs(v.AfterRuntime, *v.NextInputs); err != nil {
			return err
		}
		return install.ValidateBootstrapProvidersChange(b, a)
	}
	if v.NextInputs != nil {
		return fmt.Errorf("unrelated policy carries credential rotation")
	}
	if v.Kind == "command-draft" {
		if string(v.BeforeRuntime) != string(v.AfterRuntime) || b.Worker.Interaction != nil || b.Telegram != nil || b.Signal != nil || v.Before.Commands != nil || v.Before.CommandHistoryHash != "" {
			return fmt.Errorf("command draft must retain the exact unbound runtime")
		}
		return nil
	}
	validate := worker.ValidateBootstrapLimitsChange
	if v.Kind == "routes" {
		validate = worker.ValidateBootstrapRoutesChange
	}
	if err = validate(b.Worker, a.Worker); err != nil {
		return err
	}
	b.Worker = a.Worker
	if protocol.Hash(b) != protocol.Hash(a) {
		return fmt.Errorf("bootstrap policy intent changed other runtime policy")
	}
	return nil
}
func validateLimitsManifest(v notificationLimitsIntent, m install.ManagedManifest) error {
	if v.Kind == "command-draft" {
		if protocol.Hash(v.Before) != protocol.Hash(m) {
			return fmt.Errorf("command draft changed the installed gateway")
		}
		return nil
	}
	var input install.RuntimeInput
	if err := safefs.DecodeJSON(v.AfterRuntime, &input, maxControlFrame); err != nil {
		return err
	}
	c, err := input.Resolve()
	if err != nil {
		return err
	}
	want := v.Before
	want.ConfigHash, want.RuntimeHash = protocol.Hash(c), inputDigest(v.AfterRuntime)
	want.Revision, want.UnitHash = m.Revision, m.UnitHash
	want.Paths.Config, want.Paths.Executable = m.Paths.Config, m.Paths.Executable
	want.HelperHash, want.HelperRevision = want.BinaryHash, m.Revision
	if m.Mode != "verification" || m.Revision == v.Before.Revision || protocol.Hash(want) != protocol.Hash(m) {
		return fmt.Errorf("bootstrap policy migration changed retained identity or authority")
	}
	return nil
}

type notificationLimitsStep struct {
	kind             string
	directory        *safefs.Directory
	request          ControlRequest
	paths            ResolvedPaths
	helper, expected string
	before           config.DesiredWorkers
	lifecycle        notificationLimitsLifecycle
	manifest         install.ManagedManifest
}

func (s *notificationLimitsStep) Name() string {
	if s.kind == "command-draft" {
		return "notification-command-draft"
	}
	if s.kind == "providers" {
		return "notification-providers"
	}
	if s.kind == "routes" {
		return "notification-routes"
	}
	return "notification-limits"
}
func policyJournalKind(j OperationJournal) string {
	if len(j.Children) == 1 && j.Children[0].Name == "notification-command-draft" {
		return "command-draft"
	}
	if len(j.Children) == 1 && j.Children[0].Name == "notification-providers" {
		return "providers"
	}
	if len(j.Children) == 1 && j.Children[0].Name == "notification-routes" {
		return "routes"
	}
	return ""
}
func (s *notificationLimitsStep) boundary(at string) error {
	if s.lifecycle.boundary != nil {
		return s.lifecycle.boundary(at)
	}
	return nil
}
func (s *notificationLimitsStep) intent(op string) (notificationLimitsIntent, error) {
	var v notificationLimitsIntent
	if err := s.directory.ReadJSON("limits-"+op+".json", &v); err != nil {
		return v, err
	}
	if err := validateLimitsIntent(v); err != nil {
		return v, err
	}
	if v.Kind != s.kind || v.Operation != op || v.DesiredHash != s.request.DesiredHash || protocol.Hash(v.Before) != s.expected || v.Before.BinaryHash != s.request.HelperSHA256 {
		return v, fmt.Errorf("bootstrap policy operation changed")
	}
	var input install.RuntimeInput
	if err := safefs.DecodeJSON(v.BeforeRuntime, &input, maxControlFrame); err != nil {
		return v, err
	}
	after, err := compileLimitsRuntime(s.request, s.paths, input)
	if s.kind == "providers" {
		var pins notificationInputPins
		after, pins, err = compileProvidersRuntime(s.request, s.paths, input, v.Inputs)
		if err == nil && pins != v.afterInputs() {
			return v, fmt.Errorf("selected provider credentials changed during migration")
		}
	}
	if err != nil {
		return v, err
	}
	if inputDigest(after) != inputDigest(v.AfterRuntime) {
		return v, fmt.Errorf("bootstrap policy operation differs from desired policy")
	}
	return v, nil
}
func compileLimitsRuntime(r ControlRequest, paths ResolvedPaths, input install.RuntimeInput) ([]byte, error) {
	compiled, err := CompileNotification(unboundCommandDeclaration(r.Desired), r.Facts, paths, NotificationBindings{Providers: input.Providers, LEDSocket: input.LEDSocket, LEDEndpoint: input.LEDEndpoint})
	if err != nil {
		return nil, err
	}
	// Output disablement retains the daemon endpoint in the runtime; its
	// separate disposition controls admission. Compiling disabled YAML from
	// scratch would drop that endpoint and silently turn a budget update into
	// an LED configuration migration.
	compiled.Input.LEDSocket, compiled.Input.LEDEndpoint = input.LEDSocket, input.LEDEndpoint
	preserveRuntimeRouteLabels(input, &compiled.Input)
	return compiled.Input.Bytes()
}
func (s *notificationLimitsStep) verifyInputs(ctx context.Context, v notificationLimitsIntent) error {
	var pins notificationInputPins
	if err := s.directory.ReadJSON("notification-inputs.json", &pins); err != nil {
		return err
	}
	if pins != v.Inputs && pins != v.afterInputs() {
		return fmt.Errorf("bootstrap policy credential pins changed")
	}
	raw, err := readLimitsRuntime(s.paths.Runtime)
	if err != nil {
		return err
	}
	hash := inputDigest(raw)
	if hash != inputDigest(v.BeforeRuntime) && hash != inputDigest(v.AfterRuntime) {
		return fmt.Errorf("bootstrap policy runtime changed outside the operation")
	}
	if s.kind == "providers" {
		if err := verifyProviderRuntimeInputs(v.BeforeRuntime, v.Inputs); err != nil {
			return err
		}
		if err := verifyProviderRuntimeInputs(v.AfterRuntime, v.afterInputs()); err != nil {
			return err
		}
		return verifyProviderRetainedInputs(ctx, s.request, s.paths, s.directory, s.before, v.Before, v.Inputs, s.lifecycle)
	}
	return verifyLEDOutputInputs(s.request, s.paths, s.directory, s.before, inputDigest(v.BeforeRuntime), inputDigest(v.AfterRuntime))
}
func (s *notificationLimitsStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	o, err := s.lifecycle.inspect(ctx, s.paths.Installation)
	if err != nil {
		return "", false, err
	}
	v, err := s.intent(op)
	if errors.Is(err, os.ErrNotExist) && o.Phase == "complete" && protocol.Hash(o.Manifest) == s.expected {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if err = s.verifyInputs(ctx, v); err != nil {
		return "", false, err
	}
	if o.Phase != "complete" || s.kind != "command-draft" && protocol.Hash(o.Manifest) == s.expected {
		return "", false, nil
	}
	if !o.Unbound {
		return "", false, fmt.Errorf("bootstrap policy operation lost unbound authority")
	}
	if err = validateLimitsManifest(v, o.Manifest); err != nil {
		return "", false, err
	}
	var pins notificationInputPins
	if err = s.directory.ReadJSON("notification-inputs.json", &pins); err != nil {
		return "", false, err
	}
	raw, err := readLimitsRuntime(s.paths.Runtime)
	if err != nil {
		return "", false, err
	}
	if pins != v.afterInputs() || inputDigest(raw) != inputDigest(v.AfterRuntime) {
		return "", false, nil
	}
	if _, err = readDeclaration(s.directory, s.request.DesiredHash, s.request.DesiredScope); err != nil {
		if s.kind == "command-draft" && errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	s.manifest = o.Manifest
	return protocol.Hash(o.Manifest), true, nil
}
func (s *notificationLimitsStep) Run(ctx context.Context, op string) error {
	v, err := s.intent(op)
	if errors.Is(err, os.ErrNotExist) {
		o, e := s.lifecycle.inspect(ctx, s.paths.Installation)
		if e != nil {
			return e
		}
		if !o.Unbound || o.Phase != "complete" || protocol.Hash(o.Manifest) != s.expected {
			return fmt.Errorf("gateway changed before bootstrap policy migration")
		}
		if s.kind == "providers" {
			err = verifyProviderPredecessor(s.paths, s.directory)
		} else {
			err = verifyLEDOutputInputs(s.request, s.paths, s.directory, s.before)
		}
		if err != nil {
			return err
		}
		v = notificationLimitsIntent{Kind: s.kind, Version: 1, Operation: op, DesiredHash: s.request.DesiredHash, Before: o.Manifest}
		if s.kind == "routes" {
			v.Version = 2
		}
		if s.kind == "command-draft" {
			v.Version = 5
		}
		if err = s.directory.ReadJSON("notification-inputs.json", &v.Inputs); err != nil {
			return err
		}
		v.BeforeRuntime, err = readLimitsRuntime(s.paths.Runtime)
		if err != nil {
			return err
		}
		var input install.RuntimeInput
		if err = safefs.DecodeJSON(v.BeforeRuntime, &input, maxControlFrame); err != nil {
			return err
		}
		v.AfterRuntime, err = compileLimitsRuntime(s.request, s.paths, input)
		if s.kind == "providers" {
			v.Version = 3
			if v.Inputs.Version == 2 {
				v.Version = 4
			}
			var pins notificationInputPins
			v.AfterRuntime, pins, err = compileProvidersRuntime(s.request, s.paths, input, v.Inputs)
			v.NextInputs = &pins
		}
		if err != nil {
			return err
		}
		if err = validateLimitsIntent(v); err != nil {
			return err
		}
		err = s.directory.WriteJSON("limits-"+op+".json", v, false)
	}
	if err != nil {
		return err
	}
	if err = s.verifyInputs(ctx, v); err != nil {
		return err
	}
	if err = s.boundary("limits-intent"); err != nil {
		return err
	}
	if err = retainDeclaration(s.directory, s.request.Desired, s.request.DesiredHash, s.request.DesiredScope); err != nil {
		return err
	}
	if s.kind == "command-draft" {
		o, e := s.lifecycle.inspect(ctx, s.paths.Installation)
		if e != nil {
			return e
		}
		if !o.Unbound || o.Phase != "complete" {
			return fmt.Errorf("command draft lost unbound installation")
		}
		if e = validateLimitsManifest(v, o.Manifest); e != nil {
			return e
		}
		return s.boundary("command-draft-published")
	}
	candidate := filepath.Join(s.paths.Control, "limits-"+op+"-runtime.json")
	if raw, e := readPrivateGeneratedInput(candidate, len(v.AfterRuntime)); errors.Is(e, os.ErrNotExist) {
		if err = publishPrivateInput(candidate, v.AfterRuntime); err != nil {
			return err
		}
	} else if e != nil || inputDigest(raw) != inputDigest(v.AfterRuntime) {
		return fmt.Errorf("retained bootstrap policy candidate changed")
	}
	o, err := s.lifecycle.inspect(ctx, s.paths.Installation)
	if err != nil {
		return err
	}
	if o.Phase != "complete" || protocol.Hash(o.Manifest) == s.expected {
		spec := install.ManagedSpec{Version: 1, Root: s.paths.Installation, State: s.paths.State, UnitDirectory: s.paths.UnitDirectory, RuntimeFile: candidate, BinaryFile: s.helper, BinarySHA256: s.request.HelperSHA256, DeniedPaths: s.paths.DeniedPaths, FreshSignal: v.Before.FreshSignal}
		if _, err = s.lifecycle.migrate(ctx, spec, s.expected); err != nil {
			return err
		}
		o, err = s.lifecycle.inspect(ctx, s.paths.Installation)
		if err != nil {
			return err
		}
	}
	if !o.Unbound || o.Phase != "complete" {
		return fmt.Errorf("bootstrap policy migration completion unavailable")
	}
	if err = validateLimitsManifest(v, o.Manifest); err != nil {
		return err
	}
	if err = s.boundary("limits-domain-complete"); err != nil {
		return err
	}
	if err = replaceDeclaredRuntime(s.paths.Runtime, v.BeforeRuntime, v.AfterRuntime); err != nil {
		return err
	}
	if err = s.boundary("limits-runtime-published"); err != nil {
		return err
	}
	if err = s.directory.WriteJSON("notification-inputs.json", v.afterInputs(), true); err != nil {
		return err
	}
	return s.boundary("limits-inputs-published")
}

func verifyLimitsReceipt(d *safefs.Directory, j OperationJournal, m install.ManagedManifest) error {
	if !limitsJournal(j) {
		return nil
	}
	var prior OperationJournal
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
		return err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous || !unboundPredecessor(prior) {
		return fmt.Errorf("bootstrap policy predecessor changed")
	}
	var v notificationLimitsIntent
	if err := d.ReadJSON("limits-"+j.Children[0].Operation+".json", &v); err != nil {
		return err
	}
	if err := validateLimitsIntent(v); err != nil {
		return err
	}
	if v.Kind != policyJournalKind(j) || v.Operation != j.Children[0].Operation || v.DesiredHash != j.Pins.DesiredHash || v.Before.BinaryHash != j.Pins.Artifacts["helper"] || protocol.Hash(v.Before) != prior.Children[0].Receipt || protocol.Hash(m) != j.Children[0].Receipt {
		return fmt.Errorf("bootstrap policy receipt changed")
	}
	var pins notificationInputPins
	if err := d.ReadJSON("notification-inputs.json", &pins); err != nil {
		return err
	}
	if pins != v.afterInputs() {
		return fmt.Errorf("bootstrap policy completion input pins changed")
	}
	if v.Kind == "command-draft" {
		before, e := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
		if e != nil {
			return e
		}
		after, e := readDeclaration(d, j.Pins.DesiredHash, j.Pins.DesiredScope)
		if e != nil {
			return e
		}
		if e = validateCommandDraftDeclaration(before, after); e != nil {
			return e
		}
		expected := prior.Pins
		expected.Previous, expected.PreviousOperation, expected.DesiredHash = prior.Digest(), prior.ID, j.Pins.DesiredHash
		if !reflect.DeepEqual(expected, j.Pins) {
			return fmt.Errorf("command draft changed installation authority")
		}
	}
	if v.Kind == "providers" {
		if err := verifyProviderRuntimeInputs(v.BeforeRuntime, v.Inputs); err != nil {
			return err
		}
		if err := verifyProviderRuntimeInputs(v.AfterRuntime, v.afterInputs()); err != nil {
			return err
		}
	}
	raw, err := readLimitsRuntime(j.Pins.Installations["runtime"])
	if err != nil {
		return err
	}
	if inputDigest(raw) != inputDigest(v.AfterRuntime) {
		return fmt.Errorf("bootstrap policy completion runtime changed")
	}
	return validateLimitsManifest(v, m)
}
