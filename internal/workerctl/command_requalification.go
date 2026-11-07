package workerctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func commandPauseJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-command-requalification"
}
func commandDeliveryJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-command-delivery"
}
func validateCommandRequalificationBefore(m install.ManagedManifest) error {
	if m.Mode != "delivery" || m.DeliveryActivation == nil || m.DeliveryOwnerHash == "" || !validDigest(m.CommandHistoryHash) || m.Commands != nil && m.Commands.Mode != "drain" || m.RecoveryCommitHash != "" || m.SignalVerification != nil || m.SignalProfiles != nil {
		return fmt.Errorf("disable commands before renewing an unrecovered command gateway")
	}
	return nil
}
func validateCommandRequalificationManifest(before, after install.ManagedManifest) error {
	if err := validateCommandRequalificationBefore(before); err != nil {
		return err
	}
	want := before
	want.Mode, want.DeliveryActivation, want.Commands = "verification", nil, nil
	want.UnitHash = after.UnitHash
	if !validDigest(after.UnitHash) || protocol.Hash(want) != protocol.Hash(after) {
		return fmt.Errorf("command qualification pause changed retained authority")
	}
	return nil
}

// Every pause and renewal is a distinct edge. Command authority stays at its
// original receiver; fresh delivery evidence never re-enables command intake.
func commandRenewalPredecessor(d *safefs.Directory, j OperationJournal) (OperationJournal, error) {
	var prior OperationJournal
	if validateOperationJournal(j) != nil || (!commandPauseJournal(j) && !commandDeliveryJournal(j)) {
		return prior, fmt.Errorf("invalid command renewal journal")
	}
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
		return prior, err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous || prior.Pins.DesiredHash != j.Pins.DesiredHash {
		return prior, fmt.Errorf("command renewal lost its completed predecessor")
	}
	want := prior.Pins
	want.PreviousOperation, want.Previous = prior.ID, prior.Digest()
	want.Artifacts = maps.Clone(prior.Pins.Artifacts)
	if commandPauseJournal(j) {
		if !admittedCommandHistory(prior) {
			return prior, fmt.Errorf("command pause lost admission")
		}
		desired, err := readCommandDeclaration(d, j)
		if err != nil {
			return prior, err
		}
		if desired.Notification == nil || desired.Notification.Commands.Enabled {
			return prior, fmt.Errorf("command renewal requires disabled intake")
		}
		want.Artifacts["command-backup-policy"] = j.Pins.Artifacts["command-backup-policy"]
		var intent requalificationIntent
		intentErr := d.ReadJSON("requalification-"+j.Children[0].Operation+".json", &intent)
		if intentErr != nil && !(errors.Is(intentErr, os.ErrNotExist) && !j.Complete && !j.Children[0].Complete) {
			return prior, intentErr
		}
		receipt, valid := canonicalNotificationReceipt(prior)
		if intentErr == nil && (!valid || intent.Version != 1 || intent.Operation != j.Children[0].Operation || intent.DesiredHash != j.Pins.DesiredHash || protocol.Hash(intent.Before) != receipt || validateCommandRequalificationBefore(intent.Before) != nil) {
			return prior, fmt.Errorf("command pause intent changed")
		}
	} else {
		if (!commandPauseJournal(prior) && !commandLimitsJournal(prior)) || !validDigest(j.Pins.Artifacts["qualification"]) || j.Pins.Artifacts["qualification"] == prior.Pins.Artifacts["qualification"] {
			return prior, fmt.Errorf("command renewal requires fresh pinned qualification")
		}
		if commandLimitsJournal(prior) {
			if _, err := readDeliveryProviderIntent(d, prior); err != nil {
				return prior, err
			}
		} else if _, err := commandRenewalPredecessor(d, prior); err != nil {
			return prior, err
		}
		want.Artifacts["qualification"] = j.Pins.Artifacts["qualification"]
	}
	if !reflect.DeepEqual(want, j.Pins) {
		return prior, fmt.Errorf("command renewal changed target or authority")
	}
	return prior, nil
}

