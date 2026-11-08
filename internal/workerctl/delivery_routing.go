package workerctl

import (
	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

func reportRoutePolicy(kind string) bool {
	return kind == "report-route" || kind == "report-route-declaration" || kind == "command-report-route" || kind == "command-report-route-declaration"
}
func derivedFreshnessPolicy(kind string) bool {
	return kind == "derived-freshness" || kind == "command-derived-freshness"
}
func defaultCommandDeliveryRoutes(helper string) deliveryProviderLifecycle {
	l := defaultCommandDeliveryLimits(helper)
	l.kind, l.review, l.rotate, l.inspectRotate, l.scope = "command-routes", install.ReviewCommandDeliveryRoutes, install.ChangePausedCommandDeliveryRoutes, install.InspectPausedCommandDeliveryRoutes, backup.ReconcileDeliveryRoutesScope
	return l
}
func defaultCommandDerivedFreshness(helper string) deliveryProviderLifecycle {
	l := defaultCommandDeliveryFreshness(helper)
	l.kind, l.scope = "command-derived-freshness", backup.ReconcileCommandDerivedFreshnessScope
	return l
}
func defaultDeliveryReportRoute(helper string, commands, declaration bool) deliveryProviderLifecycle {
	l := defaultDeliveryProducer(helper)
	l.kind = "report-route"
	if commands {
		l = defaultCommandDeliveryProducer(helper)
		l.kind = "command-report-route"
	}
	l.scope = backup.ReconcileDeliveryReportRouteScope
	if declaration {
		l.kind += "-declaration"
		l.review = install.ReviewDeliveryDeclaration
		if commands {
			l.review = install.ReviewCommandDeliveryDeclaration
		}
	}
	return l
}
func compileCommandDeliveryRoutes(r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal, input install.RuntimeInput, raw []byte, reservation backup.ProducerReservation) (CompiledNotification, *notificationInputPins, *notificationInputPins, error) {
	compiled, b, a, err := compileDeliveryLimits(r, p, d, j, input, raw, reservation)
	if err != nil {
		return compiled, b, a, err
	}
	next := input
	next.Worker.Routes = compiled.Input.Worker.Routes
	compiled.Input = next
	updated, err := next.Bytes()
	if err == nil {
		a.Runtime = inputDigest(updated)
	}
	return compiled, b, a, err
}
