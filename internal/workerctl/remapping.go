package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

type notificationRemappingLifecycle struct {
	scopeReview func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error)
	notificationMappingLifecycle
	nativeResume func(context.Context, ControlRequest, ResolvedPaths, string) error
	delivery     notificationDeliveryLifecycle
	scope        func(context.Context, string, backup.VerificationNotificationScopeChange, func(backup.ProducerReservation) (string, error)) (string, error)
	scopeInspect func(string, string, string, string) (string, bool, error)
}

func defaultNotificationRemapping() notificationRemappingLifecycle {
	return notificationRemappingLifecycle{
		scopeReview: backup.InspectSetupPolicy, notificationMappingLifecycle: notificationMappingLifecycle{inspect: install.InspectInstallation, migrate: install.MigrateVerificationLEDBindings},
		nativeResume: func(ctx context.Context, r ControlRequest, p ResolvedPaths, helper string) error {
			return resumeCompletedSignalChild(ctx, r, p, helper, os.Getenv("DEPLOY_CONTROL_SIGNAL_ARTIFACT"), defaultSignalSetupLifecycle())
		},
		delivery: defaultNotificationDelivery(), scope: backup.ReconcileVerificationNotificationScope, scopeInspect: backup.InspectVerificationNotificationScope,
	}
}

func remappingJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-remapping"
}

func validateRemappingPolicy(before, after config.DesiredWorkers, oldScope string) error {
	if before.Notification == nil || after.Notification == nil || !before.Notification.StatusLEDs.Enabled || !after.Notification.StatusLEDs.Enabled || reflect.DeepEqual(before.Notification.StatusLEDs.Bindings, after.Notification.StatusLEDs.Bindings) {
		return fmt.Errorf("mapping transition requires enabled LEDs and changed bindings")
	}
	next := after
	n := *after.Notification
	next.Notification = &n
	n.StatusLEDs.Bindings = before.Notification.StatusLEDs.Bindings
	hash, err := next.NotificationFingerprint()
	if err != nil || hash != oldScope {
		return fmt.Errorf("change LED bindings separately from gateway, producer and provider policy")
	}
	return nil
}