type commandRequalificationLifecycle struct {
	requalificationLifecycle
	commands commandActivityLifecycle
}

func defaultCommandRequalification(helper string) commandRequalificationLifecycle {
	l := commandRequalificationLifecycle{defaultRequalification(helper), defaultCommandActivity()}
	l.inputs = func(ctx context.Context, r ControlRequest, p ResolvedPaths, d *safefs.Directory, _ OperationJournal) error {
		if err := verifyLEDOutputInputs(r, p, d, r.Desired); err != nil {
			return err
		}
		observed, err := backup.InspectSetup(ctx, r.Facts.Home)
		if err != nil {
			return err
		}
		full, err := r.Desired.Fingerprint()
		if err != nil {
			return err
		}
		if observed.Stage != "complete" || observed.DesiredHash != full {
			return fmt.Errorf("finish applied backup changes before command requalification")
		}
		return nil
	}
	return l
}

type commandPauseStep struct{ requalificationStep }

func (s *commandPauseStep) Name() string { return "notification-command-requalification" }

type commandDeliveryStep struct {
	notificationDeliveryStep
	before install.ManagedManifest
}

func (s *commandDeliveryStep) Name() string { return "notification-command-delivery" }
func (s *commandDeliveryStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	receipt, done, err := s.notificationDeliveryStep.Inspect(ctx, op)
	if err != nil || !done {
		return receipt, done, err
	}
	want := s.before
	want.Mode, want.DeliveryActivation, want.UnitHash = "delivery", s.manifest.DeliveryActivation, s.manifest.UnitHash
	if protocol.Hash(want) != protocol.Hash(s.manifest) {
		return "", false, fmt.Errorf("renewed delivery changed command authority")
	}
	return receipt, true, nil
}

