package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type deliveryProviderLifecycle struct {
	backupReview    func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error)
	kind            string
	commandReceiver func(context.Context, *safefs.Directory, OperationJournal) error
	nativeResume    func(context.Context, ControlRequest, ResolvedPaths) error
	inspect         func(context.Context, string) (install.InstallationObservation, error)
	review          func(context.Context, string, string, []byte) (install.DeliveryProviderReview, error)
	inspectPause    func(context.Context, install.DeliveryPauseRequest) (install.ManagedManifest, bool, error)
	pause           func(context.Context, install.DeliveryPauseRequest) (install.ManagedManifest, error)
	inspectRotate   func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, bool, error)
	rotate          func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error)
	scope           func(context.Context, string, backup.VerificationNotificationScopeChange, []string, func(backup.ProducerReservation) (string, error)) (string, error)
	scopeInspect    func(string, string, string, string) (string, bool, error)
	inputs          func(context.Context, ControlRequest, ResolvedPaths, *safefs.Directory, OperationJournal) error
	delivery        notificationDeliveryLifecycle
	boundary        func(string) error
}

func defaultDeliveryProviders(helper string) deliveryProviderLifecycle {
	return deliveryProviderLifecycle{backupReview: backup.InspectSetupPolicy, inspect: install.InspectRetainedInstallation, review: install.ReviewDeliveryProviderRotation, inspectPause: install.InspectDeliveryPause, pause: install.EnsureDeliveryPause, inspectRotate: install.InspectPausedDeliveryProvider, rotate: install.RotatePausedDeliveryProvider, scope: backup.ReconcileDeliveryProviderScope, scopeInspect: backup.InspectRetainedDeliveryPolicyScope, inputs: verifyRequalificationInputs, delivery: defaultNotificationDelivery(), nativeResume: func(ctx context.Context, r ControlRequest, p ResolvedPaths) error {
		return resumeCompletedSignalChild(ctx, r, p, helper, os.Getenv("DEPLOY_CONTROL_SIGNAL_ARTIFACT"), defaultSignalSetupLifecycle())
	}}
}
func deliveryProviderJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && (j.Children[0].Name == "notification-command-provider" || j.Children[0].Name == "notification-command-destination" || j.Children[0].Name == "notification-command-routes" || j.Children[0].Name == "notification-command-derived-freshness" || j.Children[0].Name == "notification-command-report-route" || j.Children[0].Name == "notification-command-report-route-declaration" || j.Children[0].Name == "notification-delivery-report-route" || j.Children[0].Name == "notification-delivery-report-route-declaration" || j.Children[0].Name == "notification-command-producer" || j.Children[0].Name == "notification-command-declaration" || j.Children[0].Name == "notification-command-freshness" || j.Children[0].Name == "notification-command-limits" || j.Children[0].Name == "notification-delivery-derived-freshness" || j.Children[0].Name == "notification-delivery-providers" || j.Children[0].Name == "notification-delivery-limits" || j.Children[0].Name == "notification-delivery-producer" || j.Children[0].Name == "notification-delivery-routes" || j.Children[0].Name == "notification-delivery-destination" || j.Children[0].Name == "notification-delivery-provider-enrollment" || j.Children[0].Name == "notification-delivery-declaration")
}

type deliveryProviderIntent struct {
	Kind          string                  `json:"kind,omitempty"`
	Version       int                     `json:"version"`
	Operation     string                  `json:"operation"`
	DesiredHash   string                  `json:"desired_hash"`
	Before        install.ManagedManifest `json:"before"`
	BeforeRuntime []byte                  `json:"before_runtime"`
	AfterRuntime  []byte                  `json:"after_runtime"`
	BeforeInputs  *notificationInputPins  `json:"before_inputs"`
	AfterInputs   *notificationInputPins  `json:"after_inputs"`
	Credentials   []string                `json:"credentials"`
}

