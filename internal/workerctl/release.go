package workerctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type notificationReleaseLifecycle struct {
	inspect func(context.Context, string) (install.InstallationObservation, error)
	update  func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error)
}

func releaseJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-artifact"
}
func unboundPredecessor(j OperationJournal) bool {
	if j.Pins.Action != "apply" || len(j.Children) != 1 {
		return false
	}
	switch j.Children[0].Name {
	case "notification-adopt", "notification-bootstrap", "notification-led-output", "notification-activity", "notification-artifact", "notification-command-draft", "notification-limits", "notification-routes", "notification-providers":
		return true
	}
	return false
}

func reconcileNotificationRelease(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, l notificationReleaseLifecycle) (ControlResult, bool, error) {
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
	isRelease := releaseJournal(old)
	if !isRelease && old.Pins.Artifacts["helper"] == r.HelperSHA256 && old.Pins.DesiredScope == r.DesiredScope && (old.Pins.DesiredHash == r.DesiredHash || !unboundPredecessor(old)) {
		return result, false, nil
	}
	if !unboundPredecessor(old) || old.Pins.Worker != r.Worker || old.Pins.UID != r.Facts.UID || old.Pins.Home != r.Facts.Home || old.Pins.Installations["notification-worker"] != paths.Installation || old.Pins.Installations["state"] != paths.State || old.Pins.Installations["runtime"] != paths.Runtime {
		return result, true, fmt.Errorf("artifact/scope update requires the retained unbound installation")
	}
	if old.Pins.DesiredScope != r.DesiredScope && !(old.Pins.DesiredScope == 0 && r.DesiredScope == 1) {
		return result, true, fmt.Errorf("unsupported policy scope conversion")
	}
	retry := isRelease && old.Pins.DesiredHash == r.DesiredHash && old.Pins.DesiredScope == r.DesiredScope && old.Pins.Artifacts["helper"] == r.HelperSHA256
	pins, prior := old.Pins, old
	if retry {
		prior = OperationJournal{}
		if err = d.ReadJSON("history/"+old.Pins.PreviousOperation+".json", &prior); err != nil {
			return result, true, err
		}
		if validateOperationJournal(prior) != nil || prior.Digest() != old.Pins.Previous || prior.ID != old.Pins.PreviousOperation {
			return result, true, fmt.Errorf("artifact predecessor changed")
		}
	} else {
		if !old.Complete {
			return result, true, fmt.Errorf("finish the pending operation before changing the release")
		}
		pins.Previous, pins.PreviousOperation = old.Digest(), old.ID
		pins.DesiredHash, pins.DesiredScope = r.DesiredHash, r.DesiredScope
		pins.Artifacts = maps.Clone(old.Pins.Artifacts)
		pins.Artifacts["helper"] = r.HelperSHA256
	}
	if !prior.Complete || !unboundPredecessor(prior) {
		return result, true, fmt.Errorf("artifact predecessor is not a completed unbound lifecycle")
	}
	before, err := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
	if err != nil {
		return result, true, err
	}
	if before.Notification == nil {
		return result, true, fmt.Errorf("retained notification policy missing")
	}
	next := before
	n := *before.Notification
	next.Notification = &n
	next.Notification.Artifacts.Catalog = r.Desired.Notification.Artifacts.Catalog
	next.Notification.Artifacts.Helper = r.Desired.Notification.Artifacts.Helper
	next.Notification.Artifacts.HelperSHA256 = r.Desired.Notification.Artifacts.HelperSHA256
	hash, err := declarationFingerprint(next, r.DesiredScope)
	if err != nil || hash != r.DesiredHash {
		if !retry && old.Pins.Artifacts["helper"] == r.HelperSHA256 && old.Pins.DesiredScope == r.DesiredScope {
			return result, false, nil
		}
		return result, true, fmt.Errorf("apply policy changes separately from artifact/scope migration")
	}
	if !retry {
		if err = requireUnreservedNotificationPolicy(r, paths, before, prior); err != nil {
			return result, true, err
		}
		o, e := l.inspect(ctx, paths.Installation)
		if e != nil {
			return result, true, e
		}
		if !o.Unbound || o.Phase != "complete" || o.Manifest.Mode != "verification" || protocol.Hash(o.Manifest) != prior.Children[0].Receipt || o.Manifest.DeliveryOwnerHash != "" || o.Manifest.RecoveryCommitHash != "" {
			return result, true, fmt.Errorf("artifact/scope update requires an unchanged unbound verification gateway")
		}
		if err = verifyActivityReceipt(d, prior, o.Manifest); err != nil {
			return result, true, err
		}
		if err = verifyReleaseReceipt(d, prior, o.Manifest); err != nil {
			return result, true, err
		}
	}
	if err = verifyLEDOutputInputs(r, paths, d, before); err != nil {
		return result, true, err
	}
	step := &notificationReleaseStep{directory: d, paths: paths, helper: helper, expected: prior.Children[0].Receipt, desired: r.Desired, desiredHash: r.DesiredHash, desiredScope: r.DesiredScope, helperHash: r.HelperSHA256, lifecycle: l}
	if _, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step}); err != nil {
		return result, true, err
	}
	result.State, result.Code = "waiting_for_producers", "ok"
	result.Identity, result.LEDOutput = &step.manifest.Identity, step.manifest.LEDOutput
	result.Next = "declared artifact and policy scope reconciled; retained gateway identity and runtime policy verified; qualification remains separate"
	return result, true, nil
}

