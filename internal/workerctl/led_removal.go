package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// LED cleanup is a prerequisite child of removal, not a hidden YAML boolean
// edit. Its canonical receipt survives gateway removal and continues to pin the
// disabled output disposition and the retained incident ledger.
type notificationRemovalLEDStep struct {
	directory      *safefs.Directory
	root, expected string
	inspect        func(context.Context, string) (install.InstallationObservation, error)
	disable        func(context.Context, string, string, bool) (install.ManagedManifest, error)
	manifest       install.ManagedManifest
}

func (s *notificationRemovalLEDStep) Name() string { return "notification-remove-led-output" }

func (s *notificationRemovalLEDStep) before(op string) (install.ManagedManifest, error) {
	var m install.ManagedManifest
	if !safefs.ValidID(op, "n") {
		return m, fmt.Errorf("invalid removal cleanup operation")
	}
	err := s.directory.ReadJSON("removal-led-"+op+".json", &m)
	if err == nil && (protocol.Hash(m) != s.expected || m.Mode != "verification" || m.DeliveryOwnerHash != "" || m.RecoveryCommitHash != "") {
		err = fmt.Errorf("removal LED predecessor differs from reviewed authority")
	}
	return m, err
}

func (s *notificationRemovalLEDStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	m, err := s.before(op)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	observed, e := s.inspect(ctx, s.root)
	if e != nil {
		return "", false, e
	}
	if err != nil {
		if observed.Phase != "complete" || protocol.Hash(observed.Manifest) != s.expected {
			return "", false, fmt.Errorf("LED output changed before removal intent")
		}
		return "", false, nil
	}
	if observed.Phase != "complete" {
		return "", false, nil
	}
	actual := observed.Manifest
	if actual.Mode != "verification" && actual.Mode != "uninstalled" {
		return "", false, fmt.Errorf("LED cleanup cannot adopt delivery authority")
	}
	actual.Mode = "verification"
	if actual.LEDOutput == nil || !actual.LEDOutput.Disabled {
		if protocol.Hash(actual) == s.expected {
			return "", false, nil
		}
		return "", false, fmt.Errorf("removal LED disposition differs")
	}
	m.LEDOutput = actual.LEDOutput
	if !safefs.ValidID(actual.LEDOutput.Operation, "n") || protocol.Hash(m) != protocol.Hash(actual) {
		return "", false, fmt.Errorf("LED cleanup changed gateway identity or runtime")
	}
	s.manifest = actual
	return protocol.Hash(actual), true, nil
}

func (s *notificationRemovalLEDStep) Run(ctx context.Context, op string) error {
	_, err := s.before(op)
	if errors.Is(err, os.ErrNotExist) {
		observed, e := s.inspect(ctx, s.root)
		if e != nil {
			return e
		}
		m := observed.Manifest
		if !observed.LEDs || !observed.Unbound || observed.Phase != "complete" || protocol.Hash(m) != s.expected || m.Mode != "verification" || m.DeliveryOwnerHash != "" || m.RecoveryCommitHash != "" {
			return fmt.Errorf("LED removal requires exact unbound verification authority")
		}
		if err = s.directory.WriteJSON("removal-led-"+op+".json", m, false); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, err = s.disable(ctx, s.root, s.expected, true)
	return err
}

func verifyRemovalLEDReceipt(ctx context.Context, d *safefs.Directory, j OperationJournal, root string, inspect func(context.Context, string) (install.InstallationObservation, error)) error {
	if len(j.Children) != 2 || j.Pins.Action != "uninstall" {
		return nil
	}
	if _, valid := canonicalNotificationReceipt(j); !valid {
		return fmt.Errorf("removal child sequence changed")
	}
	var before OperationJournal
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &before); err != nil {
		return err
	}
	if validateOperationJournal(before) != nil || !before.Complete || before.Digest() != j.Pins.Previous || len(before.Children) != 1 {
		return fmt.Errorf("removal predecessor changed")
	}
	s := notificationRemovalLEDStep{directory: d, root: root, expected: before.Children[0].Receipt, inspect: inspect}
	receipt, complete, err := s.Inspect(ctx, j.Children[0].Operation)
	if err != nil {
		return err
	}
	if !complete || receipt != j.Children[0].Receipt {
		return fmt.Errorf("removal LED cleanup receipt unavailable or changed")
	}
	return nil
}