func validateDeliveryProviderIntent(v deliveryProviderIntent) error {
	if v.Version != 1 || !safefs.ValidID(v.Operation, "n") || !validDigest(v.DesiredHash) || validateDeliveryPolicyBefore(v.Before, v.Kind) != nil || v.Before.RuntimeHash != inputDigest(v.BeforeRuntime) || !slices.IsSorted(v.Credentials) || v.Kind == "" && len(v.Credentials) == 0 || len(slices.Compact(slices.Clone(v.Credentials))) != len(v.Credentials) {
		return fmt.Errorf("invalid delivery provider intent")
	}
	if err := validateSharedProviderPins(notificationSharedLimitsIntent{Version: 7, BeforeRuntime: v.BeforeRuntime, AfterRuntime: v.AfterRuntime, BeforeInputs: v.BeforeInputs, AfterInputs: v.AfterInputs}); err != nil {
		return err
	}
	var b, a install.RuntimeInput
	if err := safefs.DecodeJSON(v.BeforeRuntime, &b, maxControlFrame); err != nil {
		return err
	}
	if err := safefs.DecodeJSON(v.AfterRuntime, &a, maxControlFrame); err != nil {
		return err
	}
	before, err := b.Resolve()
	if err != nil {
		return err
	}
	after, err := a.Resolve()
	if err != nil {
		return err
	}
	if protocol.Hash(before) != v.Before.ConfigHash {
		return fmt.Errorf("provider intent changed source runtime")
	}
	paused := v.Before
	paused.Mode, paused.DeliveryActivation = "verification", nil
	if isCommandDeliveryPolicy(v.Kind) && isDeliveryDeclaration(v.Kind) {
		if string(v.BeforeRuntime) != string(v.AfterRuntime) {
			return fmt.Errorf("command declaration changed retained runtime bytes")
		}
		paused.Commands = nil
		return install.ValidateCommandDeliveryDeclaration(paused, before, after)
	}
	if isCommandDeliveryPolicy(v.Kind) {
		paused.Commands = nil
		if commandProviderPolicy(v.Kind) {
			if v.Kind == "command-destination" {
				return install.ValidatePausedCommandDeliveryDestination(paused, before, after)
			}
			return install.ValidatePausedCommandDeliveryProviders(paused, before, after)
		}
		if v.Kind == "command-routes" {
			return install.ValidatePausedCommandDeliveryRoutes(paused, before, after)
		}
		if v.Kind == "command-producer" || v.Kind == "command-report-route" {
			return install.ValidatePausedCommandDeliveryProducerPolicy(paused, before, after)
		}
		if v.Kind == "command-freshness" || v.Kind == "command-derived-freshness" {
			return install.ValidatePausedCommandDeliveryFreshness(paused, before, after)
		}
		return install.ValidatePausedCommandDeliveryLimits(paused, before, after)
	}
	if v.Kind == "limits" {
		return install.ValidatePausedDeliveryLimits(paused, before, after)
	}
	if isDeliveryDeclaration(v.Kind) {
		if string(v.BeforeRuntime) != string(v.AfterRuntime) {
			return fmt.Errorf("declaration continuation changed retained runtime bytes")
		}
		return install.ValidatePausedDeliveryDeclaration(paused, before, after)
	}
	if v.Kind == "provider-enrollment" {
		return install.ValidatePausedDeliveryProviderEnrollment(paused, before, after)
	}
	if v.Kind == "destination" {
		return install.ValidatePausedDeliveryDestination(paused, before, after)
	}
	if v.Kind == "routes" {
		return install.ValidatePausedDeliveryRoutes(paused, before, after)
	}
	if v.Kind == "producer" || v.Kind == "derived-freshness" || v.Kind == "report-route" {
		return install.ValidatePausedDeliveryProducerPolicy(paused, before, after)
	}
	if v.Kind != "" {
		return fmt.Errorf("unknown delivery policy intent")
	}
	return install.ValidateDeliveryProviderRotation(paused, before, after)
}
func deliveryProviderPredecessor(d *safefs.Directory, j OperationJournal) (OperationJournal, error) {
	var prior OperationJournal
	if validateOperationJournal(j) != nil || !deliveryProviderJournal(j) {
		return prior, fmt.Errorf("invalid delivery provider coordinator")
	}
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
		return prior, err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous || !deliveryPolicyCandidate(prior, deliveryProviderKind(j)) {
		return prior, fmt.Errorf("provider rotation lost delivery predecessor")
	}
	before, err := readDeliveryPolicyDeclaration(d, prior, deliveryProviderKind(j))
	if err != nil {
		return prior, err
	}
	after, err := readDeliveryPolicyDeclaration(d, j, deliveryProviderKind(j))
	if err != nil {
		return prior, err
	}
	if err = validateDeliveryPolicyDeclaration(before, after, deliveryProviderKind(j)); err != nil {
		return prior, err
	}
	if !reflect.DeepEqual(deliveryOutputPins(prior, after, j.Pins.DesiredHash), j.Pins) {
		return prior, fmt.Errorf("provider rotation changed target, helper or paths")
	}
	return prior, nil
}
func readDeliveryProviderIntent(d *safefs.Directory, j OperationJournal) (deliveryProviderIntent, error) {
	var v deliveryProviderIntent
	prior, err := deliveryProviderPredecessor(d, j)
	if err != nil {
		return v, err
	}
	if err = d.ReadJSON("delivery-providers-"+j.Children[0].Operation+".json", &v); err != nil {
		return v, err
	}
	if err = validateDeliveryProviderIntent(v); err != nil {
		return v, err
	}
	if v.Kind != deliveryProviderKind(j) || v.Operation != j.Children[0].Operation || v.DesiredHash != j.Pins.DesiredHash || protocol.Hash(v.Before) != prior.Children[0].Receipt || v.Before.Root != j.Pins.Installations["notification-worker"] || v.Before.BinaryHash != j.Pins.Artifacts["helper"] {
		return v, fmt.Errorf("provider rotation intent lost coordinator pins")
	}
	return v, nil
}
func validateDeliveryProviderManifest(v deliveryProviderIntent, m install.ManagedManifest) error {
	if isDeliveryDeclaration(v.Kind) {
		if protocol.Hash(v.Before) != protocol.Hash(m) {
			return fmt.Errorf("declaration continuation changed manifest")
		}
		return nil
	}
	want := v.Before
	want.Mode, want.DeliveryActivation = "verification", nil
	if isCommandDeliveryPolicy(v.Kind) {
		want.Commands = nil
	}
	if v.Kind != "" {
		var source install.RuntimeInput
		if err := safefs.DecodeJSON(v.BeforeRuntime, &source, maxControlFrame); err != nil {
			return err
		}
		config, err := source.Resolve()
		if err != nil {
			return err
		}
		want.DeliveryOwnerConfigHash = v.Before.DeliveryOwnerConfigHash
		if want.DeliveryOwnerConfigHash == "" {
			want.DeliveryOwnerConfigHash = protocol.Hash(config.Worker)
		}
	}
	if commandProviderPolicy(v.Kind) || v.Kind == "" || v.Kind == "destination" || v.Kind == "provider-enrollment" {
		want.ReportCredentialHistory = m.ReportCredentialHistory
		if m.ReportCredentialHistory == nil || m.ReportCredentialHistory.Operation != m.Revision || !validDigest(m.ReportCredentialHistory.Hash) {
			return fmt.Errorf("provider rotation lost credential exclusion history")
		}
	}
	want.Revision, want.RuntimeHash, want.ConfigHash, want.UnitHash = m.Revision, inputDigest(v.AfterRuntime), m.ConfigHash, m.UnitHash
	want.Paths.Executable = filepath.Join(want.Root, "releases", m.Revision, "deploy")
	want.Paths.Config = filepath.Join(want.Root, "releases", m.Revision, "runtime.json")
	var input install.RuntimeInput
	if err := safefs.DecodeJSON(v.AfterRuntime, &input, maxControlFrame); err != nil {
		return err
	}
	runtime, err := input.Resolve()
	if err != nil {
		return err
	}
	if m.ConfigHash != protocol.Hash(runtime) || !safefs.ValidID(m.Revision, "n") || !validDigest(m.UnitHash) || protocol.Hash(want) != protocol.Hash(m) {
		return fmt.Errorf("provider rotation changed gateway ownership or runtime")
	}
	return nil
}

