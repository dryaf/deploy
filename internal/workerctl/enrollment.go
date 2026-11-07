package workerctl

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type notificationEnrollmentLifecycle struct {
	inspect func(context.Context, string) (install.InstallationObservation, error)
	enroll  func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error)
}

func reconcileReservedBackup(ctx context.Context, r ControlRequest, paths ResolvedPaths, executable string, d *safefs.Directory, old OperationJournal, input install.RuntimeInput, unbound ControlResult) (ControlResult, error) {
	b := r.Desired.Backup
	if b == nil || !config.WorkerEnabled(b.Enabled) || !config.WorkerEnabled(b.Notifications.Enabled) || b.Notifications.Worker == "" {
		return unbound, nil
	}
	if b.Notifications.Worker != r.Server {
		return unbound, fmt.Errorf("shared notifications require the same server and UID")
	}
	reservation, present, err := backup.InspectReservedProducer(r.Facts.Home, r.DesiredHash, paths.Installation, b.Notifications.Route)
	if err != nil {
		return unbound, err
	}
	if !present {
		if len(old.Children) == 1 && enrolledNotificationJournal(old) {
			return unbound, fmt.Errorf("enrolled backup reservation missing; recovery required")
		}
		return unbound, nil
	}
	result := unbound
	if remappingJournal(old) || enrolledOutputJournal(old) || sharedLimitsJournal(old) {
		observed, e := install.InspectInstallation(ctx, paths.Installation)
		if e != nil {
			return result, e
		}
		if !old.Complete || observed.Phase != "complete" || observed.Unbound || protocol.Hash(observed.Manifest) != old.Children[0].Receipt {
			return result, fmt.Errorf("finish retained LED policy before admission")
		}
		if e = verifyReleaseReceipt(d, old, observed.Manifest); e != nil {
			return result, e
		}
		raw, e := retainedEnrolledRuntime(d, paths, old)
		if e != nil {
			return result, e
		}
		var retained install.RuntimeInput
		if e = safefs.DecodeJSON(raw, &retained, maxControlFrame); e != nil {
			return result, e
		}
		if e = verifyReservedConsumerInput(r, paths, d, old, retained, reservation); e != nil {
			return result, e
		}
		result.Code, result.State, result.Identity, result.LEDOutput = "ok", "verification", &observed.Manifest.Identity, observed.Manifest.LEDOutput
		return advanceNotificationDelivery(ctx, r, paths, d, old, result, defaultNotificationDelivery())
	}
	if !mappingJournal(old) {
		result, err = enrollReservedBackup(ctx, r, paths, executable, d, old, input, reservation, notificationEnrollmentLifecycle{install.InspectInstallation, install.EnrollVerificationConsumers})
		if err != nil {
			return result, err
		}
	}
	var enrolled OperationJournal
	if err = d.ReadJSON("operation.json", &enrolled); err != nil {
		return result, err
	}
	if mappingJournal(enrolled) || r.Desired.Notification.StatusLEDs.Enabled && len(r.Desired.Notification.StatusLEDs.Bindings) != 0 {
		result, err = applyEnrolledLEDBindings(ctx, r, paths, executable, d, enrolled, input, reservation, notificationMappingLifecycle{inspect: install.InspectInstallation, migrate: install.MigrateVerificationLEDBindings})
		if err != nil {
			return result, err
		}
		if err = d.ReadJSON("operation.json", &enrolled); err != nil {
			return result, err
		}
	}
	return advanceNotificationDelivery(ctx, r, paths, d, enrolled, result, defaultNotificationDelivery())
}

