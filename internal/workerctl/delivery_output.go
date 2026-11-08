package workerctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type deliveryOutputLifecycle struct {
	output       notificationLEDLifecycle
	review       func(context.Context, string, string, bool) (install.ManagedManifest, error)
	scope        func(context.Context, string, backup.VerificationNotificationScopeChange, func(backup.ProducerReservation) (string, error)) (string, error)
	scopeInspect func(string, string, string, string) (string, bool, error)
	inputs       func(ControlRequest, ResolvedPaths, *safefs.Directory, config.DesiredWorkers) error
	connect      func(context.Context, string, string, string) error
	commands     commandActivityLifecycle
	nativeResume func(context.Context, ControlRequest, ResolvedPaths) error
}

func defaultDeliveryOutput(helper string) deliveryOutputLifecycle {
	return deliveryOutputLifecycle{output: notificationLEDLifecycle{install.InspectInstallation, install.SetLEDOutput}, review: install.ReviewDeliveryLEDOutput, scope: backup.ReconcileDeliveryLEDOutputScope, scopeInspect: backup.InspectVerificationNotificationScope, inputs: func(r ControlRequest, p ResolvedPaths, d *safefs.Directory, b config.DesiredWorkers) error {
		return verifyLEDOutputInputs(r, p, d, b)
	}, connect: backup.ConnectSharedReporting, commands: defaultCommandActivity(), nativeResume: func(ctx context.Context, r ControlRequest, p ResolvedPaths) error {
		return resumeCompletedSignalChild(ctx, r, p, helper, os.Getenv("DEPLOY_CONTROL_SIGNAL_ARTIFACT"), defaultSignalSetupLifecycle())
	}}
}
func deliveryOutputJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-delivery-led-output"
}
func admittedCommandHistory(j OperationJournal) bool {
	return sharedActivityJournal(j) && j.Pins.Artifacts["command-qualification"] != "" || commandDeclarationJournal(j) || commandDeliveryJournal(j) || commandJournal(j) || commandActivityJournal(j) || deliveryOutputJournal(j) && j.Pins.Artifacts["command-qualification"] != ""
}
func deliveryOutputCandidate(j OperationJournal) bool {
	return admittedCommandHistory(j) || deliveryDeclarationJournal(j) || deliveryOutputJournal(j) || j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-delivery"
}
func validateDeliveryOutputPolicy(before, after config.DesiredWorkers, scope string) error {
	if before.Backup == nil || after.Backup == nil || !reflect.DeepEqual(before.Backup, after.Backup) {
		return fmt.Errorf("change delivery LED output separately from backup policy")
	}
	return validateEnrolledOutputPolicy(before, after, scope)
}
func deliveryOutputPins(prior OperationJournal, desired config.DesiredWorkers, scope string) OperationPins {
	p := prior.Pins
	p.PreviousOperation, p.Previous, p.DesiredHash = prior.ID, prior.Digest(), scope
	p.Artifacts = maps.Clone(p.Artifacts)
	if p.Artifacts["command-backup-policy"] != "" {
		p.Artifacts["command-backup-policy"], _ = desired.Fingerprint()
	}
	return p
}
func deliveryOutputPredecessor(d *safefs.Directory, j OperationJournal) (OperationJournal, error) {
	var prior OperationJournal
	if validateOperationJournal(j) != nil || !deliveryOutputJournal(j) {
		return prior, fmt.Errorf("invalid delivery LED output journal")
	}
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
		return prior, err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || !deliveryOutputCandidate(prior) || prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous || prior.Pins.DesiredScope != 1 || j.Pins.DesiredScope != 1 {
		return prior, fmt.Errorf("delivery LED output predecessor changed")
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, 1)
	if err != nil {
		return prior, err
	}
	after, err := readDeclaration(d, j.Pins.DesiredHash, 1)
	if err != nil {
		return prior, err
	}
	if err = validateDeliveryOutputPolicy(before, after, prior.Pins.DesiredHash); err != nil {
		return prior, err
	}
	if !reflect.DeepEqual(deliveryOutputPins(prior, after, j.Pins.DesiredHash), j.Pins) {
		return prior, fmt.Errorf("delivery LED output changed pinned authority")
	}
	return prior, nil
}
func readDeliveryOutputIntent(d *safefs.Directory, j OperationJournal) (notificationLEDIntent, error) {
	var v notificationLEDIntent
	prior, err := deliveryOutputPredecessor(d, j)
	if err != nil {
		return v, err
	}
	if err = d.ReadJSON("led-output-"+j.Children[0].Operation+".json", &v); err != nil {
		return v, err
	}
	desired, err := readDeclaration(d, j.Pins.DesiredHash, 1)
	if err != nil {
		return v, err
	}
	receipt, ok := canonicalNotificationReceipt(prior)
	if !ok || v.Version != 1 || v.Operation != j.Children[0].Operation || v.DesiredHash != j.Pins.DesiredHash || protocol.Hash(v.Before) != receipt || v.Before.Root != j.Pins.Installations["notification-worker"] || v.Before.BinaryHash != j.Pins.Artifacts["helper"] || v.Before.Mode != "delivery" || v.Before.DeliveryActivation == nil || v.Before.RecoveryCommitHash != "" || v.Disabled == desired.Notification.StatusLEDs.Enabled {
		return v, fmt.Errorf("delivery LED output intent changed")
	}
	return v, nil
}
func verifyDeliveryOutputReceipt(d *safefs.Directory, j OperationJournal, m install.ManagedManifest, inspect func(string, string, string, string) (string, bool, error)) error {
	v, err := readDeliveryOutputIntent(d, j)
	if err != nil {
		return err
	}
	want := v.Before
	want.LEDOutput = m.LEDOutput
	if protocol.Hash(m) != j.Children[0].Receipt || m.LEDOutput == nil || !safefs.ValidID(m.LEDOutput.Operation, "n") || m.LEDOutput.Disabled != v.Disabled || protocol.Hash(want) != protocol.Hash(m) {
		return fmt.Errorf("delivery LED output completion changed")
	}
	receipt, done, err := inspect(j.Pins.Home, j.Children[0].Operation, protocol.Hash(v.Before), j.Pins.DesiredHash)
	if err != nil {
		return err
	}
	if !done || receipt != protocol.Hash(m) {
		return fmt.Errorf("delivery LED output lost backup policy receipt")
	}
	return nil
}
func reconcileDeliveryLEDOutput(ctx context.Context, r ControlRequest, paths ResolvedPaths, l deliveryOutputLifecycle) (ControlResult, bool, error) {
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
	if !deliveryOutputCandidate(current) {
		return result, false, nil
	}
	if validateOperationJournal(current) != nil {
		return result, true, fmt.Errorf("invalid delivery coordinator")
	}
	retry := deliveryOutputJournal(current) && current.Pins.DesiredHash == r.DesiredHash
	if !retry && current.Pins.DesiredHash == r.DesiredHash {
		return result, false, nil
	}
	if !retry && !current.Complete {
		return result, true, fmt.Errorf("finish the retained delivery operation before changing YAML")
	}
	prior := current
	if retry {
		prior, err = deliveryOutputPredecessor(d, current)
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
	if !prior.Complete || r.DesiredScope != 1 || prior.Pins.DesiredScope != 1 || prior.Pins.UID != r.Facts.UID || prior.Pins.Home != r.Facts.Home || prior.Pins.Worker != r.Worker || prior.Pins.Artifacts["helper"] != r.HelperSHA256 || prior.Pins.Installations["notification-worker"] != paths.Installation || prior.Pins.Installations["state"] != paths.State || prior.Pins.Installations["runtime"] != paths.Runtime {
		return result, true, fmt.Errorf("delivery LED output requires unchanged target, paths and helper")
	}
	if err = validateDeliveryOutputPolicy(before, r.Desired, prior.Pins.DesiredHash); err != nil {
		return result, true, err
	}
	if err = l.inputs(r, paths, d, before); err != nil {
		return result, true, err
	}
	expected, ok := canonicalNotificationReceipt(prior)
	if !ok {
		return result, true, fmt.Errorf("delivery LED output lost gateway receipt")
	}
	if admittedCommandHistory(prior) {
		if _, err = retainedCommandReceiver(ctx, d, prior, l.commands.notificationCommandLifecycle); err != nil {
			return result, true, err
		}
	}
	var enrollment consumerEnrollmentIntent
	if err = d.ReadJSON("consumer-enrollment.json", &enrollment); err != nil {
		return result, true, err
	}
	reservation := enrollment.Reservation
	if err = reservation.Validate(reservation.Producer.ID); err != nil {
		return result, true, err
	}
	if l.nativeResume != nil {
		nativeRequest := r
		if !retry {
			nativeRequest.Desired, nativeRequest.DesiredHash = before, prior.Pins.DesiredHash
		}
		if err = l.nativeResume(ctx, nativeRequest, paths); err != nil {
			return result, true, err
		}
	}
	if !retry {
		observed, e := l.output.inspect(ctx, paths.Installation)
		if e != nil {
			return result, true, e
		}
		if observed.Phase != "complete" || observed.Unbound || observed.Manifest.Mode != "delivery" || protocol.Hash(observed.Manifest) != expected {
			return result, true, fmt.Errorf("delivery LED output lost completed installation")
		}
		if commandActivityJournal(prior) {
			e = verifyCommandActivityReceipt(ctx, d, prior, observed.Manifest, l.commands)
		} else if deliveryOutputJournal(prior) {
			e = verifyDeliveryOutputReceipt(d, prior, observed.Manifest, l.scopeInspect)
		}
		if e != nil {
			return result, true, e
		}
		reviewed, e := l.review(ctx, paths.Installation, expected, !r.Desired.Notification.StatusLEDs.Enabled)
		if e != nil {
			return result, true, e
		}
		if protocol.Hash(reviewed) != expected {
			return result, true, fmt.Errorf("LED output review changed gateway pins")
		}
	}
	pins := deliveryOutputPins(prior, r.Desired, r.DesiredHash)
	step := &deliveryOutputStep{enrolledOutputStep{notificationLEDStep: notificationLEDStep{directory: d, paths: paths, expected: expected, desiredHash: r.DesiredHash, desiredScope: 1, desired: r.Desired, disabled: !r.Desired.Notification.StatusLEDs.Enabled, enrolled: true, production: true, lifecycle: l.output}, request: r, priorScope: prior.Pins.DesiredHash, reservation: reservation, coordination: notificationRemappingLifecycle{scope: l.scope, scopeInspect: l.scopeInspect}}}
	if err = retainDeclaration(d, r.Desired, r.DesiredHash, 1); err != nil {
		return result, true, err
	}
	if _, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step}); err != nil {
		return result, true, ledOutputPendingError{Server: r.Server, Cause: err}
	}
	full, _ := r.Desired.Fingerprint()
	if err = l.connect(ctx, r.Facts.Home, full, protocol.Hash(step.manifest)); err != nil {
		return result, true, ledOutputPendingError{Server: r.Server, Cause: err}
	}
	result.State, result.Code, result.Identity, result.LEDOutput = "delivery_admitted", "ok", &step.manifest.Identity, step.manifest.LEDOutput
	if step.manifest.Commands != nil {
		result.State = "commands_admitted"
		if step.manifest.Commands.Mode == "drain" {
			result.State = "commands_disabled"
		}
	}
	result.Next = "LED output permission updated; bindings, incidents, shared reporting and command results retained"
	return result, true, nil
}

type deliveryOutputStep struct{ enrolledOutputStep }

func (s *deliveryOutputStep) Name() string { return "notification-delivery-led-output" }

// Validate a pending output transition before independently resuming its
// unchanged native Signal service. Missing intent is allowed only before the
// first domain effect; a damaged retained intent never authorizes a restart.
func verifyPendingDeliveryOutputSignal(r ControlRequest, paths ResolvedPaths, d *safefs.Directory, current OperationJournal) error {
	prior, err := deliveryOutputPredecessor(d, current)
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
	_, err = readDeliveryOutputIntent(d, current)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
