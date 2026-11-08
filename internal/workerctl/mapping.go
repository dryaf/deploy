package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

type notificationMappingLifecycle struct {
	inspect  func(context.Context, string) (install.InstallationObservation, error)
	migrate  func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error)
	boundary func(string) error
}

func mappingJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && (j.Children[0].Name == "notification-mapping" || j.Children[0].Name == "notification-remapping")
}

func enrolledNotificationJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && (sharedActivityJournal(j) || j.Children[0].Name == "notification-enrollment" || commandPauseJournal(j) || mappingJournal(j) || enrolledOutputJournal(j) || sharedLimitsJournal(j) || requalificationJournal(j) || deliveryProviderJournal(j))
}

// Enrollment and mapping are separate forward transitions. Keep the original
// consumer input immutable; the mapping owns a new input, never rewrites the
// enrollment receipt, and cannot admit delivery or activate producer generations.
func applyEnrolledLEDBindings(ctx context.Context, r ControlRequest, paths ResolvedPaths, helper string, d *safefs.Directory, current OperationJournal, input install.RuntimeInput, reservation backup.ProducerReservation, l notificationMappingLifecycle) (ControlResult, error) {
	result := ControlResult{Version: 1}
	if validateOperationJournal(current) != nil || !enrolledNotificationJournal(current) || current.Pins.DesiredScope != r.DesiredScope || current.Pins.DesiredHash != r.DesiredHash || current.Pins.UID != r.Facts.UID || current.Pins.Home != r.Facts.Home || current.Pins.Artifacts["helper"] != r.HelperSHA256 || current.Pins.Installations["notification-worker"] != paths.Installation || current.Pins.Installations["runtime"] != paths.Runtime || current.Pins.Installations["state"] != paths.State {
		return result, fmt.Errorf("LED mapping requires unchanged enrolled target, policy and artifacts")
	}
	if r.Desired.Notification == nil || !r.Desired.Notification.StatusLEDs.Enabled || len(r.Desired.Notification.StatusLEDs.Bindings) == 0 || !deferredBackupLEDBindings(r.Desired) {
		return result, fmt.Errorf("initial LED mapping requires declared local backup bindings")
	}
	compiled, err := compileReservedNotification(r.Desired, r.Facts, paths, input, reservation)
	if err != nil {
		return result, err
	}
	preserveRuntimeRouteLabels(input, &compiled.Input)
	after, err := compiled.Input.Bytes()
	if err != nil {
		return result, err
	}
	compiled.Input.Worker.LEDBindings = nil
	before, err := compiled.Input.Bytes()
	if err != nil {
		return result, err
	}
	enrolled := current
	pins := current.Pins
	if mappingJournal(current) {
		enrolled, err = mappingPredecessor(d, current)
		if err != nil {
			return result, err
		}
	} else {
		if !current.Complete {
			return result, fmt.Errorf("finish consumer enrollment before mapping LEDs")
		}
		o, e := l.inspect(ctx, paths.Installation)
		if e != nil {
			return result, e
		}
		if o.Phase != "complete" || o.Unbound || protocol.Hash(o.Manifest) != current.Children[0].Receipt {
			return result, fmt.Errorf("gateway changed before LED mapping review")
		}
		if e = validateMappingIntent(notificationMappingIntent{1, current.ID, r.DesiredHash, o.Manifest, before, after}); e != nil {
			return result, e
		}
		pins.PreviousOperation, pins.Previous = current.ID, current.Digest()
	}
	if err = verifyEnrollmentInput(d, paths, enrolled, reservation, before); err != nil {
		return result, err
	}
	s := &notificationMappingStep{directory: d, paths: paths, helper: helper, helperHash: r.HelperSHA256, desiredHash: r.DesiredHash, expected: enrolled.Children[0].Receipt, before: before, after: after, lifecycle: l}
	if _, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{s}); err != nil {
		return result, err
	}
	result.State, result.Code = "verification", "ok"
	result.Identity = &s.manifest.Identity
	result.Next = "declared LED bindings installed for the real backup producer; record actual observations with deploy notification-worker qualify " + r.Server + " before production reporting"
	return result, nil
}

