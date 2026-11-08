package workerctl

import (
	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

func commandProviderPolicy(kind string) bool {
	return kind == "command-provider" || kind == "command-destination"
}
func defaultCommandDeliveryProviders(helper string, destination bool) deliveryProviderLifecycle {
	l := defaultCommandDeliveryLimits(helper)
	l.kind, l.review, l.rotate, l.inspectRotate, l.scope = "command-provider", install.ReviewCommandDeliveryProviders, install.ChangePausedCommandDeliveryProviders, install.InspectPausedCommandDeliveryProviders, backup.ReconcileDeliveryProviderScope
	if destination {
		l.kind, l.review, l.rotate, l.inspectRotate, l.scope = "command-destination", install.ReviewCommandDeliveryDestination, install.ChangePausedCommandDeliveryDestination, install.InspectPausedCommandDeliveryDestination, backup.ReconcileDeliveryDestinationScope
	}
	return l
}
func compileCommandDeliveryProviders(r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal, input install.RuntimeInput, raw []byte, reservation backup.ProducerReservation) (CompiledNotification, *notificationInputPins, *notificationInputPins, error) {
	compiled, b, a, err := compileSharedProviders(r, p, d, j, input, raw, reservation)
	if err != nil {
		return compiled, b, a, err
	}
	next := input
	next.Providers = compiled.Input.Providers
	next.Worker.Routes = compiled.Input.Worker.Routes
	if input.Telegram != nil {
		account := *input.Telegram
		account.TokenFile = next.Providers["telegram"].TokenFile
		next.Telegram = &account
	}
	compiled.Input = next
	body, err := next.Bytes()
	if err == nil {
		a.Runtime = inputDigest(body)
	}
	return compiled, b, a, err
}
