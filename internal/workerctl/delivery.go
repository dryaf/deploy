package workerctl

import (
	"context"
	"fmt"
	"maps"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type notificationDeliveryLifecycle struct {
	scopeInspect func(string, string, string, string) (string, bool, error)
	review       func(context.Context, string, string) (install.DeliveryAdmissionRequest, bool, error)
	inspect      func(context.Context, install.DeliveryAdmissionRequest) (install.ManagedManifest, bool, error)
	ensure       func(context.Context, install.DeliveryAdmissionRequest) (install.ManagedManifest, error)
	backupStatus func(context.Context, string) (backup.SetupObservation, error)
	connect      func(context.Context, string, string, string) error
}

func defaultNotificationDelivery() notificationDeliveryLifecycle {
	return notificationDeliveryLifecycle{backup.InspectRetainedDeliveryPolicyScope, install.ReviewDeliveryAdmission, install.InspectDeliveryAdmission, install.EnsureDeliveryAdmission, backup.InspectSetup, backup.ConnectSharedReporting}
}

func verifyAdmittedConsumerInput(r ControlRequest, paths ResolvedPaths, d *safefs.Directory, current OperationJournal, input install.RuntimeInput) error {
	enrolled, err := requalificationEnrollment(d, current)
	if err != nil {
		return err
	}
	if validateOperationJournal(enrolled) != nil || !enrolled.Complete || !enrolledNotificationJournal(enrolled) {
		return fmt.Errorf("admitted delivery lost enrollment history")
	}
	b := r.Desired.Backup
	if b == nil || b.Notifications.Worker != r.Server {
		return fmt.Errorf("admitted delivery lost its local backup declaration")
	}
	reservation, present, err := backup.InspectReservedProducer(r.Facts.Home, r.DesiredHash, paths.Installation, b.Notifications.Route)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("admitted delivery lost its backup reservation")
	}
	return verifyReservedConsumerInput(r, paths, d, enrolled, input, reservation)
}

func verifyReservedConsumerInput(r ControlRequest, paths ResolvedPaths, d *safefs.Directory, enrolled OperationJournal, input install.RuntimeInput, reservation backup.ProducerReservation) error {
	renewed := requalificationJournal(enrolled)
	if renewed {
		var err error
		enrolled, err = requalificationEnrollment(d, enrolled)
		if err != nil {
			return err
		}
	}
	selected, rawProviders, err := retainedSharedProviderInputs(d, enrolled, enrolled.Pins.DesiredHash)
	if err != nil {
		return err
	}
	if selected != nil {
		var latest install.RuntimeInput
		if err = safefs.DecodeJSON(rawProviders, &latest, maxControlFrame); err != nil {
			return err
		}
		input.Providers = latest.Providers
	}
	desired := r.Desired
	if (renewed || deliveryProviderJournal(enrolled) || enrolledOutputJournal(enrolled) || sharedLimitsJournal(enrolled) || mappingJournal(enrolled)) && input.LEDSocket != "" {
		n := *desired.Notification
		desired.Notification = &n
		n.StatusLEDs.Enabled = true
	}
	compiled, err := compileReservedNotification(desired, r.Facts, paths, input, reservation)
	if err != nil {
		return err
	}
	preserveRuntimeRouteLabels(input, &compiled.Input)
	raw, err := compiled.Input.Bytes()
	if err != nil {
		return err
	}
	for depth := 0; mappingJournal(enrolled) || enrolledOutputJournal(enrolled) || sharedLimitsJournal(enrolled) || deliveryProviderJournal(enrolled); depth++ {
		if depth >= 128 {
			return fmt.Errorf("LED mapping history exceeds review bound")
		}
		if deliveryProviderJournal(enrolled) {
			v, e := readDeliveryProviderIntent(d, enrolled)
			if e != nil {
				return e
			}
			if string(raw) != string(v.AfterRuntime) {
				return fmt.Errorf("declared provider runtime differs from completed rotation")
			}
			raw = v.BeforeRuntime
			enrolled, err = deliveryProviderPredecessor(d, enrolled)
			if err != nil {
				return err
			}
			enrolled, err = requalificationEnrollment(d, enrolled)
			if err != nil {
				return err
			}
			continue
		}
		if sharedLimitsJournal(enrolled) {
			v, e := readSharedLimitsIntent(d, enrolled)
			if e != nil {
				return e
			}
			if string(raw) != string(v.AfterRuntime) {
				return fmt.Errorf("shared limits differ from declaration or retained history")
			}
			raw = v.BeforeRuntime
			enrolled, err = sharedLimitsPredecessor(d, enrolled)
			if err != nil {
				return err
			}
			continue
		}
		if enrolledOutputJournal(enrolled) {
			if _, e := readEnrolledOutputIntent(d, enrolled); e != nil {
				return e
			}
			enrolled, err = enrolledOutputPredecessor(d, enrolled)
			if err != nil {
				return err
			}
			continue
		}
		v, e := readMappingIntent(d, enrolled)
		if e != nil {
			return e
		}
		if string(raw) != string(v.AfterRuntime) {
			return fmt.Errorf("admitted LED mapping differs from declaration or retained history")
		}
		raw = v.BeforeRuntime
		enrolled, err = mappingPredecessor(d, enrolled)
		if err != nil {
			return err
		}
	}

	return verifyEnrollmentInput(d, paths, enrolled, reservation, raw)
}

