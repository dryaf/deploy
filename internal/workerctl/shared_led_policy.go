package workerctl

import (
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

// Output permission is a separate domain record. Policy compilation retains
// the installed endpoint and layout when output is disabled.
func retainedLEDDeclaration(d config.DesiredWorkers, input install.RuntimeInput) config.DesiredWorkers {
	if d.Notification != nil && input.LEDSocket != "" {
		n := *d.Notification
		d.Notification = &n
		n.StatusLEDs.Enabled = true
	}
	return d
}

// A policy journal can follow mapping/output records. Verify the complete
// declaration against its retained runtime and enrollment before qualification
// or admission, instead of treating the latest operation's name as proof.
func verifySharedLEDDeclaration(r ControlRequest, paths ResolvedPaths, d *safefs.Directory, j OperationJournal) error {
	v, err := readSharedLimitsIntent(d, j)
	if err != nil {
		return err
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(v.AfterRuntime, &input, maxControlFrame); err != nil {
		return err
	}
	var enrollment consumerEnrollmentIntent
	if err = d.ReadJSON("consumer-enrollment.json", &enrollment); err != nil {
		return err
	}
	if err = enrollment.Reservation.Validate(enrollment.Reservation.Producer.ID); err != nil {
		return err
	}
	return verifyReservedConsumerInput(r, paths, d, j, input, enrollment.Reservation)
}