func verifyDeliveryProviderReceipt(d *safefs.Directory, j OperationJournal, m install.ManagedManifest, inspect func(string, string, string, string) (string, bool, error)) error {
	v, err := readDeliveryProviderIntent(d, j)
	if err != nil {
		return err
	}
	if protocol.Hash(m) != j.Children[0].Receipt {
		return fmt.Errorf("provider rotation completion receipt changed")
	}
	if err = validateDeliveryProviderManifest(v, m); err != nil {
		return err
	}
	if isDeliveryDeclaration(v.Kind) {
		return verifyMappingScope(d, j, inspect)
	}
	var paused install.ManagedManifest
	if err = d.ReadJSON("delivery-provider-pause-"+v.Operation+".json", &paused); err != nil {
		return err
	}
	if err = validateDeliveryPolicyPause(v.Before, paused, v.Kind); err != nil {
		return err
	}
	return verifyMappingScope(d, j, inspect)
}

func reconcileDeliveryProviders(ctx context.Context, r ControlRequest, p ResolvedPaths, helper string, l deliveryProviderLifecycle) (ControlResult, bool, error) {
	result := ControlResult{Version: 1}
	d, err := safefs.Open(p.Control)
	if errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	}
	if err != nil {
		return result, true, err
	}
	defer d.Close()
	var current OperationJournal
	if err = d.ReadJSON("operation.json", &current); errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	} else if err != nil {
		return result, true, err
	}
	if validateOperationJournal(current) != nil {
		return result, true, fmt.Errorf("invalid delivery provider coordinator")
	}
	if commandDeliveryPolicyJournal(current) && current.Complete && isCommandDeliveryPolicy(l.kind) {
		return result, false, nil
	}
	completedDeclarationChanged := deliveryDeclarationJournal(current) && current.Complete && current.Pins.DesiredHash != r.DesiredHash
	if deliveryDeclarationJournal(current) && current.Complete {
		full, e := r.Desired.Fingerprint()
		if e != nil {
			return result, true, e
		}
		retainedFull := current.Pins.Artifacts["command-backup-policy"]
		if retainedFull == "" {
			saved, e := readDeclaration(d, current.Pins.DesiredHash, current.Pins.DesiredScope)
			if e != nil {
				return result, true, e
			}
			retainedFull, e = saved.Fingerprint()
			if e != nil {
				return result, true, e
			}
		}
		completedDeclarationChanged = completedDeclarationChanged || full != retainedFull
	}
	retry := deliveryProviderJournal(current) && !completedDeclarationChanged
	if retry && deliveryProviderKind(current) != l.kind {
		return result, false, nil
	}
	if !retry && !deliveryPolicyCandidate(current, l.kind) {
		return result, false, nil
	}
	if retry && isCommandDeliveryPolicy(l.kind) {
		full, e := r.Desired.Fingerprint()
		if e != nil || full != current.Pins.Artifacts["command-backup-policy"] {
			return result, true, fmt.Errorf("resume command policy changes with their retained backup declaration")
		}
	}
	prior := current
	if retry {
		prior, err = deliveryProviderPredecessor(d, current)
		if err != nil {
			return result, true, err
		}
	}
	before, err := readDeliveryPolicyDeclaration(d, prior, l.kind)
	if err != nil {
		return result, true, err
	}
	if !retry && isDeliveryDeclaration(l.kind) && validateDeliveryPolicyDeclaration(before, r.Desired, l.kind) != nil {
		return result, false, nil
	}
	_, explicitProducer := before.Notification.Producers["backup-worker"]
	if !retry && l.kind == "command-producer" && validateDeliveryPolicyDeclaration(before, r.Desired, "command-freshness") == nil {
		return result, false, nil
	}
	if !retry && derivedFreshnessPolicy(l.kind) && (explicitProducer || before.Backup == nil || r.Desired.Backup == nil || before.Backup.Freshness == r.Desired.Backup.Freshness) {
		return result, false, nil
	}
	if !retry && ((l.kind == "routes" || l.kind == "command-routes") && reflect.DeepEqual(before.Notification.Routes, r.Desired.Notification.Routes) || (l.kind == "producer" || l.kind == "command-freshness" || l.kind == "command-producer" || l.kind == "command-declaration") && reflect.DeepEqual(before.Notification.Producers, r.Desired.Notification.Producers) || (l.kind == "limits" || l.kind == "command-limits") && before.Notification.Limits == r.Desired.Notification.Limits || (commandProviderPolicy(l.kind) || l.kind == "" || l.kind == "destination" || l.kind == "provider-enrollment") && reflect.DeepEqual(before.Notification.Providers, r.Desired.Notification.Providers)) {
		return result, false, nil
	}
	if !retry && reportRoutePolicy(l.kind) && (before.Backup == nil || r.Desired.Backup == nil || before.Backup.Notifications.Route == r.Desired.Backup.Notifications.Route) {
		return result, false, nil
	}
	if !prior.Complete || r.DesiredScope != 1 || prior.Pins.DesiredScope != 1 || prior.Pins.UID != r.Facts.UID || prior.Pins.Home != r.Facts.Home || prior.Pins.Worker != r.Worker || prior.Pins.Artifacts["helper"] != r.HelperSHA256 || prior.Pins.Installations["notification-worker"] != p.Installation || prior.Pins.Installations["state"] != p.State || prior.Pins.Installations["runtime"] != p.Runtime {
		return result, true, fmt.Errorf("credential rotation requires unchanged target, paths and helper")
	}
	if !retry && l.kind == "provider-enrollment" {
		_, had := before.Notification.Providers["telegram"]
		_, has := r.Desired.Notification.Providers["telegram"]
		if had == has {
			return result, false, nil
		}
	}
	comparison := r.Desired
	if retry && current.Complete && current.Pins.DesiredHash == r.DesiredHash {
		comparison, err = readDeclaration(d, current.Pins.DesiredHash, current.Pins.DesiredScope)
		if err != nil {
			return result, true, err
		}
	}
	if err = validateDeliveryPolicyDeclaration(before, comparison, l.kind); err != nil {
		return result, true, err
	}
	if !retry {
		if err = reviewDeliveryBackup(ctx, r, before, prior.Pins.DesiredHash, l.kind, l); err != nil {
			return result, true, err
		}
	}
	if l.commandReceiver != nil {
		if err = l.commandReceiver(ctx, d, prior); err != nil {
			return result, true, err
		}
	}
	before, err = deliveryBackupBefore(before, comparison, l.kind)
	if err != nil {
		return result, true, err
	}

	if r.Action == "resume" && l.nativeResume != nil {
		if retry {
			if err = verifyPendingDeliveryProviderSignal(r, p, d, current); err != nil {
				return result, true, err
			}
		}
		nativeRequest := r
		if !retry {
			if nativeRequest, err = reviewedDeliveryPredecessorInputs(r, p, d, prior, before); err != nil {
				return result, true, err
			}
		}
		if err = l.nativeResume(ctx, nativeRequest, p); err != nil {
			return result, true, err
		}
	}
	var enrollment consumerEnrollmentIntent
	if err = d.ReadJSON("consumer-enrollment.json", &enrollment); err != nil {
		return result, true, err
	}
	if err = enrollment.Reservation.Validate(enrollment.Reservation.Producer.ID); err != nil {
		return result, true, err
	}
	var intent deliveryProviderIntent
	retained := retry
	if retry {
		var probe deliveryProviderIntent
		e := d.ReadJSON("delivery-providers-"+current.Children[0].Operation+".json", &probe)
		if errors.Is(e, os.ErrNotExist) && !current.Complete && !current.Children[0].Complete {
			// Runtime/pause evidence is written only after the exact intent.
			// Its later loss cannot be treated as the pre-intent boundary,
			// including when an equivalent declaration kept the manifest fixed.
			for _, name := range []string{"delivery-providers-" + current.Children[0].Operation + "-runtime.json", "delivery-provider-pause-" + current.Children[0].Operation + ".json"} {
				if _, readErr := safefs.ReadFile(d, name, maxControlFrame); !errors.Is(readErr, os.ErrNotExist) {
					return result, true, fmt.Errorf("delivery policy intent missing after retained effects; recovery review required")
				}
			}
			retained = false
		} else if e != nil {
			return result, true, e
		}
	}
	if retained {
		intent, err = readDeliveryProviderIntent(d, current)
		if err != nil {
			return result, true, err
		}
		if intent.DesiredHash != r.DesiredHash {
			return result, true, fmt.Errorf("resume the exact credential rotation YAML")
		}
		var input install.RuntimeInput
		if err = safefs.DecodeJSON(intent.BeforeRuntime, &input, maxControlFrame); err != nil {
			return result, true, err
		}
		reviewed, e := retainedProviderReviewRequest(r, before, input)
		if e != nil {
			return result, true, e
		}
		if err = verifyLEDOutputInputs(reviewed, p, d, before); err != nil {
			return result, true, err
		}
		if err = verifyProviderHistoryInputs(d, prior); err != nil {
			return result, true, err
		}
	} else {
		previous := r
		previous.Desired, previous.DesiredHash = before, prior.Pins.DesiredHash
		if err = l.inputs(ctx, previous, p, d, prior); err != nil {
			return result, true, err
		}
		origin, e := deliveryPolicyRuntimeOrigin(d, prior, l.kind)
		if e != nil {
			return result, true, e
		}
		raw, e := retainedEnrolledRuntime(d, p, origin)
		if e != nil {
			return result, true, e
		}
		var input install.RuntimeInput
		if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
			return result, true, err
		}
		compile := compileSharedProviders
		if l.kind != "" && l.kind != "destination" && l.kind != "provider-enrollment" {
			compile = compileDeliveryLimits
			if isCommandDeliveryPolicy(l.kind) {
				compile = compileCommandDeliveryLimits
				if commandProviderPolicy(l.kind) {
					compile = compileCommandDeliveryProviders
				}
				if l.kind == "command-routes" {
					compile = compileCommandDeliveryRoutes
				}
				if l.kind == "command-freshness" || l.kind == "command-derived-freshness" {
					compile = compileCommandDeliveryFreshness
				}
				if l.kind == "command-producer" || l.kind == "command-declaration" || reportRoutePolicy(l.kind) {
					compile = compileCommandDeliveryProducer
				}
			}
		}
		compiled, b, a, e := compile(r, p, d, prior, input, raw, enrollment.Reservation)
		if e != nil {
			return result, true, e
		}
		if l.kind == "destination" || commandProviderPolicy(l.kind) {
			beforeRuntime, err := input.Resolve()
			if err != nil {
				return result, true, err
			}
			afterRuntime, err := compiled.Input.Resolve()
			if err != nil {
				return result, true, err
			}
			changed := false
			for name, old := range beforeRuntime.Providers {
				if old.Kind == "telegram" || old.Kind == "signal" {
					next, ok := afterRuntime.Providers[name]
					changed = changed || ok && old.Destination != next.Destination
				}
			}
			if changed != (l.kind != "command-provider") {
				return result, false, nil
			}
		}

		after, e := compiled.Input.Bytes()
		if e != nil {
			return result, true, e
		}
		if isDeliveryDeclaration(l.kind) {
			oldRuntime, e := input.Resolve()
			if e != nil {
				return result, true, e
			}
			newRuntime, e := compiled.Input.Resolve()
			if e != nil {
				return result, true, e
			}
			if protocol.Hash(oldRuntime) != protocol.Hash(newRuntime) {
				return result, false, nil
			}
			after = raw
			a.Runtime = inputDigest(raw)
		}

		review, e := l.review(ctx, p.Installation, prior.Children[0].Receipt, after)
		if e != nil {
			return result, true, e
		}
		intent = deliveryProviderIntent{Version: 1, Kind: l.kind, DesiredHash: r.DesiredHash, Before: review.Before, BeforeRuntime: raw, AfterRuntime: after, BeforeInputs: b, AfterInputs: a, Credentials: review.Credentials}
	}
	check := intent
	check.Operation = prior.Children[0].Operation
	if err = validateDeliveryProviderIntent(check); err != nil {
		return result, true, err
	}
	if protocol.Hash(intent.Before) != prior.Children[0].Receipt {
		return result, true, fmt.Errorf("credential review changed gateway pins")
	}
	step := &deliveryProviderStep{d: d, r: r, p: p, helper: helper, priorScope: prior.Pins.DesiredHash, reservation: enrollment.Reservation, intent: intent, l: l}
	if err = retainDeclaration(d, r.Desired, r.DesiredHash, 1); err != nil {
		return result, true, err
	}
	if isCommandDeliveryPolicy(l.kind) {
		full, e := r.Desired.Fingerprint()
		if e != nil {
			return result, true, e
		}
		if e = retainDeclaration(d, r.Desired, full); e != nil {
			return result, true, e
		}
	}
	completed, err := (JournalCoordinator{Directory: d}).Apply(ctx, deliveryOutputPins(prior, r.Desired, r.DesiredHash), []LifecycleStep{step})
	if err != nil {
		return result, true, err
	}
	result.State, result.Code, result.Next = "needs_qualification", "ok", "credentials updated; delivery is paused with reporting and queued work retained; run deploy notification-worker qualify "+r.Server+", then apply"
	if l.kind == "limits" || l.kind == "command-limits" {
		result.Next = "notification limits updated; delivery is paused with queued work retained; run deploy notification-worker qualify " + r.Server + ", then apply"
	}
	if l.kind == "provider-enrollment" {
		result.Next = "Telegram provider enrollment updated; delivery is paused with Signal ownership and reporting retained; qualify, then apply"
	}
	if l.kind == "destination" {
		result.Next = "Telegram destination updated; delivery is paused with original ownership and history retained; qualify the selected destination, then apply"
	}
	if l.kind == "routes" || l.kind == "command-routes" {
		result.Next = "notification routes updated; delivery is paused with reporting and ownership retained; qualify the configuration, then apply"
	}
	if derivedFreshnessPolicy(l.kind) {
		result.Next = "backup freshness and gateway evidence deadline updated; qualify the notification worker and apply to resume delivery; backup capture remains staged until qualified and activated"
	}
	if l.kind == "producer" || l.kind == "command-freshness" || l.kind == "command-producer" || l.kind == "command-declaration" {
		result.Next = "producer policy updated; delivery is paused with original observations and reporting retained; qualify the configuration, then apply"
	}
	result.Identity = &step.manifest.Identity
	if isCommandDeliveryPolicy(l.kind) && isDeliveryDeclaration(l.kind) {
		result.State, result.Next = "commands_disabled", "equivalent producer declaration recorded; delivery, qualification, original receiver and pending work remain; enable commands separately when wanted"
		return result, true, nil
	}
	if isCommandDeliveryPolicy(l.kind) {
		return result, true, nil
	}
	if l.kind == "report-route-declaration" {
		result.State, result.Next = "delivery_admitted", "report route recorded with unchanged runtime and qualification"
		return result, true, nil
	}
	result, err = advanceNotificationDelivery(ctx, r, p, d, completed, result, l.delivery)
	return result, true, err
}

