package workerctl

import (
	"context"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func uninstallNotificationControl(ctx context.Context, r ControlRequest, paths ResolvedPaths, inspect func(context.Context, string) (install.InstallationObservation, error), remove func(context.Context, string, string) (install.ManagedManifest, error)) (ControlResult, error) {
	return uninstallNotificationWithLED(ctx, r, paths, inspect, remove, install.SetLEDOutput)
}

func uninstallNotificationWithLED(ctx context.Context, r ControlRequest, paths ResolvedPaths, inspect func(context.Context, string) (install.InstallationObservation, error), remove func(context.Context, string, string) (install.ManagedManifest, error), disable func(context.Context, string, string, bool) (install.ManagedManifest, error)) (ControlResult, error) {
	result := ControlResult{Version: 1}
	d, err := safefs.Open(paths.Control)
	if err != nil {
		return result, fmt.Errorf("no readable declarative ownership record; import or recovery review required")
	}
	defer d.Close()
	// Read one atomic predecessor snapshot. JournalCoordinator rechecks the exact
	// predecessor receipt under its exclusive lock before creating removal intent.
	var old OperationJournal
	if err = d.ReadJSON("operation.json", &old); err != nil {
		return result, err
	}
	if err = validateOperationJournal(old); err != nil {
		return result, err
	}
	if old.Pins.UID != r.Facts.UID || old.Pins.Home != r.Facts.Home || old.Pins.Installations["notification-worker"] != paths.Installation || old.Pins.Installations["state"] != paths.State || old.Pins.Installations["runtime"] != paths.Runtime {
		return result, fmt.Errorf("uninstall target differs from pinned installation")
	}
	var expected string
	cleanupLED := false
	stopped := false
	pins := old.Pins
	if old.Pins.Action == "uninstall" {
		cleanupLED = len(old.Children) == 2 && old.Children[0].Name == "notification-remove-led-output" && old.Children[1].Name == "notification-uninstall"
		if !cleanupLED && (len(old.Children) != 1 || old.Children[0].Name != "notification-uninstall") {
			return result, fmt.Errorf("uninstall child sequence changed")
		}
		if old.Pins.DesiredHash != r.DesiredHash {
			return result, fmt.Errorf("uninstall retry changed declared policy")
		}
		var predecessor OperationJournal
		if err = d.ReadJSON("history/"+old.Pins.PreviousOperation+".json", &predecessor); err != nil {
			return result, err
		}
		if predecessor.Digest() != old.Pins.Previous || len(predecessor.Children) != 1 {
			return result, fmt.Errorf("uninstall predecessor authority changed")
		}
		expected = predecessor.Children[0].Receipt
		stopped, err = activityStoppedPredecessor(d, predecessor)
		if err != nil {
			return result, err
		}
	} else {
		if !old.Complete || !unboundPredecessor(old) {
			return result, fmt.Errorf("finish the pending installation before removal")
		}
		observation, e := inspect(ctx, paths.Installation)
		if e != nil {
			return result, e
		}
		stopped, err = activityStoppedPredecessor(d, old)
		if err != nil {
			return result, err
		}
		mode := "verification"
		if stopped {
			mode = "uninstalled"
		}
		if !observation.Unbound || observation.Phase != "complete" || observation.Manifest.Mode != mode || protocol.Hash(observation.Manifest) != old.Children[0].Receipt {
			return result, fmt.Errorf("dependent consumers or changed gateway require the domain removal workflow")
		}
		expected = old.Children[0].Receipt
		if err = verifyActivityReceipt(d, old, observation.Manifest); err != nil {
			return result, err
		}
		if err = verifyReleaseReceipt(d, old, observation.Manifest); err != nil {
			return result, err
		}
		cleanupLED = observation.LEDs && !stopped
		pins.Action = "uninstall"
		pins.DesiredHash = r.DesiredHash
		pins.DesiredScope = r.DesiredScope
		pins.Previous = old.Digest()
		pins.PreviousOperation = old.ID
		pins.Installations = map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}
		pins.Artifacts = map[string]string{"helper": r.HelperSHA256}
	}
	step := &notificationUninstallStep{root: paths.Installation, expected: expected, inspect: inspect, remove: remove, stopped: stopped}
	steps := []LifecycleStep{step}
	if cleanupLED {
		cleanup := &notificationRemovalLEDStep{directory: d, root: paths.Installation, expected: expected, inspect: inspect, disable: disable}
		step.cleaned = cleanup
		steps = []LifecycleStep{cleanup, step}
	}
	if record, e := (JournalCoordinator{Directory: d}).Apply(ctx, pins, steps); e != nil {
		if cleanupLED && len(record.Children) == 2 && !record.Children[0].Complete {
			return result, ledOutputPendingError{Server: r.Server, Action: "uninstall", Cause: e}
		}
		return result, notificationRemovalPendingError{Server: r.Server, Cause: e}
	}
	result.State = "uninstalled"
	result.Code = "ok"
	result.Identity = &step.manifest.Identity
	result.Next = "service removed; gateway state, credentials and audit history retained"
	if step.manifest.FreshSignal != nil {
		result.Next = "gateway and Signal services removed; account, inbox, credentials and audit history retained"
	}
	if cleanupLED {
		result.Next += "; owned LED output cleared, unresolved faults and daemon retained"
	}
	return result, nil
}

