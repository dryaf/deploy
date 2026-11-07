package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type requalificationLifecycle struct {
	inspect      func(context.Context, string) (install.InstallationObservation, error)
	inspectPause func(context.Context, install.DeliveryPauseRequest) (install.ManagedManifest, bool, error)
	pause        func(context.Context, install.DeliveryPauseRequest) (install.ManagedManifest, error)
	inputs       func(context.Context, ControlRequest, ResolvedPaths, *safefs.Directory, OperationJournal) error
	boundary     func(string) error
	nativeResume func(context.Context, ControlRequest, ResolvedPaths) error
	delivery     notificationDeliveryLifecycle
}

func defaultRequalification(helper string) requalificationLifecycle {
	return requalificationLifecycle{inspect: install.InspectRetainedInstallation, inspectPause: install.InspectDeliveryPause, pause: install.EnsureDeliveryPause, inputs: verifyRequalificationInputs, delivery: defaultNotificationDelivery(), nativeResume: func(ctx context.Context, r ControlRequest, p ResolvedPaths) error {
		return resumeCompletedSignalChild(ctx, r, p, helper, os.Getenv("DEPLOY_CONTROL_SIGNAL_ARTIFACT"), defaultSignalSetupLifecycle())
	}}
}
func requalificationJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-requalification"
}

// Renewal may follow a direct admission or a reviewed output-only change.
// Command authority remains excluded even when intake is currently disabled.
func requalificationCandidate(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 &&
		(j.Children[0].Name == "notification-delivery" || deliveryDeclarationJournal(j) || deliveryOutputJournal(j) && !admittedCommandHistory(j))
}

type requalificationIntent struct {
	Version     int                     `json:"version"`
	Operation   string                  `json:"operation"`
	DesiredHash string                  `json:"desired_hash"`
	Before      install.ManagedManifest `json:"before"`
}

func validateRequalificationBefore(m install.ManagedManifest) error {
	if m.Mode != "delivery" || m.DeliveryActivation == nil || m.DeliveryOwnerHash == "" || m.Commands != nil || m.CommandHistoryHash != "" || m.RecoveryCommitHash != "" || m.SignalVerification != nil || m.SignalProfiles != nil {
		return fmt.Errorf("guided requalification currently requires an unrecovered direct-alert gateway without command history")
	}
	return nil
}
func validateRequalificationManifest(before, after install.ManagedManifest) error {
	if err := validateRequalificationBefore(before); err != nil {
		return err
	}
	want := before
	want.Mode, want.DeliveryActivation, want.Commands = "verification", nil, nil
	want.UnitHash = after.UnitHash
	if !validDigest(after.UnitHash) || protocol.Hash(want) != protocol.Hash(after) {
		return fmt.Errorf("requalification changed retained installation authority")
	}
	return nil
}

func requalificationPredecessor(d *safefs.Directory, j OperationJournal) (OperationJournal, requalificationIntent, error) {
	var prior OperationJournal
	var v requalificationIntent
	if validateOperationJournal(j) != nil || !requalificationJournal(j) {
		return prior, v, fmt.Errorf("invalid requalification journal")
	}
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
		return prior, v, err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous || !requalificationCandidate(prior) {
		return prior, v, fmt.Errorf("requalification lost delivery predecessor")
	}
	want := prior.Pins
	want.PreviousOperation, want.Previous = prior.ID, prior.Digest()
	if !reflect.DeepEqual(want, j.Pins) {
		return prior, v, fmt.Errorf("requalification changed declaration or target")
	}
	if err := d.ReadJSON("requalification-"+j.Children[0].Operation+".json", &v); err != nil {
		return prior, v, err
	}
	if v.Version != 1 || v.Operation != j.Children[0].Operation || v.DesiredHash != j.Pins.DesiredHash || protocol.Hash(v.Before) != prior.Children[0].Receipt || v.Before.Root != j.Pins.Installations["notification-worker"] || v.Before.BinaryHash != j.Pins.Artifacts["helper"] || validateRequalificationBefore(v.Before) != nil {
		return prior, v, fmt.Errorf("requalification intent changed")
	}
	return prior, v, nil
}

// Find the unchanged enrollment through any number of completed renewals. Every
// pause and delivery edge remains independently pinned; missing proof is fatal.
func requalificationEnrollment(d *safefs.Directory, j OperationJournal) (OperationJournal, error) {
	return requalificationAncestry(d, j, nil)
}