// A pending backup scope cannot be inspected as an earlier completed scope.
// Still verify every retained private-file set before resuming domain effects;
// full backup receipt inspection follows completion of the current scope.
func verifyProviderHistoryInputs(d *safefs.Directory, j OperationJournal) error {
	for depth := 0; depth < 128; depth++ {
		if validateOperationJournal(j) != nil || !j.Complete {
			return fmt.Errorf("provider input history changed")
		}
		if deliveryProviderJournal(j) {
			if _, err := readDeliveryProviderIntent(d, j); err != nil {
				return err
			}
		}
		if sharedLimitsJournal(j) && sharedPolicyKind(j) == "providers" {
			if _, err := readSharedLimitsIntent(d, j); err != nil {
				return err
			}
		}
		if j.Pins.PreviousOperation == "" {
			return nil
		}
		var prior OperationJournal
		if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
			return err
		}
		if prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous {
			return fmt.Errorf("provider input predecessor changed")
		}
		j = prior
	}
	return fmt.Errorf("provider input history exceeds review bound")
}

func reviewedDeliveryPredecessorInputs(r ControlRequest, p ResolvedPaths, d *safefs.Directory, prior OperationJournal, before config.DesiredWorkers) (ControlRequest, error) {
	origin, err := requalificationEnrollment(d, prior)
	if err != nil {
		return r, err
	}
	raw, err := retainedEnrolledRuntime(d, p, origin)
	if err != nil {
		return r, err
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return r, err
	}
	reviewed, err := retainedProviderReviewRequest(r, before, input)
	if err != nil {
		return r, err
	}
	if err = verifyLEDOutputInputs(reviewed, p, d, before); err != nil {
		return r, err
	}
	if err = verifyProviderHistoryInputs(d, prior); err != nil {
		return r, err
	}
	return reviewed, nil
}