func verifyEnrollmentInput(d *safefs.Directory, paths ResolvedPaths, enrolled OperationJournal, reservation backup.ProducerReservation, raw []byte) error {
	var intent consumerEnrollmentIntent
	if err := d.ReadJSON("consumer-enrollment.json", &intent); err != nil {
		return err
	}
	if validateOperationJournal(enrolled) != nil || !enrolled.Complete || len(enrolled.Children) != 1 || enrolled.Children[0].Name != "notification-enrollment" || intent.Version != 1 || intent.Operation != enrolled.Children[0].Operation || intent.Reservation != reservation || intent.RuntimeHash != inputDigest(raw) {
		return fmt.Errorf("consumer enrollment input differs from declaration")
	}
	actual, err := readPrivateGeneratedInput(filepath.Join(paths.Config, "consumers/runtime.json"), len(raw))
	if err != nil || string(actual) != string(raw) {
		return fmt.Errorf("generated consumer runtime changed; migration required")
	}
	return nil
}

func mappingPredecessor(d *safefs.Directory, current OperationJournal) (OperationJournal, error) {
	var prior OperationJournal
	if validateOperationJournal(current) != nil || !mappingJournal(current) {
		return prior, fmt.Errorf("invalid mapping journal")
	}
	if err := d.ReadJSON("history/"+current.Pins.PreviousOperation+".json", &prior); err != nil {
		return prior, err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != current.Pins.PreviousOperation || prior.Digest() != current.Pins.Previous || len(prior.Children) != 1 || (!remappingJournal(current) && prior.Children[0].Name != "notification-enrollment") || remappingJournal(current) && !enrolledNotificationJournal(prior) {
		return prior, fmt.Errorf("LED mapping lost its enrolled predecessor")
	}
	want := prior.Pins
	want.PreviousOperation, want.Previous = prior.ID, prior.Digest()
	if remappingJournal(current) {
		if current.Pins.DesiredScope != 1 || prior.Pins.DesiredScope != 1 {
			return prior, fmt.Errorf("mapping revision requires notification scope pins")
		}
		before, err := readDeclaration(d, prior.Pins.DesiredHash, prior.Pins.DesiredScope)
		if err != nil {
			return prior, err
		}
		after, err := readDeclaration(d, current.Pins.DesiredHash, current.Pins.DesiredScope)
		if err != nil {
			return prior, err
		}
		if err = validateRemappingPolicy(before, after, prior.Pins.DesiredHash); err != nil {
			return prior, err
		}
		want.DesiredHash = current.Pins.DesiredHash
	}
	if !reflect.DeepEqual(want, current.Pins) {
		return prior, fmt.Errorf("LED mapping changed enrolled policy or target authority")
	}
	return prior, nil
}

type notificationMappingIntent struct {
	Version       int                     `json:"version"`
	Operation     string                  `json:"operation"`
	DesiredHash   string                  `json:"desired_hash"`
	Before        install.ManagedManifest `json:"before"`
	BeforeRuntime []byte                  `json:"before_runtime"`
	AfterRuntime  []byte                  `json:"after_runtime"`
}

func validateMappingIntent(v notificationMappingIntent) error {
	if v.Version != 1 || !safefs.ValidID(v.Operation, "n") || !validDigest(v.DesiredHash) || v.Before.Mode != "verification" || v.Before.RecoveryCommitHash != "" || v.Before.DeliveryOwnerHash != "" || v.Before.LEDOutput != nil && (v.Before.LEDOutput.Disabled || !safefs.ValidID(v.Before.LEDOutput.Operation, "n")) || v.Before.Commands != nil || v.Before.RuntimeHash != inputDigest(v.BeforeRuntime) {
		return fmt.Errorf("LED mapping input authority changed")
	}
	var before, after install.RuntimeInput
	if err := safefs.DecodeJSON(v.BeforeRuntime, &before, maxControlFrame); err != nil {
		return err
	}
	if err := safefs.DecodeJSON(v.AfterRuntime, &after, maxControlFrame); err != nil {
		return err
	}
	b, err := before.Resolve()
	if err != nil {
		return err
	}
	a, err := after.Resolve()
	if err != nil {
		return err
	}
	if b.Bootstrap || a.Bootstrap || b.LEDSocket == "" || protocol.Hash(b) != v.Before.ConfigHash {
		return fmt.Errorf("LED mapping source runtime changed")
	}
	if err = worker.ValidateLEDBindingChange(b.Worker, a.Worker); err != nil {
		return err
	}
	b.Worker.LEDBindings = a.Worker.LEDBindings
	if protocol.Hash(b) != protocol.Hash(a) {
		return fmt.Errorf("LED mapping changed unrelated runtime policy")
	}
	return nil
}

func validateMappingManifest(v notificationMappingIntent, m install.ManagedManifest) error {
	var input install.RuntimeInput
	if err := safefs.DecodeJSON(v.AfterRuntime, &input, maxControlFrame); err != nil {
		return err
	}
	c, err := input.Resolve()
	if err != nil {
		return err
	}
	want := v.Before
	want.ConfigHash, want.RuntimeHash = protocol.Hash(c), inputDigest(v.AfterRuntime)
	want.Revision, want.UnitHash = m.Revision, m.UnitHash
	want.Paths.Config, want.Paths.Executable = m.Paths.Config, m.Paths.Executable
	want.HelperHash, want.HelperRevision = want.BinaryHash, m.Revision
	if m.Revision == v.Before.Revision || protocol.Hash(want) != protocol.Hash(m) {
		return fmt.Errorf("LED mapping receipt changed installation authority")
	}
	return nil
}

type notificationMappingStep struct {
	directory                                 *safefs.Directory
	paths                                     ResolvedPaths
	helper, helperHash, desiredHash, expected string
	before, after                             []byte
	lifecycle                                 notificationMappingLifecycle
	manifest                                  install.ManagedManifest
}

func (s *notificationMappingStep) Name() string { return "notification-mapping" }
func (s *notificationMappingStep) runtimePath(op string) string {
	return filepath.Join(s.paths.Control, "mapping-"+op+"-runtime.json")
}
func (s *notificationMappingStep) boundary(at string) error {
	if s.lifecycle.boundary != nil {
		return s.lifecycle.boundary(at)
	}
	return nil
}
func (s *notificationMappingStep) intent(op string) (notificationMappingIntent, error) {
	var v notificationMappingIntent
	if err := s.directory.ReadJSON("mapping-"+op+".json", &v); err != nil {
		return v, err
	}
	if err := validateMappingIntent(v); err != nil {
		return v, err
	}
	if v.Operation != op || v.DesiredHash != s.desiredHash || protocol.Hash(v.Before) != s.expected || v.Before.BinaryHash != s.helperHash || string(v.BeforeRuntime) != string(s.before) || string(v.AfterRuntime) != string(s.after) {
		return v, fmt.Errorf("retained LED mapping inputs changed")
	}
	return v, nil
}

func (s *notificationMappingStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	o, err := s.lifecycle.inspect(ctx, s.paths.Installation)
	if err != nil {
		return "", false, err
	}
	v, e := s.intent(op)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return "", false, e
	}
	if o.Phase == "complete" && protocol.Hash(o.Manifest) == s.expected && !o.Unbound {
		return "", false, nil
	}
	if e != nil {
		return "", false, fmt.Errorf("LED mapping changed without retained intent")
	}
	if o.Phase != "complete" {
		return "", false, nil
	}
	if o.Unbound {
		return "", false, fmt.Errorf("LED mapping lost producer enrollment")
	}
	if err = validateMappingManifest(v, o.Manifest); err != nil {
		return "", false, err
	}
	raw, err := readPrivateGeneratedInput(s.runtimePath(op), len(v.AfterRuntime))
	if err != nil || string(raw) != string(v.AfterRuntime) {
		return "", false, fmt.Errorf("generated LED mapping input changed")
	}
	s.manifest = o.Manifest
	return protocol.Hash(o.Manifest), true, nil
}