func requalificationAncestry(d *safefs.Directory, j OperationJournal, output func(OperationJournal) error) (OperationJournal, error) {
	for depth := 0; depth < 128; depth++ {
		if requalificationJournal(j) {
			prior, _, err := requalificationPredecessor(d, j)
			if err != nil {
				return j, err
			}
			j = prior
			continue
		}
		if deliveryOutputJournal(j) {
			if admittedCommandHistory(j) {
				return j, fmt.Errorf("renewal cannot cross command authority")
			}
			if _, err := readDeliveryOutputIntent(d, j); err != nil {
				return j, err
			}
			if output != nil {
				if err := output(j); err != nil {
					return j, err
				}
			}
			prior, err := deliveryOutputPredecessor(d, j)
			if err != nil {
				return j, err
			}
			j = prior
			continue
		}
		if deliveryDeclarationJournal(j) || !requalificationCandidate(j) {
			return j, nil
		}
		var prior OperationJournal
		if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
			return j, err
		}
		if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous || !enrolledNotificationJournal(prior) || prior.Pins.DesiredHash != j.Pins.DesiredHash {
			return j, fmt.Errorf("renewed delivery lost enrollment")
		}
		j = prior
	}
	return j, fmt.Errorf("requalification history exceeds review bound")
}

func verifyRequalificationInputs(ctx context.Context, r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal) error {
	raw, err := readLimitsRuntime(p.Runtime)
	if err != nil {
		return err
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return err
	}
	reviewed, err := retainedProviderReviewRequest(r, r.Desired, input)
	if err != nil {
		return err
	}
	if err = verifyLEDOutputInputs(reviewed, p, d, r.Desired); err != nil {
		return err
	}
	if requalificationJournal(j) {
		j, _, err = requalificationPredecessor(d, j)
		if err != nil {
			return err
		}
	}
	origin, err := requalificationAncestry(d, j, func(output OperationJournal) error {
		return verifyMappingScope(d, output, backup.InspectRetainedDeliveryPolicyScope)
	})
	if err != nil {
		return err
	}
	if err = verifyMappingScope(d, origin, backup.InspectRetainedDeliveryPolicyScope); err != nil {
		return err
	}
	if err = verifyAdmittedConsumerInput(r, p, d, j, input); err != nil {
		return err
	}
	observation, err := backup.InspectSetup(ctx, r.Facts.Home)
	if err != nil {
		return err
	}
	full, err := r.Desired.Fingerprint()
	if err != nil {
		return err
	}
	if observation.DesiredHash != full || observation.Stage != "complete" {
		return fmt.Errorf("finish the declared backup setup before requalification")
	}
	return nil
}

// Only qualify-begin starts a pause. Apply/resume can finish the exact retained
// pause and later admit delivery using fresh qualification, never invent it.
func reconcileRequalification(ctx context.Context, r ControlRequest, p ResolvedPaths, l requalificationLifecycle) (ControlResult, bool, error) {
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
		return result, true, fmt.Errorf("invalid requalification coordinator")
	}
	retry := requalificationJournal(current)
	if !retry && (r.Action != "qualify-begin" || !requalificationCandidate(current)) {
		return result, false, nil
	}
	if current.Pins.DesiredScope != r.DesiredScope || current.Pins.DesiredHash != r.DesiredHash || current.Pins.Worker != r.Worker || current.Pins.UID != r.Facts.UID || current.Pins.Home != r.Facts.Home || current.Pins.Artifacts["helper"] != r.HelperSHA256 || current.Pins.Installations["notification-worker"] != p.Installation || current.Pins.Installations["state"] != p.State || current.Pins.Installations["runtime"] != p.Runtime {
		return result, true, fmt.Errorf("requalification requires unchanged YAML, target and helper")
	}
	if r.Desired.Notification.Commands.Enabled {
		return result, true, fmt.Errorf("command requalification requires its receiver admission lifecycle")
	}
	prior := current
	if retry {
		if err = d.ReadJSON("history/"+current.Pins.PreviousOperation+".json", &prior); err != nil {
			return result, true, err
		}
		if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != current.Pins.PreviousOperation || prior.Digest() != current.Pins.Previous {
			return result, true, fmt.Errorf("requalification predecessor changed")
		}
	} else if !current.Complete {
		return result, true, fmt.Errorf("finish pending delivery admission before requalification")
	}
	if r.Action == "resume" && l.nativeResume != nil {
		if err = l.nativeResume(ctx, r, p); err != nil {
			return result, true, err
		}
	}
	if err = l.inputs(ctx, r, p, d, prior); err != nil {
		return result, true, err
	}
	if !retry {
		o, e := l.inspect(ctx, p.Installation)
		if e != nil {
			return result, true, e
		}
		if o.Phase != "complete" || o.Unbound || protocol.Hash(o.Manifest) != prior.Children[0].Receipt {
			return result, true, fmt.Errorf("gateway changed before requalification")
		}
		if err = validateRequalificationBefore(o.Manifest); err != nil {
			return result, true, err
		}
	}
	pins := prior.Pins
	pins.PreviousOperation, pins.Previous = prior.ID, prior.Digest()
	step := &requalificationStep{d: d, paths: p, expected: prior.Children[0].Receipt, desired: r.DesiredHash, l: l}
	completed, err := (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step})
	if err != nil {
		return result, true, err
	}
	result.State, result.Code, result.Next = "needs_qualification", "qualification_required", "delivery is paused; queued work and ownership remain; run deploy notification-worker qualify "+r.Server+", then apply to resume delivery"
	result.Identity = &step.manifest.Identity
	if r.Action == "apply" || r.Action == "resume" {
		result, err = advanceNotificationDelivery(ctx, r, p, d, completed, result, l.delivery)
	}
	return result, true, err
}

