package workerctl

import (
	"context"
	"fmt"
	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"slices"
)

func defaultDeliveryLimits(helper string) deliveryProviderLifecycle {
	l := defaultDeliveryProviders(helper)
	l.kind = "limits"
	l.review = install.ReviewDeliveryLimits
	l.rotate = install.ChangePausedDeliveryLimits
	l.inspectRotate = install.InspectPausedDeliveryLimits
	l.scope = backup.ReconcileDeliveryLimitsScope
	return l
}
func deliveryProviderKind(j OperationJournal) string {
	if len(j.Children) == 1 {
		switch j.Children[0].Name {
		case "notification-command-provider":
			return "command-provider"
		case "notification-command-destination":
			return "command-destination"
		case "notification-command-routes":
			return "command-routes"
		case "notification-command-derived-freshness":
			return "command-derived-freshness"
		case "notification-command-report-route":
			return "command-report-route"
		case "notification-command-report-route-declaration":
			return "command-report-route-declaration"
		case "notification-delivery-report-route":
			return "report-route"
		case "notification-delivery-report-route-declaration":
			return "report-route-declaration"
		}
	}

	if commandDeclarationJournal(j) {
		return "command-declaration"
	}
	if commandDeliveryPolicyJournal(j) && j.Children[0].Name == "notification-command-producer" {
		return "command-producer"
	}
	if commandDeliveryPolicyJournal(j) && j.Children[0].Name == "notification-command-freshness" {
		return "command-freshness"
	}
	if commandDeliveryPolicyJournal(j) {
		return "command-limits"
	}
	if deliveryProviderJournal(j) && j.Children[0].Name == "notification-delivery-derived-freshness" {
		return "derived-freshness"
	}
	if deliveryProviderJournal(j) && j.Children[0].Name == "notification-delivery-declaration" {
		return "declaration"
	}
	if deliveryProviderJournal(j) && j.Children[0].Name == "notification-delivery-provider-enrollment" {
		return "provider-enrollment"
	}
	if deliveryProviderJournal(j) && j.Children[0].Name == "notification-delivery-destination" {
		return "destination"
	}
	if deliveryProviderJournal(j) && j.Children[0].Name == "notification-delivery-routes" {
		return "routes"
	}
	if deliveryProviderJournal(j) && j.Children[0].Name == "notification-delivery-producer" {
		return "producer"
	}
	if deliveryProviderJournal(j) && j.Children[0].Name == "notification-delivery-limits" {
		return "limits"
	}
	return ""
}
func validateDeliveryPolicyDeclaration(before, after config.DesiredWorkers, kind string) error {
	var err error
	before, err = deliveryBackupBefore(before, after, kind)
	if err != nil {
		return err
	}
	if commandProviderPolicy(kind) {
		return config.ValidateCommandDeliveryProviderDeclaration(before, after)
	}
	if kind == "command-derived-freshness" {
		return config.ValidateCommandDerivedFreshnessDeclaration(before, after)
	}
	if kind == "command-routes" {
		return config.ValidateCommandDeliveryRoutesDeclaration(before, after)
	}
	if reportRoutePolicy(kind) {
		if isCommandDeliveryPolicy(kind) {
			return config.ValidateCommandDeliveryReportRouteDeclaration(before, after)
		}
		return config.ValidateDeliveryReportRouteDeclaration(before, after)
	}
	if kind == "derived-freshness" {
		return config.ValidateDerivedFreshnessDeclaration(before, after)
	}
	if kind == "declaration" {
		return config.ValidateDeliveryDeclarationChange(before, after)
	}
	if kind == "provider-enrollment" {
		return config.ValidateDeliveryProviderEnrollmentDeclaration(before, after)
	}
	if kind == "routes" {
		return config.ValidateDeliveryRoutesDeclaration(before, after)
	}
	if kind == "command-producer" || kind == "command-declaration" {
		return config.ValidateCommandDeliveryProducerDeclaration(before, after)
	}
	if kind == "producer" {
		return config.ValidateDeliveryProducerDeclaration(before, after)
	}
	if kind == "command-freshness" {
		return config.ValidateCommandDeliveryFreshnessDeclaration(before, after)
	}
	if kind == "limits" || kind == "command-limits" {
		return config.ValidateDeliveryLimitsDeclaration(before, after)
	}
	if kind != "" && kind != "destination" {
		return fmt.Errorf("unknown production policy change")
	}
	return config.ValidateDeliveryProviderDeclaration(before, after)
}
func compileDeliveryLimits(r ControlRequest, p ResolvedPaths, d *safefs.Directory, prior OperationJournal, input install.RuntimeInput, raw []byte, reservation backup.ProducerReservation) (CompiledNotification, *notificationInputPins, *notificationInputPins, error) {
	var zero CompiledNotification
	pins, _, err := retainedSharedProviderInputs(d, prior, prior.Pins.DesiredHash)
	if err != nil {
		return zero, nil, nil, err
	}
	if pins == nil {
		pins = &notificationInputPins{}
		if err = d.ReadJSON("notification-inputs.json", pins); err != nil {
			return zero, nil, nil, err
		}
	}
	before := *pins
	before.Runtime = inputDigest(raw)
	if err = verifyProviderRuntimeInputs(raw, before); err != nil {
		return zero, nil, nil, err
	}
	compiled, err := compileReservedNotification(retainedLEDDeclaration(r.Desired, input), r.Facts, p, input, reservation)
	if err != nil {
		return zero, nil, nil, err
	}
	retainsReportRoute := false
	for _, producer := range compiled.Input.Worker.Producers {
		if producer.Producer == reservation.Producer && slices.Contains(producer.Routes, r.Desired.Backup.Notifications.Route) {
			retainsReportRoute = true
		}
	}
	if !retainsReportRoute {
		return zero, nil, nil, fmt.Errorf("retain the active backup report route while changing producer policy")
	}
	preserveRuntimeRouteLabels(input, &compiled.Input)
	next, err := compiled.Input.Bytes()
	if err != nil {
		return zero, nil, nil, err
	}
	after := before
	after.Runtime = inputDigest(next)
	return compiled, &before, &after, nil
}
func reconcileDeliveryLimits(ctx context.Context, r ControlRequest, p ResolvedPaths, helper string, l deliveryProviderLifecycle) (ControlResult, bool, error) {
	l.kind = "limits"
	return reconcileDeliveryProviders(ctx, r, p, helper, l)
}

