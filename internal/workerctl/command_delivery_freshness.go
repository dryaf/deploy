package workerctl

import (
	"slices"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

func isCommandDeliveryPolicy(kind string) bool {
	return commandProviderPolicy(kind) || kind == "command-routes" || kind == "command-derived-freshness" || kind == "command-report-route" || kind == "command-report-route-declaration" || kind == "command-limits" || kind == "command-freshness" || kind == "command-producer" || kind == "command-declaration"
}
func defaultCommandDeliveryFreshness(helper string) deliveryProviderLifecycle {
	l := defaultCommandDeliveryLimits(helper)
	l.kind = "command-freshness"
	l.review = install.ReviewCommandDeliveryFreshness
	l.rotate = install.ChangePausedCommandDeliveryFreshness
	l.inspectRotate = install.InspectPausedCommandDeliveryFreshness
	l.scope = backup.ReconcileDeliveryProducerScope
	return l
}
func compileCommandDeliveryFreshness(r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal, input install.RuntimeInput, raw []byte, reservation backup.ProducerReservation) (CompiledNotification, *notificationInputPins, *notificationInputPins, error) {
	compiled, b, a, err := compileDeliveryLimits(r, p, d, j, input, raw, reservation)
	if err != nil {
		return compiled, b, a, err
	}
	next := input
	next.Worker.Producers = slices.Clone(input.Worker.Producers)
	for i := range next.Worker.Producers {
		for _, revised := range compiled.Input.Worker.Producers {
			if revised.Producer == next.Worker.Producers[i].Producer {
				next.Worker.Producers[i].MaxSilenceSeconds = revised.MaxSilenceSeconds
				next.Worker.Producers[i].MaxEvidenceSeconds = revised.MaxEvidenceSeconds
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