type requalificationStep struct {
	commands          bool
	d                 *safefs.Directory
	paths             ResolvedPaths
	expected, desired string
	l                 requalificationLifecycle
	manifest          install.ManagedManifest
}

func (s *requalificationStep) Name() string { return "notification-requalification" }
func (s *requalificationStep) intent(op string) (requalificationIntent, error) {
	var v requalificationIntent
	err := s.d.ReadJSON("requalification-"+op+".json", &v)
	if err == nil && (v.Version != 1 || v.Operation != op || v.DesiredHash != s.desired || protocol.Hash(v.Before) != s.expected || v.Before.Root != s.paths.Installation || s.validateBefore(v.Before) != nil) {
		err = fmt.Errorf("retained requalification intent changed")
	}
	return v, err
}
func (s *requalificationStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	v, err := s.intent(op)
	if errors.Is(err, os.ErrNotExist) {
		o, e := s.l.inspect(ctx, s.paths.Installation)
		if e != nil {
			return "", false, e
		}
		if o.Phase == "complete" && protocol.Hash(o.Manifest) == s.expected {
			return "", false, nil
		}
	}
	if err != nil {
		return "", false, err
	}
	m, done, err := s.l.inspectPause(ctx, install.DeliveryPauseRequest{Root: s.paths.Installation, ManifestHash: s.expected})
	if err != nil || !done {
		return "", false, err
	}
	if err = s.validateManifest(v.Before, m); err != nil {
		return "", false, err
	}
	s.manifest = m
	return protocol.Hash(m), true, nil
}
func (s *requalificationStep) Run(ctx context.Context, op string) error {
	_, err := s.intent(op)
	if errors.Is(err, os.ErrNotExist) {
		o, e := s.l.inspect(ctx, s.paths.Installation)
		if e != nil {
			return e
		}
		if o.Phase != "complete" || protocol.Hash(o.Manifest) != s.expected {
			return fmt.Errorf("gateway changed before retaining requalification")
		}
		if err = s.validateBefore(o.Manifest); err != nil {
			return err
		}
		err = s.d.WriteJSON("requalification-"+op+".json", requalificationIntent{1, op, s.desired, o.Manifest}, false)
	}
	if err != nil {
		return err
	}
	if s.l.boundary != nil {
		if err = s.l.boundary("requalification-intent"); err != nil {
			return err
		}
	}
	_, err = s.l.pause(ctx, install.DeliveryPauseRequest{Root: s.paths.Installation, ManifestHash: s.expected})
	if err == nil && s.l.boundary != nil {
		err = s.l.boundary("requalification-domain-complete")
	}
	return err
}

func (s *requalificationStep) validateBefore(m install.ManagedManifest) error {
	if s.commands {
		return validateCommandRequalificationBefore(m)
	}
	return validateRequalificationBefore(m)
}
func (s *requalificationStep) validateManifest(before, after install.ManagedManifest) error {
	if s.commands {
		return validateCommandRequalificationManifest(before, after)
	}
	return validateRequalificationManifest(before, after)
}
