package workerctl

import (
	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

func commandDeclarationJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && (j.Children[0].Name == "notification-command-declaration" || j.Children[0].Name == "notification-command-report-route-declaration")
}
func isDeliveryDeclaration(kind string) bool {
	return kind == "declaration" || kind == "command-declaration" || kind == "report-route-declaration" || kind == "command-report-route-declaration"
}
func defaultCommandDeliveryProducer(helper string) deliveryProviderLifecycle {
	l := defaultCommandDeliveryFreshness(helper)
	l.kind = "command-producer"
	l.review = install.ReviewCommandDeliveryProducerPolicy
	l.rotate = install.ChangePausedCommandDeliveryProducerPolicy
	l.inspectRotate = install.InspectPausedCommandDeliveryProducerPolicy
	return l
}
func defaultCommandDeliveryDeclaration(helper string) deliveryProviderLifecycle {
	l := defaultCommandDeliveryProducer(helper)
	l.kind = "command-declaration"
	l.review = install.ReviewCommandDeliveryDeclaration
	l.scope = backup.ReconcileDeliveryDeclarationScope
	return l
}
func compileCommandDeliveryProducer(r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal, input install.RuntimeInput, raw []byte, reservation backup.ProducerReservation) (CompiledNotification, *notificationInputPins, *notificationInputPins, error) {
	compiled, b, a, err := compileDeliveryLimits(r, p, d, j, input, raw, reservation)
	if err != nil {
		return compiled, b, a, err
	}
	next := input
	next.Worker.Producers = compiled.Input.Worker.Producers
	compiled.Input = next
	updated, err := next.Bytes()
	if err != nil {
		return compiled, b, a, err
	}
	a.Runtime = inputDigest(updated)
	return compiled, b, a, nil
}