type notificationRemovalPendingError struct {
	Server string
	Cause  error
}

func (e notificationRemovalPendingError) Error() string { return e.Cause.Error() }
func (e notificationRemovalPendingError) Unwrap() error { return e.Cause }

// Only the documented cleanup/removal sequence may have multiple children.
func canonicalNotificationReceipt(j OperationJournal) (string, bool) {
	if commandJournal(j) {
		return j.Children[1].Receipt, true
	}
	if len(j.Children) == 2 && j.Pins.Action == "uninstall" && j.Children[0].Name == "notification-remove-led-output" && j.Children[1].Name == "notification-uninstall" {
		return j.Children[1].Receipt, true
	}
	if len(j.Children) != 1 {
		return "", false
	}
	switch j.Children[0].Name {
	case "notification-shared-activity", "notification-command-provider", "notification-command-destination", "notification-command-routes", "notification-command-derived-freshness", "notification-command-report-route", "notification-command-report-route-declaration", "notification-delivery-report-route", "notification-delivery-report-route-declaration", "notification-command-producer", "notification-command-declaration", "notification-command-freshness", "notification-command-limits", "notification-command-requalification", "notification-command-delivery", "notification-delivery-derived-freshness", "notification-delivery-declaration", "notification-delivery-provider-enrollment", "notification-delivery-destination", "notification-delivery-routes", "notification-delivery-producer", "notification-delivery-limits", "notification-delivery-providers", "notification-requalification", "notification-shared-report-route", "notification-shared-derived-freshness", "notification-shared-command-activity", "notification-shared-command-policy", "notification-shared-commands", "notification-shared-routes", "notification-shared-producer", "notification-shared-providers", "notification-shared-freshness", "notification-shared-limits", "notification-delivery-led-output", "notification-command-enrollment", "notification-command-permissions", "notification-command-activity", "notification-adopt", "notification-bootstrap", "notification-uninstall", "notification-enrollment", "notification-mapping", "notification-remapping", "notification-enrolled-led-output", "notification-delivery", "notification-led-output", "notification-activity", "notification-artifact", "notification-command-draft", "notification-limits", "notification-routes", "notification-providers":
		return j.Children[0].Receipt, true
	}
	return "", false
}

type notificationUninstallStep struct {
	stopped        bool // A completed YAML disable already removed the gateway service.
	cleaned        *notificationRemovalLEDStep
	root, expected string
	inspect        func(context.Context, string) (install.InstallationObservation, error)
	remove         func(context.Context, string, string) (install.ManagedManifest, error)
	manifest       install.ManagedManifest
	inspectSignal  func(context.Context, install.FreshSignalReference) (install.FreshSignalManifest, bool, error)
	removeSignal   func(context.Context, install.FreshSignalReference) (install.FreshSignalManifest, error)
}

func (s *notificationUninstallStep) Name() string { return "notification-uninstall" }
func (s *notificationUninstallStep) expectedManifest() string {
	if s.cleaned != nil {
		return protocol.Hash(s.cleaned.manifest)
	}
	return s.expected
}
func (s *notificationUninstallStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	observed, err := s.inspect(ctx, s.root)
	if err != nil {
		return "", false, err
	}
	if observed.Phase != "complete" {
		return "", false, nil
	}
	if observed.Manifest.Mode == "uninstalled" {
		before := observed.Manifest
		if !s.stopped {
			before.Mode = "verification"
		}
		if protocol.Hash(before) != s.expectedManifest() {
			return "", false, fmt.Errorf("uninstall changed retained gateway identity or files")
		}
		s.manifest = observed.Manifest
		if s.manifest.FreshSignal != nil {
			inspect := s.inspectSignal
			if inspect == nil {
				inspect = install.InspectFreshSignalRemoval
			}
			_, complete, err := inspect(ctx, *s.manifest.FreshSignal)
			if err != nil || !complete {
				return "", false, err
			}
		}
		return protocol.Hash(s.manifest), true, nil
	}
	if protocol.Hash(observed.Manifest) != s.expectedManifest() {
		return "", false, fmt.Errorf("gateway changed before removal")
	}
	return "", false, nil
}
func (s *notificationUninstallStep) Run(ctx context.Context, op string) error {
	m, err := s.remove(ctx, s.root, s.expectedManifest())
	if err != nil {
		return err
	}
	if m.Mode != "uninstalled" {
		return fmt.Errorf("gateway removal did not complete")
	}
	before := m
	if !s.stopped {
		before.Mode = "verification"
	}
	if protocol.Hash(before) != s.expectedManifest() {
		return fmt.Errorf("gateway removal receipt changed")
	}
	if m.FreshSignal != nil {
		remove := s.removeSignal
		if remove == nil {
			remove = install.UninstallFreshSignal
		}
		if _, err = remove(ctx, *m.FreshSignal); err != nil {
			return err
		}
	}
	return nil
}