func defaultDeliveryProducer(helper string) deliveryProviderLifecycle {
	l := defaultDeliveryProviders(helper)
	l.kind = "producer"
	l.review = install.ReviewDeliveryProducerPolicy
	l.rotate = install.ChangePausedDeliveryProducerPolicy
	l.inspectRotate = install.InspectPausedDeliveryProducerPolicy
	l.scope = backup.ReconcileDeliveryProducerScope
	return l
}
func reconcileDeliveryProducer(ctx context.Context, r ControlRequest, p ResolvedPaths, helper string, l deliveryProviderLifecycle) (ControlResult, bool, error) {
	l.kind = "producer"
	return reconcileDeliveryProviders(ctx, r, p, helper, l)
}

func defaultDeliveryRoutes(helper string) deliveryProviderLifecycle {
	l := defaultDeliveryProviders(helper)
	l.kind = "routes"
	l.review = install.ReviewDeliveryRoutes
	l.rotate = install.ChangePausedDeliveryRoutes
	l.inspectRotate = install.InspectPausedDeliveryRoutes
	l.scope = backup.ReconcileDeliveryRoutesScope
	return l
}
func reconcileDeliveryRoutes(ctx context.Context, r ControlRequest, p ResolvedPaths, helper string, l deliveryProviderLifecycle) (ControlResult, bool, error) {
	l.kind = "routes"
	return reconcileDeliveryProviders(ctx, r, p, helper, l)
}

func defaultDeliveryDestination(helper string) deliveryProviderLifecycle {
	l := defaultDeliveryProviders(helper)
	l.kind = "destination"
	l.review = install.ReviewDeliveryDestination
	l.rotate = install.ChangePausedDeliveryDestination
	l.inspectRotate = install.InspectPausedDeliveryDestination
	l.scope = backup.ReconcileDeliveryDestinationScope
	return l
}

func defaultDeliveryProviderEnrollment(helper string) deliveryProviderLifecycle {
	l := defaultDeliveryProviders(helper)
	l.kind = "provider-enrollment"
	l.review = install.ReviewDeliveryProviderEnrollment
	l.rotate = install.ChangePausedDeliveryProviderEnrollment
	l.inspectRotate = install.InspectPausedDeliveryProviderEnrollment
	l.scope = backup.ReconcileDeliveryProviderEnrollmentScope
	return l
}

func defaultDeliveryDeclaration(helper string) deliveryProviderLifecycle {
	l := defaultDeliveryProviders(helper)
	l.kind = "declaration"
	l.review = install.ReviewDeliveryDeclaration
	l.scope = backup.ReconcileDeliveryDeclarationScope
	return l
}

func deliveryDeclarationJournal(j OperationJournal) bool {
	return deliveryProviderJournal(j) && isDeliveryDeclaration(deliveryProviderKind(j))
}

func reviewDeliveryDeclarationRuntime(r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal) (bool, error) {
	return reviewPolicyDeclarationRuntime(r, p, d, j, "declaration")
}
func reviewPolicyDeclarationRuntime(r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal, kind string) (bool, error) {
	origin, err := deliveryPolicyRuntimeOrigin(d, j, kind)
	if err != nil {
		return false, err
	}
	raw, err := retainedEnrolledRuntime(d, p, origin)
	if err != nil {
		return false, err
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return false, err
	}
	var enrollment consumerEnrollmentIntent
	if err = d.ReadJSON("consumer-enrollment.json", &enrollment); err != nil {
		return false, err
	}
	compile := compileDeliveryLimits
	if isCommandDeliveryPolicy(kind) {
		compile = compileCommandDeliveryProducer
	}
	compiled, _, _, err := compile(r, p, d, j, input, raw, enrollment.Reservation)
	if err != nil {
		return false, err
	}
	before, err := input.Resolve()
	if err != nil {
		return false, err
	}
	after, err := compiled.Input.Resolve()
	if err != nil {
		return false, err
	}
	return protocol.Hash(before) == protocol.Hash(after), nil
}

func defaultDeliveryDerivedFreshness(helper string) deliveryProviderLifecycle {
	l := defaultDeliveryProducer(helper)
	l.kind = "derived-freshness"
	l.scope = backup.ReconcileDeliveryDerivedFreshnessScope
	return l
}
