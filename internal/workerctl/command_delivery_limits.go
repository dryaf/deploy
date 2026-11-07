package workerctl

import (
	"context"
	"fmt"
	"slices"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

func commandLimitsJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-command-limits"
}
func deliveryPolicyCandidate(j OperationJournal, kind string) bool {
	if kind == "command-limits" {
		return admittedCommandHistory(j)
	}
	return requalificationCandidate(j)
}
func validateDeliveryPolicyBefore(m install.ManagedManifest, kind string) error {
	if kind == "command-limits" {
		return validateCommandRequalificationBefore(m)
	}
	return validateRequalificationBefore(m)
}
func validateDeliveryPolicyPause(b, a install.ManagedManifest, kind string) error {
	if kind == "command-limits" {
		return validateCommandRequalificationManifest(b, a)
	}
	return validateRequalificationManifest(b, a)
}
func readDeliveryPolicyDeclaration(d *safefs.Directory, j OperationJournal, kind string) (config.DesiredWorkers, error) {
	if kind == "command-limits" {
		return readCommandDeclaration(d, j)
	}
	return readDeclaration(d, j.Pins.DesiredHash, j.Pins.DesiredScope)
}
func defaultCommandDeliveryLimits(helper string) deliveryProviderLifecycle {
	l := defaultDeliveryLimits(helper)
	l.kind = "command-limits"
	l.review = install.ReviewCommandDeliveryLimits
	l.rotate = install.ChangePausedCommandDeliveryLimits
	l.inspectRotate = install.InspectPausedCommandDeliveryLimits
	l.inputs = func(ctx context.Context, r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal) error {
		if err := verifyLEDOutputInputs(r, p, d, r.Desired); err != nil {
			return err
		}
		return nil
	}
	l.commandReceiver = func(ctx context.Context, d *safefs.Directory, j OperationJournal) error {
		_, err := retainedCommandReceiver(ctx, d, j, defaultNotificationCommands())
		return err
	}
	// These transitions preserve the retained native service. No native repair
	// may run before command authority and the changed YAML have been reviewed.
	l.nativeResume = nil
	return l
}
func deliveryPolicyRuntimeOrigin(d *safefs.Directory, j OperationJournal, kind string) (OperationJournal, error) {
	if kind != "command-limits" {
		return requalificationEnrollment(d, j)
	}
	for depth := 0; depth < 128; depth++ {
		var err error
		switch {
		case commandLimitsJournal(j):
			return j, nil
		case commandPauseJournal(j) || commandDeliveryJournal(j):
			j, err = commandRenewalPredecessor(d, j)
		case commandActivityJournal(j):
			j, err = commandActivityPredecessor(d, j)
		case deliveryOutputJournal(j):
			j, err = deliveryOutputPredecessor(d, j)
		case commandJournal(j):
			j, err = commandPredecessor(d, j)
		default:
			return requalificationEnrollment(d, j)
		}
		if err != nil {
			return j, err
		}
	}
	return j, fmt.Errorf("command budget runtime history exceeds review bound")
}
func compileCommandDeliveryLimits(r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal, input install.RuntimeInput, raw []byte, reservation backup.ProducerReservation) (CompiledNotification, *notificationInputPins, *notificationInputPins, error) {
	compiled, b, a, err := compileDeliveryLimits(r, p, d, j, input, raw, reservation)
	if err != nil {
		return compiled, b, a, err
	}
	// Permissions may have been narrowed through SQL command history. Preserve
	// that original runtime enrollment; only replace the declared global budgets.
	next := input
	next.Worker.MaxMessages = compiled.Input.Worker.MaxMessages
	next.Worker.MaxPayloadBytes = compiled.Input.Worker.MaxPayloadBytes
	next.Worker.MaxDatabaseBytes = compiled.Input.Worker.MaxDatabaseBytes
	next.Worker.MinFreeBytes = compiled.Input.Worker.MinFreeBytes
	next.Worker.ReceiptAuditDays = compiled.Input.Worker.ReceiptAuditDays
	next.Worker.Producers = slices.Clone(input.Worker.Producers)
	for i := range next.Worker.Producers {
		for _, p := range compiled.Input.Worker.Producers {
			if p.Producer == next.Worker.Producers[i].Producer {
				next.Worker.Producers[i].MaxMessages = p.MaxMessages
			}
		}
	}
	compiled.Input = next
	updated, err := next.Bytes()
	if err != nil {
		return compiled, b, a, err
	}
	a.Runtime = inputDigest(updated)
	return compiled, b, a, nil
}

// The domain admission separately proves the exact paused manifest receipt.
// Here we prove that its runtime and retained command authority are unchanged.
func commandRenewalPausedManifest(d *safefs.Directory, j OperationJournal, current install.ManagedManifest) (install.ManagedManifest, error) {
	if current.Commands != nil {
		return current, fmt.Errorf("delivery renewal changed command admission")
	}
	paused := current
	paused.Mode, paused.DeliveryActivation, paused.Commands = "verification", nil, nil
	if commandLimitsJournal(j) {
		v, err := readDeliveryProviderIntent(d, j)
		if err != nil {
			return paused, err
		}
		if current.Commands != nil {
			return paused, fmt.Errorf("budget renewal changed command admission")
		}
		return paused, validateDeliveryProviderManifest(v, paused)
	}
	var v requalificationIntent
	if err := d.ReadJSON("requalification-"+j.Children[0].Operation+".json", &v); err != nil {
		return paused, err
	}
	return paused, validateCommandRequalificationManifest(v.Before, paused)
}