func reconcileCommandRequalification(ctx context.Context, r ControlRequest, p ResolvedPaths, l commandRequalificationLifecycle) (ControlResult, bool, error) {
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
	retry := commandLimitsJournal(current) || commandPauseJournal(current) || commandDeliveryJournal(current)
	if !retry && (r.Action != "qualify-begin" || !admittedCommandHistory(current)) {
		return result, false, nil
	}
	// A completed renewal permits later YAML command policy edits through their
	// existing activity workflow, but an unfinished renewal pins its declaration.
	if commandDeliveryJournal(current) && current.Complete && r.Action != "qualify-begin" && current.Pins.DesiredHash != r.DesiredHash {
		return result, false, nil
	}
	if validateOperationJournal(current) != nil || current.Pins.DesiredScope != 1 || r.DesiredScope != 1 || current.Pins.DesiredHash != r.DesiredHash || current.Pins.Worker != r.Worker || current.Pins.UID != r.Facts.UID || current.Pins.Home != r.Facts.Home || current.Pins.Artifacts["helper"] != r.HelperSHA256 || current.Pins.Installations["notification-worker"] != p.Installation || current.Pins.Installations["state"] != p.State || current.Pins.Installations["runtime"] != p.Runtime {
		return result, true, fmt.Errorf("command requalification requires unchanged target and notification YAML")
	}
	if r.Desired.Notification == nil || r.Desired.Notification.Commands.Enabled {
		return result, true, fmt.Errorf("set commands.enabled: false and apply before requalification")
	}
	full, err := r.Desired.Fingerprint()
	if err != nil {
		return result, true, err
	}
	if (commandLimitsJournal(current) || commandPauseJournal(current) || commandDeliveryJournal(current) && !current.Complete) && full != current.Pins.Artifacts["command-backup-policy"] {
		return result, true, fmt.Errorf("resume command qualification with its retained backup declaration")
	}
	if !retry && !current.Complete {
		return result, true, fmt.Errorf("finish command activity before qualification")
	}
	if retry && !commandLimitsJournal(current) {
		if _, err = commandRenewalPredecessor(d, current); err != nil {
			return result, true, err
		}
	}
	if r.Action == "resume" && l.nativeResume != nil {
		if err = l.nativeResume(ctx, r, p); err != nil {
			return result, true, err
		}
	}
	if err = l.inputs(ctx, r, p, d, current); err != nil {
		return result, true, err
	}
	if _, err = retainedCommandReceiver(ctx, d, current, l.commands.notificationCommandLifecycle); err != nil {
		return result, true, err
	}
	if r.Action == "qualify-begin" && commandDeliveryJournal(current) && current.Complete {
		retry = false
	}
	if !retry {
		observed, e := l.inspect(ctx, p.Installation)
		if e != nil {
			return result, true, e
		}
		receipt, valid := canonicalNotificationReceipt(current)
		if !valid || observed.Phase != "complete" || protocol.Hash(observed.Manifest) != receipt {
			return result, true, fmt.Errorf("command gateway changed before qualification")
		}
		if err = validateCommandRequalificationBefore(observed.Manifest); err != nil {
			return result, true, err
		}
		if commandActivityJournal(current) {
			err = verifyCommandActivityReceipt(ctx, d, current, observed.Manifest, l.commands)
		} else if deliveryOutputJournal(current) {
			err = verifyDeliveryOutputReceipt(d, current, observed.Manifest, l.commands.scopeInspect)
		} else if commandDeliveryJournal(current) {
			err = verifyCommandRenewalReceipt(ctx, d, current, observed.Manifest, l)
		} else {
			err = fmt.Errorf("complete command disable before renewal")
		}
		if err != nil {
			return result, true, err
		}
		pins := current.Pins
		pins.PreviousOperation, pins.Previous = current.ID, current.Digest()
		pins.Artifacts = maps.Clone(pins.Artifacts)
		pins.Artifacts["command-backup-policy"] = full
		if err = retainDeclaration(d, r.Desired, full); err != nil {
			return result, true, err
		}
		step := &commandPauseStep{requalificationStep{d: d, paths: p, expected: receipt, desired: r.DesiredHash, l: l.requalificationLifecycle, commands: true}}
		current, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step})
		if err != nil {
			return result, true, err
		}
	} else if commandPauseJournal(current) {
		var prior OperationJournal
		if err = d.ReadJSON("history/"+current.Pins.PreviousOperation+".json", &prior); err != nil {
			return result, true, err
		}
		if validateOperationJournal(prior) != nil || !prior.Complete || prior.Digest() != current.Pins.Previous {
			return result, true, fmt.Errorf("command pause predecessor changed")
		}
		receipt, valid := canonicalNotificationReceipt(prior)
		if !valid {
			return result, true, fmt.Errorf("command receipt missing")
		}
		step := &commandPauseStep{requalificationStep{d: d, paths: p, expected: receipt, desired: r.DesiredHash, l: l.requalificationLifecycle, commands: true}}
		current, err = (JournalCoordinator{Directory: d}).Apply(ctx, current.Pins, []LifecycleStep{step})
		if err != nil {
			return result, true, err
		}
	}
	if commandLimitsJournal(current) {
		if !current.Complete {
			return result, true, fmt.Errorf("finish notification budget changes before qualification")
		}
		if err = verifyMappingScope(d, current, l.commands.scopeInspect); err != nil {
			return result, true, err
		}
	} else if _, err = commandRenewalPredecessor(d, current); err != nil {
		return result, true, err
	}
	result.State, result.Code, result.Next = "needs_qualification", "qualification_required", "delivery paused; record fresh qualification, then apply; command intake stays disabled"
	if r.Action == "qualify-begin" {
		return result, true, nil
	}
	paused := current
	pins := current.Pins
	var request install.DeliveryAdmissionRequest
	if commandDeliveryJournal(current) {
		paused, err = commandRenewalPredecessor(d, current)
		if err != nil {
			return result, true, err
		}
		request = install.DeliveryAdmissionRequest{Root: p.Installation, ManifestHash: paused.Children[0].Receipt, QualificationHash: current.Pins.Artifacts["qualification"]}
	} else {
		var ready bool
		request, ready, err = l.delivery.review(ctx, p.Installation, paused.Children[0].Receipt)
		if err != nil || !ready {
			return result, true, err
		}
		if request.Root != p.Installation || request.ManifestHash != paused.Children[0].Receipt || !validDigest(request.QualificationHash) || request.QualificationHash == paused.Pins.Artifacts["qualification"] {
			return result, true, fmt.Errorf("command delivery requires fresh exact qualification")
		}
		pins.PreviousOperation, pins.Previous = paused.ID, paused.Digest()
		pins.Artifacts = maps.Clone(pins.Artifacts)
		pins.Artifacts["qualification"] = request.QualificationHash
	}
	observed, e := l.inspect(ctx, p.Installation)
	if e != nil {
		return result, true, e
	}
	before, err := commandRenewalPausedManifest(d, paused, observed.Manifest)
	if err != nil {
		return result, true, err
	}
	if !commandDeliveryJournal(current) && protocol.Hash(before) != paused.Children[0].Receipt {
		return result, true, fmt.Errorf("paused command installation changed")
	}
	step := &commandDeliveryStep{notificationDeliveryStep: notificationDeliveryStep{request: request, lifecycle: l.delivery}, before: before}
	if _, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step}); err != nil {
		return result, true, err
	}
	if err = l.delivery.connect(ctx, r.Facts.Home, full, protocol.Hash(step.manifest)); err != nil {
		return result, true, err
	}
	result.State, result.Code, result.Identity, result.Next = "commands_disabled", "ok", &step.manifest.Identity, "delivery qualification renewed; command permissions and results retained; set commands.enabled: true and apply to admit intake"
	return result, true, nil
}