type notificationReleaseIntent struct {
	Version     int                     `json:"version"`
	Operation   string                  `json:"operation"`
	Before      install.ManagedManifest `json:"before"`
	DesiredHash string                  `json:"desired_hash"`
	Helper      string                  `json:"helper_sha256"`
}
type notificationReleaseStep struct {
	directory                                 *safefs.Directory
	paths                                     ResolvedPaths
	helper, helperHash, expected, desiredHash string
	desiredScope                              int
	desired                                   config.DesiredWorkers
	lifecycle                                 notificationReleaseLifecycle
	manifest                                  install.ManagedManifest
}

func (s *notificationReleaseStep) Name() string { return "notification-artifact" }
func (s *notificationReleaseStep) intent(op string) (notificationReleaseIntent, error) {
	var v notificationReleaseIntent
	err := s.directory.ReadJSON("artifact-"+op+".json", &v)
	if err == nil && (v.Version != 1 || v.Operation != op || protocol.Hash(v.Before) != s.expected || v.DesiredHash != s.desiredHash || v.Helper != s.helperHash) {
		err = fmt.Errorf("artifact intent changed")
	}
	return v, err
}
func validateReleaseManifest(before, after install.ManagedManifest, helper string) error {
	want := before
	want.Revision, want.UnitHash = after.Revision, after.UnitHash
	want.Paths.Config, want.Paths.Executable = after.Paths.Config, after.Paths.Executable
	want.BinaryHash = helper
	if protocol.Hash(want) != protocol.Hash(after) || after.Mode != "verification" {
		return fmt.Errorf("artifact update changed retained policy or identity")
	}
	return nil
}
func (s *notificationReleaseStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	o, err := s.lifecycle.inspect(ctx, s.paths.Installation)
	if err != nil {
		return "", false, err
	}
	v, e := s.intent(op)
	if errors.Is(e, os.ErrNotExist) && o.Phase == "complete" && protocol.Hash(o.Manifest) == s.expected {
		return "", false, nil
	}
	if e != nil {
		return "", false, e
	}
	if o.Phase != "complete" {
		return "", false, nil
	}
	if o.Manifest.BinaryHash != s.helperHash && protocol.Hash(o.Manifest) == s.expected {
		return "", false, nil
	}
	if !o.Unbound {
		return "", false, fmt.Errorf("artifact update lost unbound authority")
	}
	if err = validateReleaseManifest(v.Before, o.Manifest, s.helperHash); err != nil {
		return "", false, err
	}
	if _, err = readDeclaration(s.directory, s.desiredHash, s.desiredScope); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	s.manifest = o.Manifest
	return protocol.Hash(o.Manifest), true, nil
}
func (s *notificationReleaseStep) Run(ctx context.Context, op string) error {
	v, err := s.intent(op)
	if errors.Is(err, os.ErrNotExist) {
		o, e := s.lifecycle.inspect(ctx, s.paths.Installation)
		if e != nil {
			return e
		}
		if !o.Unbound || o.Phase != "complete" || o.Manifest.Mode != "verification" || protocol.Hash(o.Manifest) != s.expected {
			return fmt.Errorf("gateway changed before artifact update")
		}
		v = notificationReleaseIntent{1, op, o.Manifest, s.desiredHash, s.helperHash}
		err = s.directory.WriteJSON("artifact-"+op+".json", v, false)
	}
	if err != nil {
		return err
	}
	if err = retainDeclaration(s.directory, s.desired, s.desiredHash, s.desiredScope); err != nil {
		return err
	}
	spec := install.ManagedSpec{Version: 1, Root: s.paths.Installation, State: s.paths.State, UnitDirectory: s.paths.UnitDirectory, RuntimeFile: s.paths.Runtime, BinaryFile: s.helper, BinarySHA256: s.helperHash, DeniedPaths: s.paths.DeniedPaths, FreshSignal: v.Before.FreshSignal}
	_, err = s.lifecycle.update(ctx, spec, s.expected)
	return err
}
func verifyReleaseReceipt(d *safefs.Directory, j OperationJournal, m install.ManagedManifest) error {
	if err := verifyRetainedSignalAdoption(d); err != nil {
		return err
	}
	if adoptionJournal(j) {
		return verifyAdoptionReceipt(d, j, m)
	}
	if enrolledOutputJournal(j) {
		return verifyEnrolledOutputReceipt(d, j, m, nil)
	}
	if sharedLimitsJournal(j) {
		return verifySharedLimitsReceipt(d, j, m, nil)
	}
	if mappingJournal(j) {
		return verifyMappingReceipt(d, j, m)
	}
	if limitsJournal(j) {
		return verifyLimitsReceipt(d, j, m)
	}
	if !releaseJournal(j) {
		return nil
	}
	var prior OperationJournal
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
		return err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous || !unboundPredecessor(prior) {
		return fmt.Errorf("artifact predecessor authority missing or changed")
	}
	var v notificationReleaseIntent
	if err := d.ReadJSON("artifact-"+j.Children[0].Operation+".json", &v); err != nil {
		return err
	}
	if v.Version != 1 || v.Operation != j.Children[0].Operation || v.DesiredHash != j.Pins.DesiredHash || v.Helper != j.Pins.Artifacts["helper"] || protocol.Hash(v.Before) != prior.Children[0].Receipt || protocol.Hash(m) != j.Children[0].Receipt {
		return fmt.Errorf("artifact completion receipt changed")
	}
	return validateReleaseManifest(v.Before, m, v.Helper)
}