// Native resumption cannot use a pending Telegram edit to bypass the retained
// gateway declaration, private files or Signal child authority. The caller
// holds the gateway coordinator lock; native inspection takes its own lock.
func verifyPendingDeliveryProviderSignal(r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal) error {
	if r.DesiredHash != j.Pins.DesiredHash {
		return fmt.Errorf("resume the exact credential rotation YAML before native repair")
	}
	prior, err := deliveryProviderPredecessor(d, j)
	if err != nil {
		return err
	}
	before, err := readDeliveryPolicyDeclaration(d, prior, deliveryProviderKind(j))
	if err != nil {
		return err
	}
	if _, err = reviewedDeliveryPredecessorInputs(r, p, d, prior, before); err != nil {
		return err
	}
	var intent deliveryProviderIntent
	err = d.ReadJSON("delivery-providers-"+j.Children[0].Operation+".json", &intent)
	if errors.Is(err, os.ErrNotExist) && !j.Complete && !j.Children[0].Complete {
		// Only the boundary before the first retained intent can be rebuilt.
		// A generated runtime proves that the now-missing intent existed.
		for _, name := range []string{"delivery-providers-" + j.Children[0].Operation + "-runtime.json", "delivery-provider-pause-" + j.Children[0].Operation + ".json"} {
			if _, e := safefs.ReadFile(d, name, maxControlFrame); !errors.Is(e, os.ErrNotExist) {
				return fmt.Errorf("delivery policy intent is missing after retained effects")
			}
		}
		observed, e := install.InspectRetainedInstallation(context.Background(), p.Installation)
		if e != nil {
			return e
		}
		if observed.Phase != "complete" || protocol.Hash(observed.Manifest) != prior.Children[0].Receipt {
			return fmt.Errorf("missing policy intent cannot restart a changed gateway dependency")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err = validateDeliveryProviderIntent(intent); err != nil {
		return err
	}
	if intent.Kind != deliveryProviderKind(j) || intent.Operation != j.Children[0].Operation || intent.DesiredHash != j.Pins.DesiredHash || intent.Before.BinaryHash != r.HelperSHA256 || protocol.Hash(intent.Before) != prior.Children[0].Receipt {
		return fmt.Errorf("provider intent changed before native resume")
	}
	return nil
}

type deliveryProviderStep struct {
	d                  *safefs.Directory
	r                  ControlRequest
	p                  ResolvedPaths
	helper, priorScope string
	reservation        backup.ProducerReservation
	intent             deliveryProviderIntent
	l                  deliveryProviderLifecycle
	manifest           install.ManagedManifest
}

func (s *deliveryProviderStep) Name() string {
	switch s.intent.Kind {
	case "command-provider":
		return "notification-command-provider"
	case "command-destination":
		return "notification-command-destination"
	case "command-routes":
		return "notification-command-routes"
	case "command-derived-freshness":
		return "notification-command-derived-freshness"
	case "command-report-route":
		return "notification-command-report-route"
	case "command-report-route-declaration":
		return "notification-command-report-route-declaration"
	case "report-route":
		return "notification-delivery-report-route"
	case "report-route-declaration":
		return "notification-delivery-report-route-declaration"
	}

	if s.intent.Kind == "command-declaration" {
		return "notification-command-declaration"
	}
	if s.intent.Kind == "command-producer" {
		return "notification-command-producer"
	}
	if s.intent.Kind == "command-freshness" {
		return "notification-command-freshness"
	}
	if isCommandDeliveryPolicy(s.intent.Kind) {
		return "notification-command-limits"
	}
	if s.intent.Kind == "derived-freshness" {
		return "notification-delivery-derived-freshness"
	}
	if s.intent.Kind == "declaration" {
		return "notification-delivery-declaration"
	}
	if s.intent.Kind == "provider-enrollment" {
		return "notification-delivery-provider-enrollment"
	}
	if s.intent.Kind == "destination" {
		return "notification-delivery-destination"
	}
	if s.intent.Kind == "routes" {
		return "notification-delivery-routes"
	}
	if s.intent.Kind == "producer" {
		return "notification-delivery-producer"
	}
	if s.intent.Kind == "limits" {
		return "notification-delivery-limits"
	}
	return "notification-delivery-providers"
}
func (s *deliveryProviderStep) runtimeName(op string) string {
	return "delivery-providers-" + op + "-runtime.json"
}
func (s *deliveryProviderStep) spec(op string) install.ManagedSpec {
	return install.ManagedSpec{Version: 1, Root: s.p.Installation, State: s.p.State, UnitDirectory: s.p.UnitDirectory, RuntimeFile: filepath.Join(s.p.Control, s.runtimeName(op)), BinaryFile: s.helper, BinarySHA256: s.r.HelperSHA256, DeniedPaths: s.p.DeniedPaths, FreshSignal: s.intent.Before.FreshSignal}
}
func (s *deliveryProviderStep) retained(op string) (deliveryProviderIntent, error) {
	var v deliveryProviderIntent
	err := s.d.ReadJSON("delivery-providers-"+op+".json", &v)
	if err != nil {
		return v, err
	}
	want := s.intent
	want.Operation = op
	if protocol.Hash(v) != protocol.Hash(want) {
		return v, fmt.Errorf("credential rotation intent changed on retry")
	}
	return v, validateDeliveryProviderIntent(v)
}
func (s *deliveryProviderStep) pauseReceipt(op string) (install.ManagedManifest, error) {
	var m install.ManagedManifest
	err := s.d.ReadJSON("delivery-provider-pause-"+op+".json", &m)
	if err == nil {
		err = validateDeliveryPolicyPause(s.intent.Before, m, s.intent.Kind)
	}
	return m, err
}
func (s *deliveryProviderStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	v, err := s.retained(op)
	if errors.Is(err, os.ErrNotExist) {
		o, e := s.l.inspect(ctx, s.p.Installation)
		if e != nil {
			return "", false, e
		}
		if o.Phase == "complete" && protocol.Hash(o.Manifest) == protocol.Hash(s.intent.Before) {
			return "", false, nil
		}
	}
	if err != nil {
		return "", false, err
	}

	if isDeliveryDeclaration(v.Kind) {
		m, e := s.declarationManifest(ctx, v)
		if e != nil {
			return "", false, e
		}
		receipt, done, e := s.l.scopeInspect(s.r.Facts.Home, op, protocol.Hash(v.Before), s.r.DesiredHash)
		if e != nil || !done {
			return "", false, e
		}
		if receipt != protocol.Hash(m) {
			return "", false, fmt.Errorf("declaration scope changed gateway")
		}
		s.manifest = m
		return receipt, true, nil
	}
	m, err := s.pauseReceipt(op)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	a, done, err := s.l.inspectRotate(ctx, s.spec(op), protocol.Hash(m))
	if err != nil || !done {
		return "", false, err
	}
	if err = validateDeliveryProviderManifest(v, a); err != nil {
		return "", false, err
	}
	receipt, complete, err := s.l.scopeInspect(s.r.Facts.Home, op, protocol.Hash(v.Before), s.r.DesiredHash)
	if err != nil || !complete {
		return "", false, err
	}
	if receipt != protocol.Hash(a) {
		return "", false, fmt.Errorf("gateway and backup provider receipts differ")
	}
	s.manifest = a
	return receipt, true, nil
}
func (s *deliveryProviderStep) Run(ctx context.Context, op string) error {
	v, err := s.retained(op)
	if errors.Is(err, os.ErrNotExist) {
		v = s.intent
		v.Operation = op
		if err = validateDeliveryProviderIntent(v); err != nil {
			return err
		}
		err = s.d.WriteJSON("delivery-providers-"+op+".json", v, false)
	}
	if err != nil {
		return err
	}
	name := s.runtimeName(op)
	raw, e := safefs.ReadFile(s.d, name, maxControlFrame)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if e == nil && string(raw) != string(v.AfterRuntime) {
		return fmt.Errorf("retained provider runtime changed")
	}
	if e != nil {
		if err = safefs.ReplaceFile(s.d, name, nil, v.AfterRuntime, 0600); err != nil {
			return err
		}
	}
	if s.l.boundary != nil {
		if err = s.l.boundary("delivery-provider-intent"); err != nil {
			return err
		}
	}
	_, err = s.l.scope(ctx, s.r.Facts.Home, backup.VerificationNotificationScopeChange{Operation: op, BeforeScope: s.priorScope, BeforeGateway: protocol.Hash(v.Before), Desired: s.r.Desired}, v.Credentials, func(p backup.ProducerReservation) (string, error) {
		if p != s.reservation {
			return "", fmt.Errorf("provider rotation changed backup reservation")
		}

		if isDeliveryDeclaration(v.Kind) {
			m, e := s.declarationManifest(ctx, v)
			if e != nil {
				return "", e
			}
			s.manifest = m
			return protocol.Hash(m), nil
		}
		pause, e := s.pauseReceipt(op)
		if errors.Is(e, os.ErrNotExist) {
			r := install.DeliveryPauseRequest{Root: s.p.Installation, ManifestHash: protocol.Hash(v.Before)}
			var complete bool
			pause, complete, e = s.l.inspectPause(ctx, r)
			if e == nil && !complete {
				pause, e = s.l.pause(ctx, r)
			}
			if e == nil {
				e = validateDeliveryPolicyPause(v.Before, pause, v.Kind)
			}
			if e == nil {
				e = s.d.WriteJSON("delivery-provider-pause-"+op+".json", pause, false)
			}
		}
		if e != nil {
			return "", e
		}
		if s.l.boundary != nil {
			if e = s.l.boundary("delivery-provider-paused"); e != nil {
				return "", e
			}
		}
		m, done, e := s.l.inspectRotate(ctx, s.spec(op), protocol.Hash(pause))
		if e != nil {
			return "", e
		}
		if !done {
			m, e = s.l.rotate(ctx, s.spec(op), protocol.Hash(pause))
			if e != nil {
				return "", e
			}
		}
		if e = validateDeliveryProviderManifest(v, m); e != nil {
			return "", e
		}
		if s.l.boundary != nil {
			if e = s.l.boundary("delivery-provider-rotated"); e != nil {
				return "", e
			}
		}
		s.manifest = m
		return protocol.Hash(m), nil
	})
	return err
}

func (s *deliveryProviderStep) declarationManifest(ctx context.Context, v deliveryProviderIntent) (install.ManagedManifest, error) {
	if isCommandDeliveryPolicy(v.Kind) && isDeliveryDeclaration(v.Kind) {
		if _, err := s.l.review(ctx, s.p.Installation, protocol.Hash(v.Before), v.AfterRuntime); err != nil {
			return v.Before, err
		}
	}
	o, err := s.l.inspect(ctx, s.p.Installation)
	if err != nil {
		return o.Manifest, err
	}
	if o.Phase != "complete" || protocol.Hash(o.Manifest) != protocol.Hash(v.Before) {
		return o.Manifest, fmt.Errorf("declaration continuation lost original delivery")
	}
	return o.Manifest, nil
}