func (s *notificationMappingStep) Run(ctx context.Context, op string) error {
	v, err := s.intent(op)
	if errors.Is(err, os.ErrNotExist) {
		o, e := s.lifecycle.inspect(ctx, s.paths.Installation)
		if e != nil {
			return e
		}
		if o.Phase != "complete" || o.Unbound || protocol.Hash(o.Manifest) != s.expected {
			return fmt.Errorf("gateway changed before LED mapping")
		}
		v = notificationMappingIntent{1, op, s.desiredHash, o.Manifest, s.before, s.after}
		if err = validateMappingIntent(v); err != nil {
			return err
		}
		err = s.directory.WriteJSON("mapping-"+op+".json", v, false)
	}
	if err != nil {
		return err
	}
	if err = s.boundary("mapping-intent"); err != nil {
		return err
	}
	if err = publishPrivateInput(s.runtimePath(op), v.AfterRuntime); err != nil {
		return err
	}
	if err = s.boundary("mapping-input-published"); err != nil {
		return err
	}
	spec := install.ManagedSpec{Version: 1, Root: s.paths.Installation, State: s.paths.State, UnitDirectory: s.paths.UnitDirectory, RuntimeFile: s.runtimePath(op), BinaryFile: s.helper, BinarySHA256: s.helperHash, DeniedPaths: s.paths.DeniedPaths, FreshSignal: v.Before.FreshSignal}
	if _, err = s.lifecycle.migrate(ctx, spec, s.expected); err != nil {
		return err
	}
	return s.boundary("mapping-domain-complete")
}

func verifyMappingReceipt(d *safefs.Directory, current OperationJournal, m install.ManagedManifest) error {
	return verifyMappingReceiptWithScope(d, current, m, backup.InspectVerificationNotificationScope)
}