func verifyCommandRenewalReceipt(ctx context.Context, d *safefs.Directory, j OperationJournal, m install.ManagedManifest, l commandRequalificationLifecycle) error {
	prior, err := commandRenewalPredecessor(d, j)
	if err != nil {
		return err
	}
	paused := j
	if commandDeliveryJournal(j) {
		paused = prior
	}
	before, err := commandRenewalPausedManifest(d, paused, m)
	if err != nil {
		return err
	}
	if commandPauseJournal(j) && protocol.Hash(before) != protocol.Hash(m) {
		return fmt.Errorf("command pause authority changed")
	}
	if !j.Complete || protocol.Hash(m) != j.Children[0].Receipt {
		return fmt.Errorf("command renewal receipt changed")
	}
	if commandPauseJournal(j) {
		var v requalificationIntent
		if err = d.ReadJSON("requalification-"+paused.Children[0].Operation+".json", &v); err != nil {
			return err
		}
		actual, done, e := l.inspectPause(ctx, install.DeliveryPauseRequest{Root: m.Root, ManifestHash: protocol.Hash(v.Before)})
		if e != nil {
			return e
		}
		if !done || protocol.Hash(actual) != protocol.Hash(m) {
			return fmt.Errorf("command pause domain receipt changed")
		}
	} else {
		actual, done, e := l.delivery.inspect(ctx, install.DeliveryAdmissionRequest{Root: m.Root, ManifestHash: paused.Children[0].Receipt, QualificationHash: j.Pins.Artifacts["qualification"]})
		if e != nil {
			return e
		}
		if !done || protocol.Hash(actual) != protocol.Hash(m) {
			return fmt.Errorf("command renewal qualification receipt changed")
		}
	}
	_, err = retainedCommandReceiver(ctx, d, j, l.commands.notificationCommandLifecycle)
	return err
}