// Enrollment is an explicit successor to the unbound bootstrap, even when the
// desired YAML has not changed. Its canonical receipt replaces the bootstrap
// receipt, while the original operation remains in private history.
func enrollReservedBackup(ctx context.Context, r ControlRequest, paths ResolvedPaths, executable string, d *safefs.Directory, old OperationJournal, input install.RuntimeInput, reservation backup.ProducerReservation, lifecycle notificationEnrollmentLifecycle) (ControlResult, error) {
	result := ControlResult{Version: 1}
	if err := reservation.Validate(reservation.Producer.ID); err != nil {
		return result, err
	}
	b := r.Desired.Backup
	if b == nil || !config.WorkerEnabled(b.Enabled) || !config.WorkerEnabled(b.Notifications.Enabled) || b.Notifications.Worker != r.Server {
		return result, fmt.Errorf("backup reservation requires the declared local notification dependency")
	}
	if err := validateOperationJournal(old); err != nil {
		return result, err
	}
	if old.Pins.UID != r.Facts.UID || old.Pins.Home != r.Facts.Home || old.Pins.DesiredHash != r.DesiredHash || old.Pins.Artifacts["helper"] != r.HelperSHA256 || old.Pins.Installations["notification-worker"] != paths.Installation || old.Pins.Installations["runtime"] != paths.Runtime || old.Pins.Installations["state"] != paths.State || len(old.Children) != 1 {
		return result, fmt.Errorf("consumer enrollment changed the pinned installation or desired policy")
	}
	compiled, err := compileReservedNotification(r.Desired, r.Facts, paths, input, reservation)
	if err != nil {
		return result, err
	}
	preserveRuntimeRouteLabels(input, &compiled.Input)
	// Binding policy has its own domain transition and receipt; consumer
	// enrollment itself may only add the reserved producer.
	compiled.Input.Worker.LEDBindings = nil
	// Output admission has a separate retained disposition; enrollment does not
	// remove the daemon endpoint when the YAML currently disables LEDs.
	compiled.Input.LEDSocket, compiled.Input.LEDEndpoint = input.LEDSocket, input.LEDEndpoint
	raw, err := compiled.Input.Bytes()
	if err != nil {
		return result, err
	}
	pins := old.Pins
	expected := ""
	switch old.Children[0].Name {
	case "notification-adopt", "notification-bootstrap", "notification-led-output", "notification-activity", "notification-artifact", "notification-command-draft", "notification-limits", "notification-routes", "notification-providers":
		if !old.Complete || old.Pins.Action != "apply" {
			return result, fmt.Errorf("finish gateway bootstrap before consumer enrollment")
		}
		if activityJournal(old) {
			observed, e := lifecycle.inspect(ctx, paths.Installation)
			if e != nil {
				return result, e
			}
			if err = verifyActivityReceipt(d, old, observed.Manifest); err != nil {
				return result, err
			}
		}
		if adoptionJournal(old) || releaseJournal(old) || limitsJournal(old) {
			observed, e := lifecycle.inspect(ctx, paths.Installation)
			if e != nil {
				return result, e
			}
			if err = verifyReleaseReceipt(d, old, observed.Manifest); err != nil {
				return result, err
			}
		}
		expected = old.Children[0].Receipt
		pins.PreviousOperation, pins.Previous = old.ID, old.Digest()
	case "notification-enrollment":
		var prior OperationJournal
		if err = d.ReadJSON("history/"+old.Pins.PreviousOperation+".json", &prior); err != nil {
			return result, err
		}
		if validateOperationJournal(prior) != nil || prior.Digest() != old.Pins.Previous || !unboundPredecessor(prior) {
			return result, fmt.Errorf("consumer enrollment predecessor changed")
		}
		expected = prior.Children[0].Receipt
	default:
		return result, fmt.Errorf("gateway lifecycle requires explicit enrollment review")
	}
	spec := install.ManagedSpec{Version: 1, Root: paths.Installation, State: paths.State, UnitDirectory: paths.UnitDirectory, RuntimeFile: filepath.Join(paths.Config, "consumers", "runtime.json"), BinaryFile: executable, BinarySHA256: r.HelperSHA256, DeniedPaths: paths.DeniedPaths}
	step := &notificationEnrollmentStep{directory: d, spec: spec, expected: expected, raw: raw, reservation: reservation, lifecycle: lifecycle}
	if _, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step}); err != nil {
		return result, err
	}
	result.State, result.Code = "verification", "ok"
	result.Identity = &step.manifest.Identity
	result.Next = "real backup producer enrolled; run deploy notification-worker qualify " + r.Server + " to record actual observations before production reporting"
	return result, nil
}