func verifyMappingReceiptWithScope(d *safefs.Directory, current OperationJournal, m install.ManagedManifest, inspect func(string, string, string, string) (string, bool, error)) error {
	if !mappingJournal(current) {
		return nil
	}
	v, err := readMappingIntent(d, current)
	if err != nil {
		return err
	}
	if protocol.Hash(m) != current.Children[0].Receipt {
		return fmt.Errorf("LED mapping completion receipt changed")
	}
	if err = validateMappingManifest(v, m); err != nil {
		return err
	}
	return verifyMappingScope(d, current, inspect)
}

func verifyMappingScope(d *safefs.Directory, current OperationJournal, inspect func(string, string, string, string) (string, bool, error)) error {
	if sharedActivityJournal(current) {
		if _, err := readSharedActivityIntent(d, current); err != nil {
			return err
		}
		prior, err := sharedActivityPredecessor(d, current)
		if err != nil {
			return err
		}
		return verifyMappingScope(d, prior, inspect)
	}
	if requalificationJournal(current) {
		if inspect == nil {
			inspect = backup.InspectRetainedDeliveryPolicyScope
		}
		origin, err := requalificationAncestry(d, current, func(j OperationJournal) error {
			return verifyMappingScope(d, j, inspect)
		})
		if err != nil {
			return err
		}
		return verifyMappingScope(d, origin, inspect)
	}
	if !remappingJournal(current) && !enrolledOutputJournal(current) && !sharedLimitsJournal(current) && !deliveryOutputJournal(current) && !deliveryProviderJournal(current) {
		return nil
	}
	var before string
	if deliveryProviderJournal(current) {
		v, err := readDeliveryProviderIntent(d, current)
		if err != nil {
			return err
		}
		before = protocol.Hash(v.Before)
	} else if deliveryOutputJournal(current) {
		v, err := readDeliveryOutputIntent(d, current)
		if err != nil {
			return err
		}
		before = protocol.Hash(v.Before)
	} else if sharedLimitsJournal(current) {
		v, err := readSharedLimitsIntent(d, current)
		if err != nil {
			return err
		}
		before = protocol.Hash(v.Before)
	} else if remappingJournal(current) {
		v, err := readMappingIntent(d, current)
		if err != nil {
			return err
		}
		before = protocol.Hash(v.Before)
	} else {
		v, err := readEnrolledOutputIntent(d, current)
		if err != nil {
			return err
		}
		before = protocol.Hash(v.Before)
	}
	if inspect == nil {
		inspect = backup.InspectRetainedDeliveryPolicyScope
		if deliveryProviderJournal(current) && isCommandDeliveryPolicy(deliveryProviderKind(current)) {
			inspect = backup.InspectRetainedCommandPolicyScope
		}
	}
	receipt, done, err := inspect(current.Pins.Home, current.Children[0].Operation, before, current.Pins.DesiredHash)
	if err != nil {
		return err
	}
	if !done || receipt != current.Children[0].Receipt {
		return fmt.Errorf("LED policy has no completed backup scope receipt")
	}
	if deliveryProviderJournal(current) {
		prior, err := deliveryProviderPredecessor(d, current)
		if err != nil {
			return err
		}
		origin, err := requalificationAncestry(d, prior, func(j OperationJournal) error { return verifyMappingScope(d, j, inspect) })
		if err != nil {
			return err
		}
		return verifyMappingScope(d, origin, inspect)
	}
	return nil
}

func readMappingIntent(d *safefs.Directory, current OperationJournal) (notificationMappingIntent, error) {
	var v notificationMappingIntent
	prior, err := mappingPredecessor(d, current)
	if err != nil {
		return v, err
	}
	if err = d.ReadJSON("mapping-"+current.Children[0].Operation+".json", &v); err != nil {
		return v, err
	}
	if err = validateMappingIntent(v); err != nil {
		return v, err
	}
	if v.Operation != current.Children[0].Operation || v.DesiredHash != current.Pins.DesiredHash || v.Before.BinaryHash != current.Pins.Artifacts["helper"] || protocol.Hash(v.Before) != prior.Children[0].Receipt {
		return v, fmt.Errorf("LED mapping completion intent changed")
	}
	raw, err := readPrivateGeneratedInput(filepath.Join(d.File().Name(), "mapping-"+v.Operation+"-runtime.json"), len(v.AfterRuntime))
	if err != nil || string(raw) != string(v.AfterRuntime) {
		return v, fmt.Errorf("LED mapping input lost or changed")
	}
	return v, nil
}