// Mapping revisions use the same domain migration as initial mappings, plus a
// backup scope transaction. Both receipts must complete before qualification or
// delivery can consume the new YAML policy.
func reconcileDeclaredLEDMappings(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
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
	retry := remappingJournal(current) && current.Pins.DesiredHash == r.DesiredHash
	if !retry && current.Pins.DesiredHash == r.DesiredHash {
		return result, false, nil
	}
	prior := current
	if retry {
		prior, err = mappingPredecessor(d, current)
		if err != nil {
			return result, true, err
		}
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
	if err != nil {
		return result, true, err
	}
	if !retry && (before.Notification == nil || r.Desired.Notification == nil || reflect.DeepEqual(before.Notification.StatusLEDs.Bindings, r.Desired.Notification.StatusLEDs.Bindings)) {
		return result, false, nil
	}
	if !prior.Complete || !enrolledNotificationJournal(prior) || prior.Pins.DesiredScope != 1 || r.DesiredScope != 1 || prior.Pins.UID != r.Facts.UID || prior.Pins.Home != r.Facts.Home || prior.Pins.Worker != r.Worker || prior.Pins.Artifacts["helper"] != r.HelperSHA256 || prior.Pins.Installations["notification-worker"] != paths.Installation || prior.Pins.Installations["state"] != paths.State || prior.Pins.Installations["runtime"] != paths.Runtime {
		return result, true, fmt.Errorf("later LED mappings require unchanged enrolled verification authority; production mappings require a separate migration")
	}
	if err = validateRemappingPolicy(before, r.Desired, prior.Pins.DesiredHash); err != nil {
		return result, true, err
	}
	if err = verifyLEDOutputInputs(r, paths, d, before); err != nil {
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
	compiled, err := compileReservedNotification(r.Desired, r.Facts, paths, input, reservation)
	if err != nil {
		return result, true, err
	}
	preserveRuntimeRouteLabels(input, &compiled.Input)
	after, err := compiled.Input.Bytes()
	if err != nil {
		return result, true, err
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
		if err = reviewSharedVerificationBackup(ctx, r, prior.Pins.DesiredHash, l); err != nil {
			return result, true, err
		}
		if !current.Complete {
			return result, true, fmt.Errorf("finish pending mapping before changing YAML")
		}
		o, e := l.inspect(ctx, paths.Installation)
		if e != nil {
			return result, true, e
		}
		if o.Phase != "complete" || o.Unbound || protocol.Hash(o.Manifest) != prior.Children[0].Receipt {
			return result, true, fmt.Errorf("gateway changed before mapping review")
		}
		if enrolledOutputJournal(prior) {
			e = verifyEnrolledOutputReceipt(d, prior, o.Manifest, l.scopeInspect)
		} else if sharedLimitsJournal(prior) {
			e = verifyRetainedSignalAdoption(d)
			if e == nil {
				e = verifySharedLimitsReceipt(d, prior, o.Manifest, l.scopeInspect)
			}
		} else if remappingJournal(prior) {
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
		if e = validateMappingIntent(notificationMappingIntent{1, current.ID, r.DesiredHash, o.Manifest, raw, after}); e != nil {
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
		if e = worker.PreflightManagedLEDBindings(ctx, paths.State, b.Worker, a.Worker, o.Manifest.Identity); e != nil {
			return result, true, e
		}
	}
	pins := prior.Pins
	pins.PreviousOperation, pins.Previous, pins.DesiredHash = prior.ID, prior.Digest(), r.DesiredHash
	step := &notificationRemappingStep{notificationMappingStep: notificationMappingStep{directory: d, paths: paths, helper: helper, helperHash: r.HelperSHA256, desiredHash: r.DesiredHash, expected: prior.Children[0].Receipt, before: raw, after: after, lifecycle: l.notificationMappingLifecycle}, request: r, priorScope: prior.Pins.DesiredHash, reservation: reservation, lifecycle: l}
	if err = retainDeclaration(d, r.Desired, r.DesiredHash, r.DesiredScope); err != nil {
		return result, true, err
	}
	completed, err := (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step})
	if err != nil {
		return result, true, err
	}
	result.State, result.Code, result.Identity = "verification", "ok", &step.manifest.Identity
	result.Next = "LED bindings updated with the retained backup producer; complete actual qualification before enabling production reporting"
	// Return to ordinary admission only after both policy publications completed.
	returnResult, err := advanceNotificationDelivery(ctx, r, paths, d, completed, result, l.delivery)
	return returnResult, true, err
}

type notificationRemappingStep struct {
	notificationMappingStep
	request     ControlRequest
	priorScope  string
	reservation backup.ProducerReservation
	lifecycle   notificationRemappingLifecycle
}

func (s *notificationRemappingStep) Name() string { return "notification-remapping" }
func (s *notificationRemappingStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	receipt, complete, err := s.notificationMappingStep.Inspect(ctx, op)
	if err != nil || !complete {
		return receipt, complete, err
	}
	confirmed, done, err := s.lifecycle.scopeInspect(s.request.Facts.Home, op, s.expected, s.request.DesiredHash)
	if err != nil || !done {
		return "", false, err
	}
	if confirmed != receipt {
		return "", false, fmt.Errorf("backup and gateway mapping completion receipts differ")
	}
	return receipt, true, nil
}
func (s *notificationRemappingStep) Run(ctx context.Context, op string) error {
	if err := retainDeclaration(s.directory, s.request.Desired, s.request.DesiredHash, s.request.DesiredScope); err != nil {
		return err
	}
	_, err := s.lifecycle.scope(ctx, s.request.Facts.Home, backup.VerificationNotificationScopeChange{Operation: op, BeforeScope: s.priorScope, BeforeGateway: s.expected, Desired: s.request.Desired}, func(reservation backup.ProducerReservation) (string, error) {
		if reservation != s.reservation {
			return "", fmt.Errorf("LED mapping backup reservation changed")
		}
		if receipt, complete, err := s.notificationMappingStep.Inspect(ctx, op); err != nil || complete {
			return receipt, err
		}
		if err := s.notificationMappingStep.Run(ctx, op); err != nil {
			return "", err
		}
		receipt, complete, err := s.notificationMappingStep.Inspect(ctx, op)
		if err != nil {
			return "", err
		}
		if !complete {
			return "", fmt.Errorf("LED mapping domain completion unavailable")
		}
		return receipt, nil
	})
	return err
}

// Validate the retained gateway transition before resuming its independently
// stopped native service. This runs before taking the Signal coordinator lock.
func verifyPendingMappingSignal(r ControlRequest, paths ResolvedPaths, d *safefs.Directory, current OperationJournal) error {
	prior, err := mappingPredecessor(d, current)
	if err != nil {
		return err
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
	if err != nil {
		return err
	}
	if err = verifyLEDOutputInputs(r, paths, d, before); err != nil {
		return err
	}
	var intent notificationMappingIntent
	err = d.ReadJSON("mapping-"+current.Children[0].Operation+".json", &intent)
	// The outer journal precedes the mapping intent. At that boundary no domain
	// operation has been authorized yet; the exact predecessor pins still apply.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = validateMappingIntent(intent); err != nil {
		return err
	}
	if intent.Operation != current.Children[0].Operation || intent.DesiredHash != current.Pins.DesiredHash || protocol.Hash(intent.Before) != prior.Children[0].Receipt || intent.Before.BinaryHash != r.HelperSHA256 {
		return fmt.Errorf("mapping authority changed before native resume")
	}
	return nil
}