type consumerEnrollmentIntent struct {
	Version     int                        `json:"version"`
	Operation   string                     `json:"operation"`
	Before      install.ManagedManifest    `json:"before"`
	RuntimeHash string                     `json:"runtime_hash"`
	Reservation backup.ProducerReservation `json:"reservation"`
}
type notificationEnrollmentStep struct {
	directory   *safefs.Directory
	spec        install.ManagedSpec
	expected    string
	raw         []byte
	reservation backup.ProducerReservation
	lifecycle   notificationEnrollmentLifecycle
	manifest    install.ManagedManifest
}

func (s *notificationEnrollmentStep) Name() string { return "notification-enrollment" }
func (s *notificationEnrollmentStep) intent(op string) (consumerEnrollmentIntent, error) {
	var intent consumerEnrollmentIntent
	err := s.directory.ReadJSON("consumer-enrollment.json", &intent)
	if err != nil {
		return intent, err
	}
	if intent.Version != 1 || intent.Operation != op || protocol.Hash(intent.Before) != s.expected || intent.RuntimeHash != fmt.Sprintf("%x", sha256.Sum256(s.raw)) || intent.Reservation != s.reservation {
		return intent, fmt.Errorf("consumer enrollment intent changed")
	}
	return intent, nil
}
func (s *notificationEnrollmentStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	observation, err := s.lifecycle.inspect(ctx, s.spec.Root)
	if err != nil {
		return "", false, err
	}
	intent, intentErr := s.intent(op)
	if intentErr != nil && !errors.Is(intentErr, os.ErrNotExist) {
		return "", false, intentErr
	}
	if protocol.Hash(observation.Manifest) == s.expected && observation.Unbound && observation.Phase == "complete" {
		return "", false, nil
	}
	if intentErr != nil {
		return "", false, fmt.Errorf("gateway enrollment changed without its retained intent")
	}
	if observation.Phase != "complete" {
		return "", false, nil
	}
	m, before := observation.Manifest, intent.Before
	if observation.Unbound || m.Identity != before.Identity || m.Installation != before.Installation || m.Root != before.Root || m.Paths.State != s.spec.State || m.UnitDirectory != s.spec.UnitDirectory || m.Mode != "verification" || m.BinaryHash != s.spec.BinarySHA256 || m.RuntimeHash != intent.RuntimeHash || protocol.Hash(m.FreshSignal) != protocol.Hash(before.FreshSignal) || protocol.Hash(m.LEDOutput) != protocol.Hash(before.LEDOutput) {
		return "", false, fmt.Errorf("gateway consumer enrollment receipt differs")
	}
	actual, err := readPrivateGeneratedInput(s.spec.RuntimeFile, len(s.raw))
	if err != nil || string(actual) != string(s.raw) {
		return "", false, fmt.Errorf("generated consumer input changed")
	}
	s.manifest = m
	return protocol.Hash(m), true, nil
}
func (s *notificationEnrollmentStep) Run(ctx context.Context, op string) error {
	_, err := s.intent(op)
	if errors.Is(err, os.ErrNotExist) {
		observation, e := s.lifecycle.inspect(ctx, s.spec.Root)
		if e != nil {
			return e
		}
		if observation.Phase != "complete" || !observation.Unbound || protocol.Hash(observation.Manifest) != s.expected {
			return fmt.Errorf("gateway changed before consumer reservation")
		}
		intent := consumerEnrollmentIntent{Version: 1, Operation: op, Before: observation.Manifest, RuntimeHash: fmt.Sprintf("%x", sha256.Sum256(s.raw)), Reservation: s.reservation}
		err = s.directory.WriteJSON("consumer-enrollment.json", intent, false)
	}
	if err != nil {
		return err
	}
	if err = publishPrivateInput(s.spec.RuntimeFile, s.raw); err != nil {
		return err
	}
	retained, err := s.intent(op)
	if err != nil {
		return err
	}
	s.spec.FreshSignal = retained.Before.FreshSignal
	_, err = s.lifecycle.enroll(ctx, s.spec, s.expected)
	return err
}
