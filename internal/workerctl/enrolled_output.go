package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func enrolledOutputJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-enrolled-led-output"
}

func validateEnrolledOutputPolicy(before, after config.DesiredWorkers, oldScope string) error {
	if before.Notification == nil || after.Notification == nil || before.Notification.StatusLEDs.Enabled == after.Notification.StatusLEDs.Enabled {
		return fmt.Errorf("LED output transition requires changed permission")
	}
	next := after
	n := *after.Notification
	next.Notification = &n
	n.StatusLEDs.Enabled = before.Notification.StatusLEDs.Enabled
	hash, err := next.NotificationFingerprint()
	if err != nil || hash != oldScope {
		return fmt.Errorf("change LED output permission separately from other policy")
	}
	return nil
}

func enrolledOutputPredecessor(d *safefs.Directory, current OperationJournal) (OperationJournal, error) {
	var prior OperationJournal
	if validateOperationJournal(current) != nil || !enrolledOutputJournal(current) {
		return prior, fmt.Errorf("invalid enrolled LED output journal")
	}
	if err := d.ReadJSON("history/"+current.Pins.PreviousOperation+".json", &prior); err != nil {
		return prior, err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || !enrolledNotificationJournal(prior) || prior.ID != current.Pins.PreviousOperation || prior.Digest() != current.Pins.Previous || prior.Pins.DesiredScope != 1 || current.Pins.DesiredScope != 1 {
		return prior, fmt.Errorf("enrolled LED output lost predecessor")
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, 1)
	if err != nil {
		return prior, err
	}
	after, err := readDeclaration(d, current.Pins.DesiredHash, 1)
	if err != nil {
		return prior, err
	}
	if err = validateEnrolledOutputPolicy(before, after, prior.Pins.DesiredHash); err != nil {
		return prior, err
	}
	want := prior.Pins
	want.PreviousOperation, want.Previous, want.DesiredHash = prior.ID, prior.Digest(), current.Pins.DesiredHash
	if !reflect.DeepEqual(want, current.Pins) {
		return prior, fmt.Errorf("enrolled LED output changed target authority")
	}
	return prior, nil
}

func readEnrolledOutputIntent(d *safefs.Directory, current OperationJournal) (notificationLEDIntent, error) {
	var v notificationLEDIntent
	prior, err := enrolledOutputPredecessor(d, current)
	if err != nil {
		return v, err
	}
	if err = d.ReadJSON("led-output-"+current.Children[0].Operation+".json", &v); err != nil {
		return v, err
	}
	desired, err := readDeclaration(d, current.Pins.DesiredHash, 1)
	if err != nil {
		return v, err
	}
	if v.Version != 1 || v.Operation != current.Children[0].Operation || v.DesiredHash != current.Pins.DesiredHash || protocol.Hash(v.Before) != prior.Children[0].Receipt || v.Before.BinaryHash != current.Pins.Artifacts["helper"] || v.Before.Mode != "verification" || v.Before.DeliveryOwnerHash != "" || v.Before.RecoveryCommitHash != "" || v.Disabled == desired.Notification.StatusLEDs.Enabled {
		return v, fmt.Errorf("enrolled LED output intent changed")
	}
	return v, nil
}

func verifyEnrolledOutputReceipt(d *safefs.Directory, j OperationJournal, m install.ManagedManifest, scope func(string, string, string, string) (string, bool, error)) error {
	v, err := readEnrolledOutputIntent(d, j)
	if err != nil {
		return err
	}
	want := v.Before
	want.LEDOutput = m.LEDOutput
	if protocol.Hash(m) != j.Children[0].Receipt || m.LEDOutput == nil || !safefs.ValidID(m.LEDOutput.Operation, "n") || m.LEDOutput.Disabled != v.Disabled || protocol.Hash(want) != protocol.Hash(m) {
		return fmt.Errorf("enrolled LED output receipt changed")
	}
	return verifyMappingScope(d, j, scope)
}

// Output permission leaves the runtime and bindings intact. Find the most recent
// mapping input through these explicit, bounded predecessor records.
func retainedEnrolledRuntime(d *safefs.Directory, paths ResolvedPaths, j OperationJournal) ([]byte, error) {
	if requalificationJournal(j) {
		var err error
		j, err = requalificationEnrollment(d, j)
		if err != nil {
			return nil, err
		}
	}
	for depth := 0; enrolledOutputJournal(j); depth++ {
		if depth >= 128 {
			return nil, fmt.Errorf("enrolled LED history exceeds review bound")
		}
		if _, err := readEnrolledOutputIntent(d, j); err != nil {
			return nil, err
		}
		var err error
		j, err = enrolledOutputPredecessor(d, j)
		if err != nil {
			return nil, err
		}
	}
	if deliveryProviderJournal(j) {
		v, err := readDeliveryProviderIntent(d, j)
		return v.AfterRuntime, err
	}
	if sharedLimitsJournal(j) {
		v, err := readSharedLimitsIntent(d, j)
		return v.AfterRuntime, err
	}
	if mappingJournal(j) {
		v, err := readMappingIntent(d, j)
		return v.AfterRuntime, err
	}
	if !enrolledNotificationJournal(j) {
		return nil, fmt.Errorf("enrolled LED runtime lost enrollment")
	}
	return readLimitsRuntime(filepath.Join(paths.Config, "consumers/runtime.json"))
}

func reconcileEnrolledLEDOutput(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationRemappingLifecycle, output notificationLEDLifecycle) (ControlResult, bool, error) {
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
	retry := enrolledOutputJournal(current) && current.Pins.DesiredHash == r.DesiredHash
	if !retry && (!enrolledNotificationJournal(current) || current.Pins.DesiredHash == r.DesiredHash) {
		return result, false, nil
	}
	prior := current
	if retry {
		prior, err = enrolledOutputPredecessor(d, current)
		if err != nil {
			return result, true, err
		}
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
	if err != nil {
		return result, true, err
	}
	if !retry && (before.Notification == nil || r.Desired.Notification == nil || before.Notification.StatusLEDs.Enabled == r.Desired.Notification.StatusLEDs.Enabled) {
		return result, false, nil
	}
	if !prior.Complete || prior.Pins.DesiredScope != 1 || r.DesiredScope != 1 || prior.Pins.UID != r.Facts.UID || prior.Pins.Home != r.Facts.Home || prior.Pins.Worker != r.Worker || prior.Pins.Artifacts["helper"] != r.HelperSHA256 || prior.Pins.Installations["notification-worker"] != paths.Installation || prior.Pins.Installations["state"] != paths.State || prior.Pins.Installations["runtime"] != paths.Runtime {
		return result, true, fmt.Errorf("enrolled LED output requires unchanged verification authority")
	}
	if err = validateEnrolledOutputPolicy(before, r.Desired, prior.Pins.DesiredHash); err != nil {
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
	if input.LEDSocket == "" {
		return result, true, fmt.Errorf("LED output needs the retained daemon; adding one requires setup")
	}
	if l.nativeResume != nil {
		nativeRequest := r
		if !retry {
			nativeRequest = oldRequest
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
			return result, true, fmt.Errorf("finish pending LED policy before changing YAML")
		}
		observed, e := output.inspect(ctx, paths.Installation)
		if e != nil {
			return result, true, e
		}
		if observed.Phase != "complete" || observed.Unbound || observed.Manifest.Mode != "verification" || observed.Manifest.DeliveryOwnerHash != "" || observed.Manifest.RecoveryCommitHash != "" || protocol.Hash(observed.Manifest) != prior.Children[0].Receipt {
			return result, true, fmt.Errorf("LED output requires unchanged enrolled verification authority")
		}
		if e = verifyRetainedSignalAdoption(d); e != nil {
			return result, true, e
		}
		if enrolledOutputJournal(prior) {
			e = verifyEnrolledOutputReceipt(d, prior, observed.Manifest, l.scopeInspect)
		} else if remappingJournal(prior) {
			e = verifyMappingReceiptWithScope(d, prior, observed.Manifest, l.scopeInspect)
		} else {
			e = verifyReleaseReceipt(d, prior, observed.Manifest)
		}
		if e != nil {
			return result, true, e
		}
	}
	pins := prior.Pins
	pins.PreviousOperation, pins.Previous, pins.DesiredHash = prior.ID, prior.Digest(), r.DesiredHash
	step := &enrolledOutputStep{notificationLEDStep: notificationLEDStep{directory: d, paths: paths, expected: prior.Children[0].Receipt, desiredHash: r.DesiredHash, desiredScope: 1, desired: r.Desired, disabled: !r.Desired.Notification.StatusLEDs.Enabled, enrolled: true, lifecycle: output}, request: r, priorScope: prior.Pins.DesiredHash, reservation: reservation, coordination: l}
	if err = retainDeclaration(d, r.Desired, r.DesiredHash, 1); err != nil {
		return result, true, err
	}
	completed, err := (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step})
	if err != nil {
		return result, true, ledOutputPendingError{Server: r.Server, Cause: err}
	}
	result.Code, result.State, result.Identity, result.LEDOutput = "ok", "verification", &step.manifest.Identity, step.manifest.LEDOutput
	result.Next = "LED output permission updated; bindings, producer, unresolved incidents and provider account retained; qualification remains separate"
	result, err = advanceNotificationDelivery(ctx, r, paths, d, completed, result, l.delivery)
	return result, true, err
}

type enrolledOutputStep struct {
	notificationLEDStep
	request      ControlRequest
	priorScope   string
	reservation  backup.ProducerReservation
	coordination notificationRemappingLifecycle
}

func (s *enrolledOutputStep) Name() string { return "notification-enrolled-led-output" }
func (s *enrolledOutputStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	receipt, done, err := s.notificationLEDStep.Inspect(ctx, op)
	if err != nil || !done {
		return receipt, done, err
	}
	confirmed, complete, err := s.coordination.scopeInspect(s.request.Facts.Home, op, s.expected, s.request.DesiredHash)
	if err != nil || !complete {
		return "", false, err
	}
	if confirmed != receipt {
		return "", false, fmt.Errorf("LED output and backup scope receipts differ")
	}
	return receipt, true, nil
}
func (s *enrolledOutputStep) Run(ctx context.Context, op string) error {
	_, err := s.coordination.scope(ctx, s.request.Facts.Home, backup.VerificationNotificationScopeChange{Operation: op, BeforeScope: s.priorScope, BeforeGateway: s.expected, Desired: s.request.Desired}, func(reservation backup.ProducerReservation) (string, error) {
		if reservation != s.reservation {
			return "", fmt.Errorf("LED output backup reservation changed")
		}
		if receipt, done, err := s.notificationLEDStep.Inspect(ctx, op); err != nil || done {
			return receipt, err
		}
		if err := s.notificationLEDStep.Run(ctx, op); err != nil {
			return "", err
		}
		receipt, done, err := s.notificationLEDStep.Inspect(ctx, op)
		if err != nil {
			return "", err
		}
		if !done {
			return "", fmt.Errorf("LED output completion unavailable")
		}
		return receipt, nil
	})
	return err
}

func verifyPendingEnrolledOutputSignal(r ControlRequest, paths ResolvedPaths, d *safefs.Directory, current OperationJournal) error {
	prior, err := enrolledOutputPredecessor(d, current)
	if err != nil {
		return err
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, 1)
	if err != nil {
		return err
	}
	if err = verifyLEDOutputInputs(r, paths, d, before); err != nil {
		return err
	}
	_, err = readEnrolledOutputIntent(d, current)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