// A qualified gateway is a successor of enrollment. Its exact qualification
// record is frozen in the outer journal before invoking the domain transition.
// Shared reporting retains its own specialist-owned transaction and can resume
// independently if admission completed before the backup installation did.
func advanceNotificationDelivery(ctx context.Context, r ControlRequest, paths ResolvedPaths, d *safefs.Directory, old OperationJournal, result ControlResult, lifecycle notificationDeliveryLifecycle) (ControlResult, error) {
	if err := validateOperationJournal(old); err != nil {
		return result, err
	}
	if len(old.Children) != 1 || old.Pins.Action != "apply" || old.Pins.UID != r.Facts.UID || old.Pins.Home != r.Facts.Home || old.Pins.DesiredHash != r.DesiredHash || old.Pins.Artifacts["helper"] != r.HelperSHA256 || old.Pins.Installations["notification-worker"] != paths.Installation || old.Pins.Installations["runtime"] != paths.Runtime || old.Pins.Installations["state"] != paths.State {
		return result, fmt.Errorf("delivery admission changed the pinned target, artifacts or policy")
	}
	pins := old.Pins
	var request install.DeliveryAdmissionRequest
	switch old.Children[0].Name {
	case "notification-delivery-declaration":
		if !old.Complete {
			return result, fmt.Errorf("finish declaration continuation")
		}
		v, err := readDeliveryProviderIntent(d, old)
		if err != nil {
			return result, err
		}
		if err = verifyMappingScope(d, old, lifecycle.scopeInspect); err != nil {
			return result, err
		}
		result.State, result.Code, result.Next = "reporting_connected", "ok", "declaration recorded; effective runtime and delivery qualification are unchanged"
		result.Identity = &v.Before.Identity
		return result, nil

	case "notification-delivery-derived-freshness", "notification-delivery-provider-enrollment", "notification-delivery-destination", "notification-delivery-routes", "notification-delivery-producer", "notification-delivery-limits", "notification-delivery-providers", "notification-requalification", "notification-shared-report-route", "notification-shared-derived-freshness", "notification-shared-command-activity", "notification-shared-command-policy", "notification-shared-commands", "notification-shared-routes", "notification-shared-producer", "notification-shared-providers", "notification-shared-freshness", "notification-shared-limits", "notification-enrollment", "notification-mapping", "notification-remapping", "notification-enrolled-led-output":
		if !old.Complete {
			return result, fmt.Errorf("finish consumer enrollment before delivery admission")
		}
		if r.Desired.Notification.StatusLEDs.Enabled && len(r.Desired.Notification.StatusLEDs.Bindings) != 0 && !mappingJournal(old) && !enrolledOutputJournal(old) && !sharedLimitsJournal(old) && !requalificationJournal(old) && !deliveryProviderJournal(old) {
			return result, fmt.Errorf("apply declared LED bindings before delivery admission")
		}
		if sharedLimitsJournal(old) {
			if err := verifySharedLEDDeclaration(r, paths, d, old); err != nil {
				return result, err
			}
		}
		if mappingJournal(old) {
			if _, err := readMappingIntent(d, old); err != nil {
				return result, err
			}
		}
		if err := verifyMappingScope(d, old, lifecycle.scopeInspect); err != nil {
			return result, err
		}
		var ready bool
		var err error
		request, ready, err = lifecycle.review(ctx, paths.Installation, old.Children[0].Receipt)
		if err != nil || !ready {
			return result, err
		}
		if requalificationJournal(old) && request.QualificationHash == old.Pins.Artifacts["qualification"] {
			// A second pause can produce the same verification manifest. The
			// operator requested new observations; the prior report is not a renewal.
			return result, nil
		}
		if request.Root != paths.Installation || request.ManifestHash != old.Children[0].Receipt || !validDigest(request.QualificationHash) {
			return result, fmt.Errorf("delivery review returned different installation pins")
		}
		pins.PreviousOperation, pins.Previous = old.ID, old.Digest()
		pins.Artifacts = maps.Clone(pins.Artifacts)
		pins.Artifacts["qualification"] = request.QualificationHash
	case "notification-delivery":
		var previous OperationJournal
		if err := d.ReadJSON("history/"+old.Pins.PreviousOperation+".json", &previous); err != nil {
			return result, err
		}
		if validateOperationJournal(previous) != nil || !previous.Complete || previous.Digest() != old.Pins.Previous || !enrolledNotificationJournal(previous) || previous.Pins.DesiredHash != old.Pins.DesiredHash {
			return result, fmt.Errorf("delivery admission lost its enrolled predecessor")
		}
		if requalificationJournal(previous) && old.Pins.Artifacts["qualification"] == previous.Pins.Artifacts["qualification"] {
			return result, fmt.Errorf("renewed delivery requires a newly recorded qualification")
		}
		if err := verifyMappingScope(d, previous, lifecycle.scopeInspect); err != nil {
			return result, err
		}
		request = install.DeliveryAdmissionRequest{Root: paths.Installation, ManifestHash: previous.Children[0].Receipt, QualificationHash: old.Pins.Artifacts["qualification"]}
		if !validDigest(request.QualificationHash) {
			return result, fmt.Errorf("delivery admission qualification pin missing")
		}
	default:
		return result, fmt.Errorf("delivery admission requires the retained enrollment lifecycle")
	}
	step := &notificationDeliveryStep{request: request, lifecycle: lifecycle}
	if _, err := (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step}); err != nil {
		return result, err
	}
	result.Version, result.State, result.Code = 1, "delivery_admitted", "ok"
	result.Identity = &step.manifest.Identity
	result.Next = "gateway delivery admitted; finish the declared backup installation to connect its reports"
	if r.Desired.Backup == nil || r.Desired.Backup.Notifications.Worker != r.Server {
		return result, fmt.Errorf("admitted delivery lost its declared backup dependency")
	}
	observation, err := lifecycle.backupStatus(ctx, r.Facts.Home)
	if err != nil {
		return result, err
	}
	backupDesired := r.DesiredHash
	if r.DesiredScope == 1 {
		backupDesired, err = r.Desired.Fingerprint()
		if err != nil {
			return result, err
		}
	}
	if observation.DesiredHash != backupDesired {
		if r.DesiredScope != 1 {
			return result, fmt.Errorf("backup policy differs from gateway admission")
		}
		result.Next = "gateway policy is unchanged; run deploy backup-worker " + r.Server + " to apply the changed backup policy"
		return result, nil
	}
	if observation.Stage != "complete" && observation.Stage != "ownership_review_required" {
		return result, nil
	}
	if err = lifecycle.connect(ctx, r.Facts.Home, backupDesired, protocol.Hash(step.manifest)); err != nil {
		return result, err
	}
	result.State = "reporting_connected"
	result.Next = "shared report service connected; backup capture qualification and schedule activation remain separate"
	if r.Desired.Notification.Commands.Enabled {
		var current OperationJournal
		if err = d.ReadJSON("operation.json", &current); err != nil {
			return result, err
		}
		return advanceNotificationCommands(ctx, r, paths, d, current, result, defaultNotificationCommands())
	}
	return result, nil
}

type notificationDeliveryStep struct {
	request   install.DeliveryAdmissionRequest
	lifecycle notificationDeliveryLifecycle
	manifest  install.ManagedManifest
}

func (s *notificationDeliveryStep) Name() string { return "notification-delivery" }
func (s *notificationDeliveryStep) Inspect(ctx context.Context, _ string) (string, bool, error) {
	m, complete, err := s.lifecycle.inspect(ctx, s.request)
	if err != nil || !complete {
		return "", complete, err
	}
	if m.Root != s.request.Root || m.Mode != "delivery" || m.DeliveryActivation == nil || m.DeliveryOwnerHash == "" {
		return "", false, fmt.Errorf("delivery domain returned no admitted installation")
	}
	s.manifest = m
	return protocol.Hash(m), true, nil
}
func (s *notificationDeliveryStep) Run(ctx context.Context, _ string) error {
	_, err := s.lifecycle.ensure(ctx, s.request)
	return err
}
