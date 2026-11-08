package workerctl

import (
	"context"
	"errors"
	"fmt"
	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
	"path/filepath"
	"reflect"
)

func defaultSharedLimits() notificationRemappingLifecycle {
	l := defaultNotificationRemapping()
	l.migrate = install.MigrateEnrolledLimits
	l.scope = backup.ReconcileVerificationLimitsScope
	l.scopeReview = backup.InspectSetupPolicy
	return l
}
func sharedLimitsJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && (j.Children[0].Name == "notification-shared-report-route" || j.Children[0].Name == "notification-shared-derived-freshness" || j.Children[0].Name == "notification-shared-command-activity" || j.Children[0].Name == "notification-shared-command-policy" || j.Children[0].Name == "notification-shared-commands" || j.Children[0].Name == "notification-shared-limits" || j.Children[0].Name == "notification-shared-freshness" || j.Children[0].Name == "notification-shared-routes" || j.Children[0].Name == "notification-shared-producer" || j.Children[0].Name == "notification-shared-providers")
}
func sharedLimitsPredecessor(d *safefs.Directory, current OperationJournal) (OperationJournal, error) {
	var prior OperationJournal
	if validateOperationJournal(current) != nil || !sharedLimitsJournal(current) {
		return prior, fmt.Errorf("invalid shared limits journal")
	}
	if err := d.ReadJSON("history/"+current.Pins.PreviousOperation+".json", &prior); err != nil {
		return prior, err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || !enrolledNotificationJournal(prior) || prior.ID != current.Pins.PreviousOperation || prior.Digest() != current.Pins.Previous || prior.Pins.DesiredScope != 1 || current.Pins.DesiredScope != 1 {
		return prior, fmt.Errorf("shared limits predecessor changed")
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
	if err != nil {
		return prior, err
	}
	after, err := readDeclaration(d, current.Pins.DesiredHash, current.Pins.DesiredScope)
	if err != nil {
		return prior, err
	}
	if err = validateSharedPolicy(before, after, prior.Pins.DesiredHash, sharedPolicyKind(current)); err != nil {
		return prior, err
	}
	want := prior.Pins
	want.PreviousOperation, want.Previous, want.DesiredHash = prior.ID, prior.Digest(), current.Pins.DesiredHash
	if !reflect.DeepEqual(want, current.Pins) {
		return prior, fmt.Errorf("shared limits changed installation authority")
	}
	return prior, nil
}
func readSharedLimitsIntent(d *safefs.Directory, current OperationJournal) (notificationSharedLimitsIntent, error) {
	var v notificationSharedLimitsIntent
	prior, err := sharedLimitsPredecessor(d, current)
	if err != nil {
		return v, err
	}
	if err = d.ReadJSON("shared-limits-"+current.Children[0].Operation+".json", &v); err != nil {
		return v, err
	}
	if err = validateSharedLimitsIntent(v); err != nil {
		return v, err
	}
	if (v.Version != sharedPolicyVersion(sharedPolicyKind(current)) && !(sharedPolicyKind(current) == "freshness" && v.Version == 3) && !(sharedPolicyKind(current) == "producer" && v.Version == 6) && !(sharedPolicyKind(current) == "report-route" && v.Version == 13)) || v.Operation != current.Children[0].Operation || v.DesiredHash != current.Pins.DesiredHash || v.Before.BinaryHash != current.Pins.Artifacts["helper"] || protocol.Hash(v.Before) != prior.Children[0].Receipt {
		return v, fmt.Errorf("shared limits intent changed")
	}
	raw, err := readPrivateGeneratedInput(filepath.Join(d.File().Name(), "shared-limits-"+v.Operation+"-runtime.json"), len(v.AfterRuntime))
	if err != nil || string(raw) != string(v.AfterRuntime) {
		return v, fmt.Errorf("shared limits runtime changed")
	}
	return v, nil
}
func verifySharedLimitsReceipt(d *safefs.Directory, j OperationJournal, m install.ManagedManifest, inspect func(string, string, string, string) (string, bool, error)) error {
	v, err := readSharedLimitsIntent(d, j)
	if err != nil {
		return err
	}
	if protocol.Hash(m) != j.Children[0].Receipt {
		return fmt.Errorf("shared limits completion changed")
	}
	if err = validateSharedLimitsManifest(v, m); err != nil {
		return err
	}
	return verifyMappingScope(d, j, inspect)
}
func sharedPolicyKind(j OperationJournal) string {
	if sharedLimitsJournal(j) && j.Children[0].Name == "notification-shared-report-route" {
		return "report-route"
	}
	if sharedLimitsJournal(j) && j.Children[0].Name == "notification-shared-derived-freshness" {
		return "derived-freshness"
	}
	if sharedLimitsJournal(j) && j.Children[0].Name == "notification-shared-command-activity" {
		return "command-activity"
	}
	if sharedLimitsJournal(j) && j.Children[0].Name == "notification-shared-command-policy" {
		return "command-policy"
	}
	if sharedLimitsJournal(j) && j.Children[0].Name == "notification-shared-commands" {
		return "commands"
	}
	if sharedLimitsJournal(j) && j.Children[0].Name == "notification-shared-providers" {
		return "providers"
	}
	if sharedLimitsJournal(j) && j.Children[0].Name == "notification-shared-producer" {
		return "producer"
	}
	if sharedLimitsJournal(j) && j.Children[0].Name == "notification-shared-routes" {
		return "routes"
	}
	if sharedLimitsJournal(j) && j.Children[0].Name == "notification-shared-freshness" {
		return "freshness"
	}
	return ""
}
func sharedPolicyVersion(kind string) int {
	if kind == "report-route" {
		return 12
	}
	if kind == "derived-freshness" {
		return 11
	}
	if kind == "command-activity" {
		return 10
	}
	if kind == "command-policy" {
		return 9
	}
	if kind == "commands" {
		return 8
	}
	if kind == "providers" {
		return 7
	}
	if kind == "producer" {
		return 5
	}
	if kind == "routes" {
		return 4
	}
	if kind == "freshness" {
		return 2
	}
	return 1
}
func validateSharedLimitsPolicy(before, after config.DesiredWorkers, oldScope string) error {
	return validateSharedPolicy(before, after, oldScope, "")
}
func validateSharedPolicy(before, after config.DesiredWorkers, oldScope, kind string) error {
	if kind == "report-route" {
		return config.ValidateSharedReportRouteDeclaration(before, after)
	}
	if kind == "derived-freshness" {
		return config.ValidateDerivedFreshnessDeclaration(before, after)
	}
	if kind == "command-activity" {
		if err := initialBackupCommandSupport(after); err != nil {
			return err
		}
		return config.ValidateVerificationCommandActivityDeclaration(before, after)
	}
	if kind == "command-policy" {
		if err := initialBackupCommandSupport(after); err != nil {
			return err
		}
		return config.ValidateVerificationCommandDeclaration(before, after)
	}
	if kind == "commands" {
		if err := initialBackupCommandSupport(after); err != nil {
			return err
		}
		return config.ValidateInitialCommandDeclaration(before, after)
	}
	if before.Notification == nil || after.Notification == nil {
		return fmt.Errorf("shared policy declaration missing")
	}
	next := after
	n := *after.Notification
	next.Notification = &n
	if kind == "providers" {
		if err := validateSharedProvidersDeclaration(before, after); err != nil {
			return err
		}
		if reflect.DeepEqual(before.Notification.Providers, after.Notification.Providers) {
			return fmt.Errorf("shared providers require changed private references")
		}
		n.Providers = before.Notification.Providers
	} else if kind == "routes" {
		if reflect.DeepEqual(before.Notification.Routes, after.Notification.Routes) {
			return fmt.Errorf("shared routing requires changed routes")
		}
		n.Routes = before.Notification.Routes
	} else if kind == "freshness" || kind == "producer" {
		if reflect.DeepEqual(before.Notification.Producers, after.Notification.Producers) {
			return fmt.Errorf("shared producer policy requires a changed declaration")
		}
		n.Producers = before.Notification.Producers
	} else {
		if before.Notification.Limits == after.Notification.Limits {
			return fmt.Errorf("shared limits require changed limits")
		}
		n.Limits = before.Notification.Limits
	}
	hash, err := next.NotificationFingerprint()
	if err != nil || hash != oldScope {
		return fmt.Errorf("change shared policy separately from other worker settings")
	}
	return nil
}
func reconcileSharedFreshness(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
	return reconcileSharedPolicy(ctx, r, paths, helper, l, "freshness")
}
func reconcileSharedRoutes(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
	return reconcileSharedPolicy(ctx, r, paths, helper, l, "routes")
}
func defaultSharedRoutes() notificationRemappingLifecycle {
	l := defaultSharedLimits()
	l.migrate = install.MigrateEnrolledRoutes
	l.scope = backup.ReconcileVerificationRoutesScope
	return l
}
func defaultSharedFreshness() notificationRemappingLifecycle {
	l := defaultSharedLimits()
	l.migrate = install.MigrateEnrolledFreshness
	l.scope = backup.ReconcileVerificationFreshnessScope
	return l
}

// Shared limits use a domain transaction and a backup scope transaction. Both receipts must complete before qualification or
// delivery can consume the new YAML policy.
func reconcileSharedLimits(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
	return reconcileSharedPolicy(ctx, r, paths, helper, l, "")
}
func reconcileSharedPolicy(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationRemappingLifecycle, kind string) (ControlResult, bool, error) {
	result := ControlResult{Version: 1}
	d, err := safefs.Open(paths.Control)
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
	if err = validateOperationJournal(current); err != nil {
		return result, true, err
	}
	retry := sharedLimitsJournal(current) && sharedPolicyKind(current) == kind && current.Pins.DesiredHash == r.DesiredHash
	if !retry && (admittedCommandHistory(current) || !enrolledNotificationJournal(current) || current.Pins.DesiredHash == r.DesiredHash) {
		return result, false, nil
	}
	prior := current
	if retry {
		prior, err = sharedLimitsPredecessor(d, current)
		if err != nil {
			return result, true, err
		}
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
	if err != nil {
		return result, true, err
	}
	same := before.Notification == nil || r.Desired.Notification == nil
	if !same {
		if kind == "report-route" {
			same = before.Backup == nil || r.Desired.Backup == nil || before.Backup.Notifications.Route == r.Desired.Backup.Notifications.Route
		} else if kind == "derived-freshness" {
			same = before.Backup == nil || r.Desired.Backup == nil || before.Backup.Freshness == r.Desired.Backup.Freshness
		} else if kind == "command-activity" {
			same = before.Notification.Commands.Enabled == r.Desired.Notification.Commands.Enabled
		} else if kind == "command-policy" {
			same = !before.Notification.Commands.Enabled || !r.Desired.Notification.Commands.Enabled || reflect.DeepEqual(before.Notification.Commands, r.Desired.Notification.Commands)
		} else if kind == "commands" {
			same = before.Notification.Commands.Enabled || !r.Desired.Notification.Commands.Enabled
		} else if kind == "providers" {
			same = reflect.DeepEqual(before.Notification.Providers, r.Desired.Notification.Providers)
		} else if kind == "routes" {
			same = reflect.DeepEqual(before.Notification.Routes, r.Desired.Notification.Routes)
		} else if kind == "freshness" || kind == "producer" {
			same = reflect.DeepEqual(before.Notification.Producers, r.Desired.Notification.Producers)
		} else {
			same = before.Notification.Limits == r.Desired.Notification.Limits
		}
	}
	if !retry && same {
		return result, false, nil
	}
	if !retry && kind == "command-activity" && !before.Notification.Commands.Enabled {
		retained, e := worker.VerificationCommandsRetained(paths.State)
		if e != nil {
			return result, true, e
		}
		if !retained {
			return result, false, nil
		}
	}

	if !prior.Complete || !enrolledNotificationJournal(prior) || prior.Pins.DesiredScope != 1 || r.DesiredScope != 1 || prior.Pins.UID != r.Facts.UID || prior.Pins.Home != r.Facts.Home || prior.Pins.Worker != r.Worker || prior.Pins.Artifacts["helper"] != r.HelperSHA256 || prior.Pins.Installations["notification-worker"] != paths.Installation || prior.Pins.Installations["state"] != paths.State || prior.Pins.Installations["runtime"] != paths.Runtime {
		return result, true, fmt.Errorf("later shared limits require unchanged enrolled verification authority; production policy requires a separate migration")
	}
	if err = validateSharedPolicy(before, r.Desired, prior.Pins.DesiredHash, kind); err != nil {
		return result, true, err
	}
	raw, err := retainedEnrolledRuntime(d, paths, prior)
	if err != nil {
		return result, true, err
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return result, true, err
	}
	reviewRequest := r
	if kind == "providers" {
		reviewRequest, err = retainedProviderReviewRequest(r, before, input)
		if err != nil {
			return result, true, err
		}
	}
	if err = verifyLEDOutputInputs(reviewRequest, paths, d, before); err != nil {
		return result, true, err
	}
	var enrollment consumerEnrollmentIntent
	if err = d.ReadJSON("consumer-enrollment.json", &enrollment); err != nil {
		return result, true, err
	}
	reservation := enrollment.Reservation
	if err = reservation.Validate(reservation.Producer.ID); err != nil {
		return result, true, err
	}
	oldRequest := r
	oldRequest.Desired, oldRequest.DesiredHash = before, prior.Pins.DesiredHash
	if err = verifyReservedConsumerInput(oldRequest, paths, d, prior, input, reservation); err != nil {
		return result, true, err
	}
	var beforeInputs, afterInputs *notificationInputPins
	compiled, err := compileReservedNotification(retainedLEDDeclaration(r.Desired, input), r.Facts, paths, input, reservation)
	if kind == "providers" {
		compiled, beforeInputs, afterInputs, err = compileSharedProviders(r, paths, d, prior, input, raw, reservation)
	}
	if err != nil {
		return result, true, err
	}
	preserveRuntimeRouteLabels(input, &compiled.Input)
	after, err := compiled.Input.Bytes()
	if err != nil {
		return result, true, err
	}
	version := sharedPolicyVersion(kind)
	if kind == "report-route" && string(raw) == string(after) {
		version = 13
	}
	if (kind == "freshness" || kind == "producer") && string(raw) == string(after) {
		version = 3
		if kind == "producer" {
			version = 6
		}
	}
	if l.nativeResume != nil {
		nativeRequest := r
		if !retry {
			nativeRequest.Desired, nativeRequest.DesiredHash = before, prior.Pins.DesiredHash
		}
		if err = l.nativeResume(ctx, nativeRequest, paths, helper); err != nil {
			return result, true, err
		}
	}
	if !retry {
		backupReview := r
		if kind == "derived-freshness" || kind == "report-route" {
			backupReview.Desired = before
		}
		if err = reviewSharedVerificationBackup(ctx, backupReview, prior.Pins.DesiredHash, l); err != nil {
			return result, true, err
		}
		if !current.Complete {
			return result, true, fmt.Errorf("finish pending limit update before changing YAML")
		}
		o, e := l.inspect(ctx, paths.Installation)
		if e != nil {
			return result, true, e
		}
		if o.Phase != "complete" || o.Unbound || protocol.Hash(o.Manifest) != prior.Children[0].Receipt {
			return result, true, fmt.Errorf("gateway changed before limit review")
		}
		if enrolledOutputJournal(prior) {
			e = verifyEnrolledOutputReceipt(d, prior, o.Manifest, l.scopeInspect)
		} else if sharedLimitsJournal(prior) {
			e = verifyRetainedSignalAdoption(d)
			if e == nil {
				e = verifySharedLimitsReceipt(d, prior, o.Manifest, l.scopeInspect)
			}
		} else if mappingJournal(prior) {
			e = verifyRetainedSignalAdoption(d)
			if e == nil {
				e = verifyMappingReceiptWithScope(d, prior, o.Manifest, l.scopeInspect)
			}
		} else {
			e = verifyReleaseReceipt(d, prior, o.Manifest)
		}
		if e != nil {
			return result, true, e
		}
		if e = validateSharedLimitsIntent(notificationSharedLimitsIntent{version, current.ID, r.DesiredHash, o.Manifest, raw, after, beforeInputs, afterInputs}); e != nil {
			return result, true, e
		}
		b, e := input.Resolve()
		if e != nil {
			return result, true, e
		}
		a, e := compiled.Input.Resolve()
		if e != nil {
			return result, true, e
		}
		preflight := worker.PreflightEnrolledLimits
		if kind == "command-activity" {
			preflight = worker.PreflightEnrolledCommandActivity
		}
		if kind == "command-policy" {
			preflight = worker.PreflightEnrolledCommandPolicy
		}
		if kind == "commands" {
			preflight = worker.PreflightEnrolledCommands
		}
		if kind == "providers" {
			preflight = worker.PreflightEnrolledProviders
		}
		if kind == "producer" || kind == "report-route" {
			preflight = worker.PreflightEnrolledProducerPolicy
		}
		if kind == "routes" {
			preflight = worker.PreflightEnrolledRoutes
		}
		if kind == "freshness" || kind == "derived-freshness" {
			preflight = worker.PreflightEnrolledFreshness
		}
		if !sharedPolicyNoop(version) {
			if e = preflight(ctx, paths.State, o.Manifest.Identity, b.Worker, a.Worker); e != nil {
				return result, true, e
			}
		}
	}
	pins := prior.Pins
	pins.PreviousOperation, pins.Previous, pins.DesiredHash = prior.ID, prior.Digest(), r.DesiredHash
	step := &notificationSharedLimitsStep{notificationSharedLimitsDomainStep: notificationSharedLimitsDomainStep{version: version, directory: d, paths: paths, helper: helper, helperHash: r.HelperSHA256, desiredHash: r.DesiredHash, expected: prior.Children[0].Receipt, before: raw, after: after, beforeInputs: beforeInputs, afterInputs: afterInputs, lifecycle: l.notificationMappingLifecycle}, request: r, priorScope: prior.Pins.DesiredHash, reservation: reservation, lifecycle: l}
	if err = retainDeclaration(d, r.Desired, r.DesiredHash, r.DesiredScope); err != nil {
		return result, true, err
	}
	completed, err := (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step})
	if err != nil {
		return result, true, err
	}
	result.State, result.Code, result.Identity = "verification", "ok", &step.manifest.Identity
	result.Next = "Shared policy updated with the retained backup producer; qualify the revised configuration before enabling reporting"
	// Return to ordinary admission only after both policy publications completed.
	returnResult, err := advanceNotificationDelivery(ctx, r, paths, d, completed, result, l.delivery)
	return returnResult, true, err
}

type notificationSharedLimitsStep struct {
	notificationSharedLimitsDomainStep
	request     ControlRequest
	priorScope  string
	reservation backup.ProducerReservation
	lifecycle   notificationRemappingLifecycle
}

func (s *notificationSharedLimitsStep) Name() string {
	if s.version == 12 || s.version == 13 {
		return "notification-shared-report-route"
	}
	if s.version == 11 {
		return "notification-shared-derived-freshness"
	}
	if s.version == 10 {
		return "notification-shared-command-activity"
	}
	if s.version == 9 {
		return "notification-shared-command-policy"
	}
	if s.version == 8 {
		return "notification-shared-commands"
	}
	if s.version == 7 {
		return "notification-shared-providers"
	}
	if s.version == 5 || s.version == 6 {
		return "notification-shared-producer"
	}
	if s.version == 4 {
		return "notification-shared-routes"
	}
	if s.version >= 2 {
		return "notification-shared-freshness"
	}
	return "notification-shared-limits"
}
func (s *notificationSharedLimitsStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	receipt, complete, err := s.notificationSharedLimitsDomainStep.Inspect(ctx, op)
	if err != nil || !complete {
		return receipt, complete, err
	}
	confirmed, done, err := s.lifecycle.scopeInspect(s.request.Facts.Home, op, s.expected, s.request.DesiredHash)
	if err != nil || !done {
		return "", false, err
	}
	if confirmed != receipt {
		return "", false, fmt.Errorf("backup and gateway limits completion receipts differ")
	}
	return receipt, true, nil
}
func (s *notificationSharedLimitsStep) Run(ctx context.Context, op string) error {
	if err := retainDeclaration(s.directory, s.request.Desired, s.request.DesiredHash, s.request.DesiredScope); err != nil {
		return err
	}
	_, err := s.lifecycle.scope(ctx, s.request.Facts.Home, backup.VerificationNotificationScopeChange{Operation: op, BeforeScope: s.priorScope, BeforeGateway: s.expected, Desired: s.request.Desired}, func(reservation backup.ProducerReservation) (string, error) {
		if reservation != s.reservation {
			return "", fmt.Errorf("shared limits backup reservation changed")
		}
		if receipt, complete, err := s.notificationSharedLimitsDomainStep.Inspect(ctx, op); err != nil || complete {
			return receipt, err
		}
		if err := s.notificationSharedLimitsDomainStep.Run(ctx, op); err != nil {
			return "", err
		}
		receipt, complete, err := s.notificationSharedLimitsDomainStep.Inspect(ctx, op)
		if err != nil {
			return "", err
		}
		if !complete {
			return "", fmt.Errorf("shared limits domain completion unavailable")
		}
		return receipt, nil
	})
	return err
}

// Validate the retained gateway transition before resuming its independently
// stopped native service. This runs before taking the Signal coordinator lock.
func verifyPendingSharedLimitsSignal(r ControlRequest, paths ResolvedPaths, d *safefs.Directory, current OperationJournal) error {
	prior, err := sharedLimitsPredecessor(d, current)
	if err != nil {
		return err
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
	if err != nil {
		return err
	}
	if sharedPolicyKind(current) == "providers" {
		raw, e := retainedEnrolledRuntime(d, paths, prior)
		if e != nil {
			return e
		}
		var input install.RuntimeInput
		if e = safefs.DecodeJSON(raw, &input, maxControlFrame); e != nil {
			return e
		}
		r, e = retainedProviderReviewRequest(r, before, input)
		if e != nil {
			return e
		}
	}
	if err = verifyLEDOutputInputs(r, paths, d, before); err != nil {
		return err
	}
	var intent notificationSharedLimitsIntent
	err = d.ReadJSON("shared-limits-"+current.Children[0].Operation+".json", &intent)
	// The outer journal precedes the mapping intent. At that boundary no domain
	// operation has been authorized yet; the exact predecessor pins still apply.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = validateSharedLimitsIntent(intent); err != nil {
		return err
	}
	if intent.Operation != current.Children[0].Operation || intent.DesiredHash != current.Pins.DesiredHash || protocol.Hash(intent.Before) != prior.Children[0].Receipt || intent.Before.BinaryHash != r.HelperSHA256 {
		return fmt.Errorf("limit update authority changed before native resume")
	}
	return nil
}

type notificationSharedLimitsIntent struct {
	Version       int                     `json:"version"`
	Operation     string                  `json:"operation"`
	DesiredHash   string                  `json:"desired_hash"`
	Before        install.ManagedManifest `json:"before"`
	BeforeRuntime []byte                  `json:"before_runtime"`
	AfterRuntime  []byte                  `json:"after_runtime"`
	BeforeInputs  *notificationInputPins  `json:"before_inputs,omitempty"`
	AfterInputs   *notificationInputPins  `json:"after_inputs,omitempty"`
}

func validateSharedLimitsIntent(v notificationSharedLimitsIntent) error {
	if (v.Version < 1 || v.Version > 13) || !safefs.ValidID(v.Operation, "n") || !validDigest(v.DesiredHash) || v.Before.RuntimeHash != inputDigest(v.BeforeRuntime) {
		return fmt.Errorf("shared limits input authority changed")
	}
	if err := validateSharedProviderPins(v); err != nil {
		return err
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
	if protocol.Hash(b) != v.Before.ConfigHash {
		return fmt.Errorf("shared limits source runtime changed")
	}
	if sharedPolicyNoop(v.Version) && string(v.BeforeRuntime) != string(v.AfterRuntime) || !sharedPolicyNoop(v.Version) && protocol.Hash(b) == protocol.Hash(a) {
		return fmt.Errorf("shared policy effective change differs from its intent version")
	}
	if v.Version == 12 || v.Version == 13 {
		return install.ValidateManagedEnrolledProducerPolicy(v.Before, b, a)
	}
	if v.Version == 11 {
		return install.ValidateManagedEnrolledFreshness(v.Before, b, a)
	}
	if v.Version == 10 {
		return install.ValidateManagedEnrolledCommandActivity(v.Before, b, a)
	}
	if v.Version == 9 {
		return install.ValidateManagedEnrolledCommandPolicy(v.Before, b, a)
	}
	if v.Version == 8 {
		return install.ValidateManagedEnrolledCommands(v.Before, b, a)
	}
	if v.Version == 7 {
		return install.ValidateManagedEnrolledProviders(v.Before, b, a)
	}
	if v.Version == 5 || v.Version == 6 {
		return install.ValidateManagedEnrolledProducerPolicy(v.Before, b, a)
	}
	if v.Version == 4 {
		return install.ValidateManagedEnrolledRoutes(v.Before, b, a)
	}
	if v.Version >= 2 {
		return install.ValidateManagedEnrolledFreshness(v.Before, b, a)
	}
	return install.ValidateManagedEnrolledLimits(v.Before, b, a)
}

func validateSharedLimitsManifest(v notificationSharedLimitsIntent, m install.ManagedManifest) error {
	if sharedPolicyNoop(v.Version) {
		if protocol.Hash(m) != protocol.Hash(v.Before) {
			return fmt.Errorf("unchanged effective policy changed the gateway")
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
	if v.Version == 7 && input.Providers["signal"].SignalDelivery != nil && protocol.Hash(m.ReportCredentialHistory) != protocol.Hash(v.Before.ReportCredentialHistory) {
		if err = install.ValidateStagedProviderCredentialHistory(v.Before, m, "enrolled-providers"); err != nil {
			return err
		}
		want.ReportCredentialHistory = m.ReportCredentialHistory
	}
	if m.Revision == v.Before.Revision || protocol.Hash(want) != protocol.Hash(m) {
		return fmt.Errorf("shared limits receipt changed installation authority")
	}
	return nil
}

type notificationSharedLimitsDomainStep struct {
	version                                   int
	directory                                 *safefs.Directory
	paths                                     ResolvedPaths
	helper, helperHash, desiredHash, expected string
	before, after                             []byte
	beforeInputs, afterInputs                 *notificationInputPins
	lifecycle                                 notificationMappingLifecycle
	manifest                                  install.ManagedManifest
}

func (s *notificationSharedLimitsDomainStep) Name() string {
	return "notification-shared-limits-domain"
}
func (s *notificationSharedLimitsDomainStep) runtimePath(op string) string {
	return filepath.Join(s.paths.Control, "shared-limits-"+op+"-runtime.json")
}
func (s *notificationSharedLimitsDomainStep) boundary(at string) error {
	if s.lifecycle.boundary != nil {
		return s.lifecycle.boundary(at)
	}
	return nil
}
func (s *notificationSharedLimitsDomainStep) intent(op string) (notificationSharedLimitsIntent, error) {
	var v notificationSharedLimitsIntent
	if err := s.directory.ReadJSON("shared-limits-"+op+".json", &v); err != nil {
		return v, err
	}
	if err := validateSharedLimitsIntent(v); err != nil {
		return v, err
	}
	if v.Version != s.version || v.Operation != op || v.DesiredHash != s.desiredHash || protocol.Hash(v.Before) != s.expected || v.Before.BinaryHash != s.helperHash || string(v.BeforeRuntime) != string(s.before) || string(v.AfterRuntime) != string(s.after) || protocol.Hash(v.BeforeInputs) != protocol.Hash(s.beforeInputs) || protocol.Hash(v.AfterInputs) != protocol.Hash(s.afterInputs) {
		return v, fmt.Errorf("retained shared limits inputs changed")
	}
	return v, nil
}

func (s *notificationSharedLimitsDomainStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	o, err := s.lifecycle.inspect(ctx, s.paths.Installation)
	if err != nil {
		return "", false, err
	}
	v, e := s.intent(op)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return "", false, e
	}
	if e == nil && sharedPolicyNoop(v.Version) && o.Phase == "complete" && protocol.Hash(o.Manifest) == s.expected && !o.Unbound {
		raw, err := readPrivateGeneratedInput(s.runtimePath(op), len(v.AfterRuntime))
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		if err != nil || string(raw) != string(v.AfterRuntime) {
			return "", false, fmt.Errorf("unchanged shared policy runtime differs")
		}
		s.manifest = o.Manifest
		return protocol.Hash(o.Manifest), true, nil
	}
	if o.Phase == "complete" && protocol.Hash(o.Manifest) == s.expected && !o.Unbound {
		return "", false, nil
	}
	if e != nil {
		return "", false, fmt.Errorf("shared limits changed without retained intent")
	}
	if o.Phase != "complete" {
		return "", false, nil
	}
	if o.Unbound {
		return "", false, fmt.Errorf("shared limits lost producer enrollment")
	}
	if err = validateSharedLimitsManifest(v, o.Manifest); err != nil {
		return "", false, err
	}
	raw, err := readPrivateGeneratedInput(s.runtimePath(op), len(v.AfterRuntime))
	if err != nil || string(raw) != string(v.AfterRuntime) {
		return "", false, fmt.Errorf("generated shared limits input changed")
	}
	s.manifest = o.Manifest
	return protocol.Hash(o.Manifest), true, nil
}

func (s *notificationSharedLimitsDomainStep) Run(ctx context.Context, op string) error {
	v, err := s.intent(op)
	if errors.Is(err, os.ErrNotExist) {
		o, e := s.lifecycle.inspect(ctx, s.paths.Installation)
		if e != nil {
			return e
		}
		if o.Phase != "complete" || o.Unbound || protocol.Hash(o.Manifest) != s.expected {
			return fmt.Errorf("gateway changed before shared limits")
		}
		v = notificationSharedLimitsIntent{s.version, op, s.desiredHash, o.Manifest, s.before, s.after, s.beforeInputs, s.afterInputs}
		if err = validateSharedLimitsIntent(v); err != nil {
			return err
		}
		err = s.directory.WriteJSON("shared-limits-"+op+".json", v, false)
	}
	if err != nil {
		return err
	}
	if err = s.boundary("shared-limits-intent"); err != nil {
		return err
	}
	if err = publishPrivateInput(s.runtimePath(op), v.AfterRuntime); err != nil {
		return err
	}
	if err = s.boundary("shared-limits-input-published"); err != nil {
		return err
	}
	if sharedPolicyNoop(v.Version) {
		return s.boundary("shared-limits-domain-complete")
	}
	spec := install.ManagedSpec{Version: 1, Root: s.paths.Installation, State: s.paths.State, UnitDirectory: s.paths.UnitDirectory, RuntimeFile: s.runtimePath(op), BinaryFile: s.helper, BinarySHA256: s.helperHash, DeniedPaths: s.paths.DeniedPaths, FreshSignal: v.Before.FreshSignal}
	if _, err = s.lifecycle.migrate(ctx, spec, s.expected); err != nil {
		return err
	}
	return s.boundary("shared-limits-domain-complete")
}

// The bootstrap input remains immutable after enrollment. Ordinary continuation
// compiles today's YAML, then projects only the budgets covered by exact shared
// limit receipts back to that retained input before checking credential pins.
// Consumer verification independently compiles and checks today's full policy.
func retainBootstrapLimitsInput(d *safefs.Directory, current OperationJournal, input *install.RuntimeInput) error {
	for depth := 0; depth < 128; depth++ {
		if deliveryProviderJournal(current) {
			v, err := readDeliveryProviderIntent(d, current)
			if err != nil {
				return err
			}
			var before, after install.RuntimeInput
			if err = safefs.DecodeJSON(v.BeforeRuntime, &before, maxControlFrame); err != nil {
				return err
			}
			if err = safefs.DecodeJSON(v.AfterRuntime, &after, maxControlFrame); err != nil {
				return err
			}
			if protocol.Hash(input.Providers) != protocol.Hash(after.Providers) {
				return fmt.Errorf("compiled credential references differ from retained rotation")
			}
			input.Providers = before.Providers
			if v.Kind == "routes" || v.Kind == "destination" || v.Kind == "provider-enrollment" {
				preserveRuntimeRouteLabels(after, input)
				if protocol.Hash(input.Worker.Routes) != protocol.Hash(after.Worker.Routes) {
					return fmt.Errorf("compiled routes differ from retained production policy")
				}
				input.Worker.Routes = before.Worker.Routes
			}
			if v.Kind == "limits" {
				if input.Worker.MaxMessages != after.Worker.MaxMessages || input.Worker.MaxPayloadBytes != after.Worker.MaxPayloadBytes || input.Worker.MaxDatabaseBytes != after.Worker.MaxDatabaseBytes || input.Worker.MinFreeBytes != after.Worker.MinFreeBytes || input.Worker.ReceiptAuditDays != after.Worker.ReceiptAuditDays {
					return fmt.Errorf("compiled budgets differ from retained production policy")
				}
				input.Worker.MaxMessages, input.Worker.MaxPayloadBytes = before.Worker.MaxMessages, before.Worker.MaxPayloadBytes
				input.Worker.MaxDatabaseBytes, input.Worker.MinFreeBytes = before.Worker.MaxDatabaseBytes, before.Worker.MinFreeBytes
				input.Worker.ReceiptAuditDays = before.Worker.ReceiptAuditDays
			}
			current, err = deliveryProviderPredecessor(d, current)
			if err != nil {
				return err
			}
			continue
		}
		if requalificationJournal(current) {
			// Disabled output is separate from the installed LED endpoint. A
			// renewed delivery still verifies the original bootstrap bytes.
			_, pause, err := requalificationPredecessor(d, current)
			if err != nil {
				return err
			}
			if pause.Before.LEDOutput != nil && pause.Before.LEDOutput.Disabled && input.LEDSocket == "" {
				var pins notificationInputPins
				if err = d.ReadJSON("notification-inputs.json", &pins); err != nil {
					return err
				}
				raw, e := readLimitsRuntime(current.Pins.Installations["runtime"])
				if e != nil {
					return e
				}
				if inputDigest(raw) != pins.Runtime {
					return fmt.Errorf("renewal lost retained LED endpoint inputs")
				}
				var retained install.RuntimeInput
				if err = safefs.DecodeJSON(raw, &retained, maxControlFrame); err != nil {
					return err
				}
				input.LEDSocket, input.LEDEndpoint = retained.LEDSocket, retained.LEDEndpoint
			}
			current, err = requalificationEnrollment(d, current)
			if err != nil {
				return err
			}
			continue
		}
		if enrolledOutputJournal(current) {
			if _, err := readEnrolledOutputIntent(d, current); err != nil {
				return err
			}
			prior, err := enrolledOutputPredecessor(d, current)
			if err != nil {
				return err
			}
			current = prior
			continue
		}
		if mappingJournal(current) {
			if _, err := readMappingIntent(d, current); err != nil {
				return err
			}
			prior, err := mappingPredecessor(d, current)
			if err != nil {
				return err
			}
			current = prior
			continue
		}
		if commandJournal(current) {
			prior, err := commandPredecessor(d, current)
			if err != nil {
				return err
			}
			current = prior
			continue
		}
		if len(current.Children) == 1 && current.Children[0].Name == "notification-delivery" {
			var prior OperationJournal
			if err := d.ReadJSON("history/"+current.Pins.PreviousOperation+".json", &prior); err != nil {
				return err
			}
			if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != current.Pins.PreviousOperation || prior.Digest() != current.Pins.Previous || !enrolledNotificationJournal(prior) || prior.Pins.DesiredHash != current.Pins.DesiredHash {
				return fmt.Errorf("delivery lost its limit policy predecessor")
			}
			current = prior
			continue
		}
		if !sharedLimitsJournal(current) {
			return nil
		}
		v, err := readSharedLimitsIntent(d, current)
		if err != nil {
			return err
		}
		var before, after install.RuntimeInput
		if err = safefs.DecodeJSON(v.BeforeRuntime, &before, maxControlFrame); err != nil {
			return err
		}
		if err = safefs.DecodeJSON(v.AfterRuntime, &after, maxControlFrame); err != nil {
			return err
		}
		if v.Version == 7 {
			if protocol.Hash(input.Providers) != protocol.Hash(after.Providers) {
				return fmt.Errorf("compiled provider selection differs from retained migration")
			}
			input.Providers = before.Providers
		}
		if v.Version == 4 || v.Version == 7 {
			preserveRuntimeRouteLabels(after, input)
			if protocol.Hash(input.Worker.Routes) != protocol.Hash(after.Worker.Routes) {
				return fmt.Errorf("compiled routes differ from retained shared migration")
			}
			input.Worker.Routes = before.Worker.Routes
		}
		budget := func(c worker.Config) [5]int64 {
			return [5]int64{int64(c.MaxMessages), c.MaxPayloadBytes, c.MaxDatabaseBytes, c.MinFreeBytes, int64(c.ReceiptAuditDays)}
		}
		if budget(input.Worker) != budget(after.Worker) {
			return fmt.Errorf("compiled limits differ from retained shared migration")
		}
		input.Worker.MaxMessages, input.Worker.MaxPayloadBytes = before.Worker.MaxMessages, before.Worker.MaxPayloadBytes
		input.Worker.MaxDatabaseBytes, input.Worker.MinFreeBytes = before.Worker.MaxDatabaseBytes, before.Worker.MinFreeBytes
		input.Worker.ReceiptAuditDays = before.Worker.ReceiptAuditDays
		current, err = sharedLimitsPredecessor(d, current)
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("shared limits continuation history exceeds review bound")
}

func sharedPolicyNoop(version int) bool { return version == 3 || version == 6 || version == 13 }
func reconcileSharedProducerPolicy(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
	return reconcileSharedPolicy(ctx, r, paths, helper, l, "producer")
}
func defaultSharedProducerPolicy() notificationRemappingLifecycle {
	l := defaultSharedLimits()
	l.migrate = install.MigrateEnrolledProducerPolicy
	l.scope = backup.ReconcileVerificationProducerPolicyScope
	return l
}

func defaultSharedDerivedFreshness() notificationRemappingLifecycle {
	l := defaultSharedFreshness()
	l.scope = backup.ReconcileVerificationDerivedFreshnessScope
	return l
}

func defaultSharedReportRoute() notificationRemappingLifecycle {
	l := defaultSharedLimits()
	l.migrate = install.MigrateEnrolledProducerPolicy
	l.scope = backup.ReconcileVerificationReportRouteScope
	return l
}
