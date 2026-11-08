package workerctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func sharedActivityStopping(d config.DesiredWorkers, server string) bool {
	return d.Notification != nil && d.Backup != nil && d.Backup.Notifications.Worker == server && server != "" && config.SharedActivityDisabled(d)
}

func sharedActivityJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-shared-activity"
}

type sharedActivityIntent struct {
	Version     int                               `json:"version"`
	Operation   string                            `json:"operation"`
	DesiredHash string                            `json:"desired_hash"` // Complete declaration, including disabled dependencies.
	Before      install.ManagedManifest           `json:"before"`
	Current     install.ManagedManifest           `json:"current"`
	Disabled    bool                              `json:"disabled"`
	StopGateway bool                              `json:"stop_gateway"`
	RestoreLED  bool                              `json:"restore_led"`
	Admission   *install.DeliveryAdmissionRequest `json:"admission,omitempty"`
	Phase       string                            `json:"phase"`
}

func sharedActivityPhase(p string) int {
	return map[string]int{"prepared": 1, "led-off": 2, "paused": 3, "service": 4, "qualified": 5, "led-restored": 6, "complete": 7}[p]
}
func validateSharedActivityIntent(v sharedActivityIntent) error {
	if v.Version != 1 || !safefs.ValidID(v.Operation, "n") || !validDigest(v.DesiredHash) || sharedActivityPhase(v.Phase) == 0 || v.Before.RecoveryCommitHash != "" || v.Before.SignalVerification != nil || v.Before.SignalProfiles != nil || v.Before.Commands != nil && v.Before.Commands.Mode != "drain" {
		return fmt.Errorf("invalid shared activity intent")
	}
	// The domain lifecycles own the exact stop/pause/LED/activation evidence. The
	// coordinator additionally refuses any change of release, runtime or identity.
	want := v.Before
	want.ActivityRestoration = v.Current.ActivityRestoration
	want.Mode, want.UnitHash, want.Commands, want.DeliveryActivation, want.LEDOutput = v.Current.Mode, v.Current.UnitHash, v.Current.Commands, v.Current.DeliveryActivation, v.Current.LEDOutput
	if protocol.Hash(want) != protocol.Hash(v.Current) || v.Current.Commands != nil && v.Current.Commands.Mode != "drain" || v.Admission != nil && (v.Disabled || v.Admission.Root != v.Before.Root || !validDigest(v.Admission.ManifestHash) || !validDigest(v.Admission.QualificationHash)) {
		return fmt.Errorf("shared activity changed retained gateway authority")
	}
	if v.Phase == "complete" {
		mode := "verification"
		if v.StopGateway {
			mode = "uninstalled"
		} else if !v.Disabled && v.Before.DeliveryOwnerHash != "" {
			mode = "delivery"
		}
		if v.Current.Mode != mode || v.Current.Commands != nil {
			return fmt.Errorf("shared activity completion has unexpected authority")
		}
	}
	return nil
}
func readSharedActivityIntent(d *safefs.Directory, j OperationJournal) (sharedActivityIntent, error) {
	var v sharedActivityIntent
	if !sharedActivityJournal(j) || validateOperationJournal(j) != nil {
		return v, fmt.Errorf("invalid shared activity journal")
	}
	if err := d.ReadJSON("shared-activity-"+j.Children[0].Operation+".json", &v); err != nil {
		return v, err
	}
	if err := validateSharedActivityIntent(v); err != nil {
		return v, err
	}
	if v.Operation != j.Children[0].Operation || v.DesiredHash != j.Pins.Artifacts["shared-activity-policy"] || v.Before.Root != j.Pins.Installations["notification-worker"] || v.Before.HelperHash != j.Pins.Artifacts["helper"] || j.Complete && (v.Phase != "complete" || protocol.Hash(v.Current) != j.Children[0].Receipt) {
		return v, fmt.Errorf("shared activity receipt changed")
	}
	return v, nil
}
func sharedActivityPredecessor(d *safefs.Directory, j OperationJournal) (OperationJournal, error) {
	var p OperationJournal
	if !sharedActivityJournal(j) || validateOperationJournal(j) != nil {
		return p, fmt.Errorf("invalid shared activity predecessor")
	}
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &p); err != nil {
		return p, err
	}
	if validateOperationJournal(p) != nil || !p.Complete || p.ID != j.Pins.PreviousOperation || p.Digest() != j.Pins.Previous {
		return p, fmt.Errorf("shared activity lost its predecessor")
	}
	before, err := readCommandDeclaration(d, p)
	if err != nil {
		return p, err
	}
	after, err := readCommandDeclaration(d, j)
	if err != nil {
		return p, err
	}
	if err = config.ValidateSharedActivityDeclaration(before, after); err != nil {
		return p, err
	}
	want := p.Pins
	want.PreviousOperation, want.Previous, want.DesiredHash = p.ID, p.Digest(), j.Pins.DesiredHash
	want.Artifacts = maps.Clone(p.Pins.Artifacts)
	want.Artifacts["shared-activity-policy"] = j.Pins.Artifacts["shared-activity-policy"]
	if want.Artifacts["command-backup-policy"] != "" {
		want.Artifacts["command-backup-policy"] = j.Pins.Artifacts["shared-activity-policy"]
	}
	if !reflect.DeepEqual(want, j.Pins) {
		return p, fmt.Errorf("shared activity changed target, artifact or dependency pins")
	}
	return p, nil
}

type sharedActivityLifecycle struct {
	inspect   func(context.Context, string) (install.InstallationObservation, error)
	pause     func(context.Context, install.DeliveryPauseRequest) (install.ManagedManifest, error)
	service   func(context.Context, string, string, bool) (install.ManagedManifest, error)
	output    func(context.Context, string, string, bool) (install.ManagedManifest, error)
	reconcile func(context.Context, string, config.DesiredWorkers, string, backup.SharedActivityRequest, func() error) (backup.ActivityResult, error)
	review    func(context.Context, string, string) (install.DeliveryAdmissionRequest, bool, error)
	ensure    func(context.Context, install.DeliveryAdmissionRequest) (install.ManagedManifest, error)
	status    func(context.Context, string) (backup.SetupObservation, error)
	reports   func(context.Context, string, string, string) error
	inputs    func(ControlRequest, ResolvedPaths, *safefs.Directory, config.DesiredWorkers) error
	qualify   qualificationLifecycle
	boundary  func(string) error
}

func defaultSharedActivity() sharedActivityLifecycle {
	return sharedActivityLifecycle{inspect: install.InspectRetainedInstallation, pause: install.EnsureDeliveryPause, service: install.SetSharedActivityService, output: install.SetLEDOutput, reconcile: backup.ReconcileSharedActivity, review: install.ReviewDeliveryAdmission, ensure: install.EnsureDeliveryAdmission, status: backup.InspectSetup, reports: backup.ResumeSharedActivityReports, inputs: func(r ControlRequest, p ResolvedPaths, d *safefs.Directory, b config.DesiredWorkers) error {
		raw, err := readLimitsRuntime(p.Runtime)
		if err != nil {
			return err
		}
		var input install.RuntimeInput
		if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
			return err
		}
		if _, present := b.Notification.Providers["signal"]; present && r.SignalSHA256 == "" {
			o, e := install.InspectRetainedInstallation(context.Background(), p.Installation)
			if e != nil {
				return e
			}
			if o.Manifest.FreshSignal == nil {
				return fmt.Errorf("shared activity requires the retained native Signal service")
			}
			native, e := install.InspectFreshSignal(context.Background(), *o.Manifest.FreshSignal, false)
			if e != nil {
				return e
			}
			r.SignalSHA256 = native.Spec.CLISHA256
		}
		r, err = retainedProviderReviewRequest(r, b, input)
		if err != nil {
			return err
		}
		var old OperationJournal
		if err = d.ReadJSON("operation.json", &old); err != nil {
			return err
		}
		receipt, valid := canonicalNotificationReceipt(old)
		if !valid {
			return fmt.Errorf("shared activity lost capability review pins")
		}
		if err = install.ReviewSharedActivityCapabilities(context.Background(), p.Installation, receipt); err != nil {
			return err
		}
		if err = backup.ReviewSharedActivity(context.Background(), r.Facts.Home, b, r.HelperSHA256, p.Installation); err != nil {
			return err
		}
		return verifyLEDOutputInputs(r, p, d, b)
	}, qualify: defaultQualificationLifecycle()}
}

// Both worker entry points use this same coordinator, including when the YAML
// removes the dependency from its derived notification fingerprint.
func reconcileSharedActivity(ctx context.Context, r ControlRequest, p ResolvedPaths, l sharedActivityLifecycle) (ControlResult, bool, error) {
	out := ControlResult{Version: 1}
	if r.Desired.Notification == nil || r.Desired.Backup == nil || r.Desired.Backup.Notifications.Worker != r.Server {
		return out, false, nil
	}
	d, err := safefs.Open(p.Control)
	if errors.Is(err, os.ErrNotExist) {
		return out, false, nil
	}
	if err != nil {
		return out, true, err
	}
	defer d.Close()
	var old OperationJournal
	if err = d.ReadJSON("operation.json", &old); errors.Is(err, os.ErrNotExist) {
		return out, false, nil
	} else if err != nil {
		return out, true, err
	}
	if validateOperationJournal(old) != nil {
		return out, true, fmt.Errorf("invalid coordinator before shared activity")
	}
	isActivity := sharedActivityJournal(old)
	if isActivity && old.Complete {
		if err = verifySharedActivityHistory(d, old); err != nil {
			return out, true, err
		}
	}
	before, err := readCommandDeclaration(d, old)
	if err != nil {
		// A newly retained outer intent can precede its declaration snapshot.
		if !isActivity || old.Complete {
			return out, true, err
		}
		before = r.Desired
	}
	changed := sharedActivityChanged(before, r.Desired)
	if !isActivity && !changed {
		return out, false, nil
	}
	if old.Pins.Worker != "notification-worker" || old.Pins.DesiredScope != 1 || r.DesiredScope != 1 || old.Pins.Home != r.Facts.Home || old.Pins.UID != r.Facts.UID || old.Pins.Artifacts["helper"] != r.HelperSHA256 || old.Pins.Installations["notification-worker"] != p.Installation || old.Pins.Installations["runtime"] != p.Runtime || old.Pins.Installations["state"] != p.State {
		return out, true, fmt.Errorf("shared activity requires unchanged target, helper and paths")
	}
	full, err := r.Desired.Fingerprint()
	if err != nil {
		return out, true, err
	}
	retry := isActivity && old.Pins.DesiredHash == r.DesiredHash && old.Pins.Artifacts["shared-activity-policy"] == full
	if !retry && isActivity && old.Complete && !changed {
		if config.SharedActivityDisabled(before) {
			return out, true, fmt.Errorf("restore shared activity before applying other configuration")
		}
		return out, false, nil
	}
	if retry && old.Complete && !config.SharedActivityDisabled(r.Desired) && (r.Action == "apply" || r.Action == "resume") {
		v, e := readSharedActivityIntent(d, old)
		if e != nil {
			return out, true, e
		}
		if v.Current.DeliveryOwnerHash == "" {
			out = sharedActivityResult(out, v)
			advanced, e := advanceNotificationDelivery(ctx, r, p, d, old, out, defaultNotificationDelivery())
			return advanced, true, e
		}
	}
	if len(r.Inputs) != 0 {
		if err = verifySharedActivityProvidedInputs(ctx, r, p); err != nil {
			return out, true, err
		}
	}
	if r.Action == "plan" || r.Action == "status" {
		if !retry {
			return out, false, nil
		}
		v, e := readSharedActivityIntent(d, old)
		if e != nil {
			return out, true, e
		}
		if _, e = l.status(ctx, r.Facts.Home); e != nil {
			return out, true, e
		}
		observed, e := l.inspect(ctx, p.Installation)
		if e != nil {
			return out, true, e
		}
		if old.Complete && (observed.Phase != "complete" || protocol.Hash(observed.Manifest) != protocol.Hash(v.Current)) {
			return out, true, fmt.Errorf("shared activity installation drift")
		}
		return sharedActivityResult(out, v), true, nil
	}
	if r.Action != "apply" && r.Action != "resume" && !isQualificationControl(r.Action) {
		return out, false, nil
	}
	if isQualificationControl(r.Action) {
		if !retry {
			return out, false, nil
		}
		v, e := readSharedActivityIntent(d, old)
		if e != nil {
			return out, true, e
		}
		if old.Complete && v.Current.Mode == "delivery" {
			return out, false, nil
		}
		staged := old.Complete && v.Phase == "complete" && v.Current.DeliveryOwnerHash == ""
		paused := !old.Complete && v.Phase == "service" && v.Current.DeliveryOwnerHash != ""
		if v.Disabled || v.Current.Mode != "verification" || !staged && !paused {
			return out, true, fmt.Errorf("restore the retained shared activity to verification before qualification")
		}
		if _, e = l.status(ctx, r.Facts.Home); e != nil {
			return out, true, e
		}
		var session install.QualificationSession
		switch r.Action {
		case "qualify-begin":
			session, e = l.qualify.begin(ctx, p.Installation, protocol.Hash(v.Current))
		case "qualify-observe":
			if r.Observation == nil {
				return out, true, fmt.Errorf("qualification observation required")
			}
			session, e = l.qualify.observe(ctx, p.Installation, protocol.Hash(v.Current), *r.Observation)
		case "qualify-commit":
			if r.Observation == nil || r.Observation.Scope != "" || r.Observation.ID != "" || r.Observation.Result != "" || r.Observation.EvidenceHash != "" || r.Observation.LEDVersion != "" || r.Observation.LEDConfigHash != "" {
				return out, true, fmt.Errorf("commit requires only its qualification session")
			}
			session, e = l.qualify.commit(ctx, p.Installation, protocol.Hash(v.Current), r.Observation.Session)
		}
		out.Code, out.State, out.Next = "ok", "qualification_draft", "record actual observations, then apply the retained YAML; commands and backup capture remain disabled"
		if session.Recorded {
			out.State = "qualification_recorded"
		}
		out.QualificationSession = &session
		return out, true, e
	}
	predecessor, pins := old, old.Pins
	if retry {
		predecessor = OperationJournal{}
		if err = d.ReadJSON("history/"+old.Pins.PreviousOperation+".json", &predecessor); err != nil {
			return out, true, err
		}
		if validateOperationJournal(predecessor) != nil || !predecessor.Complete || predecessor.ID != old.Pins.PreviousOperation || predecessor.Digest() != old.Pins.Previous {
			return out, true, fmt.Errorf("shared activity predecessor changed")
		}
	} else {
		if !old.Complete {
			return out, true, fmt.Errorf("resume the pending operation with its exact YAML before shared activity")
		}
		if err = config.ValidateSharedActivityDeclaration(before, r.Desired); err != nil {
			return out, true, err
		}
		o, e := l.inspect(ctx, p.Installation)
		if e != nil {
			return out, true, e
		}
		if o.Unbound && !isActivity {
			return out, false, nil
		}
		receipt, valid := canonicalNotificationReceipt(old)
		if !valid || o.Phase != "complete" || protocol.Hash(o.Manifest) != receipt || o.Unbound || o.Manifest.RecoveryCommitHash != "" || o.Manifest.SignalVerification != nil || o.Manifest.SignalProfiles != nil {
			return out, true, fmt.Errorf("shared activity requires exact unrecovered enrolled authority")
		}
		if o.Manifest.Mode == "verification" && o.Manifest.DeliveryOwnerHash != "" && !isActivity {
			return out, true, fmt.Errorf("finish the retained delivery qualification before changing shared activity")
		}
		if (before.Notification.Commands.Enabled || r.Desired.Notification.Commands.Enabled) || o.Manifest.Commands != nil && o.Manifest.Commands.Mode != "drain" {
			return out, true, fmt.Errorf("set commands.enabled: false and apply separately before shared activity")
		}
		if !isActivity && o.Manifest.Mode != "delivery" && o.Manifest.Mode != "verification" {
			return out, true, fmt.Errorf("explicit uninstall is not shared activity restoration authority")
		}
		if e = l.inputs(r, p, d, before); e != nil {
			return out, true, e
		}
		status, e := l.status(ctx, r.Facts.Home)
		if e != nil {
			return out, true, e
		}
		beforeFull, _ := before.Fingerprint()
		if status.Stage != "complete" || status.DesiredHash != beforeFull {
			return out, true, fmt.Errorf("finish the exact backup declaration before shared activity")
		}
		pins.PreviousOperation, pins.Previous, pins.DesiredHash = old.ID, old.Digest(), r.DesiredHash
		pins.Artifacts = maps.Clone(old.Pins.Artifacts)
		pins.Artifacts["shared-activity-policy"] = full
		if pins.Artifacts["command-backup-policy"] != "" {
			pins.Artifacts["command-backup-policy"] = full
		}
	}
	expected, valid := canonicalNotificationReceipt(predecessor)
	if !valid {
		return out, true, fmt.Errorf("shared activity predecessor has no gateway receipt")
	}
	step := &sharedActivityStep{directory: d, request: r, paths: p, expected: expected, lifecycle: l}
	_, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step})
	var pending sharedActivityQualificationRequired
	if errors.As(err, &pending) {
		return sharedActivityResult(out, step.intent), true, nil
	}
	if err != nil {
		return out, true, fmt.Errorf("shared activity coordinator: %w", err)
	}
	return sharedActivityResult(out, step.intent), true, nil
}
func sharedActivityResult(out ControlResult, v sharedActivityIntent) ControlResult {
	out.Code, out.State, out.Identity = "ok", "reporting_connected", &v.Current.Identity
	out.Next = "shared reporting restored; command intake remains disabled; backup capture requires separate qualification and explicit activation"
	if v.Disabled {
		out.State = "disabled"
		out.Next = "capture and reporting stopped; queues, history, original owner and legacy fence retained; restore all three activity controls to stage reactivation"
	} else if v.Phase != "complete" || v.Current.Mode == "verification" {
		out.State = "needs_qualification"
		out.Next = "gateway restored in verification; record fresh notification qualification and apply; command intake and backup capture remain disabled"
	}
	return out
}

type sharedActivityQualificationRequired struct{}

func (sharedActivityQualificationRequired) Error() string {
	return "fresh notification qualification required before shared activity can complete"
}

type sharedActivityStep struct {
	directory *safefs.Directory
	request   ControlRequest
	paths     ResolvedPaths
	expected  string
	lifecycle sharedActivityLifecycle
	intent    sharedActivityIntent
}

func (s *sharedActivityStep) Name() string { return "notification-shared-activity" }
func (s *sharedActivityStep) read(op string) (sharedActivityIntent, error) {
	var v sharedActivityIntent
	err := s.directory.ReadJSON("shared-activity-"+op+".json", &v)
	full, _ := s.request.Desired.Fingerprint()
	if err == nil && (validateSharedActivityIntent(v) != nil || v.Operation != op || protocol.Hash(v.Before) != s.expected || v.DesiredHash != full || v.Disabled != config.SharedActivityDisabled(s.request.Desired) || v.StopGateway != !config.WorkerEnabled(s.request.Desired.Notification.Enabled)) {
		err = fmt.Errorf("shared activity intent changed")
	}
	return v, err
}
func (s *sharedActivityStep) save(v sharedActivityIntent) error {
	if err := validateSharedActivityIntent(v); err != nil {
		return err
	}
	if err := s.directory.WriteJSON("shared-activity-"+v.Operation+".json", v, true); err != nil {
		return err
	}
	s.intent = v
	if s.lifecycle.boundary != nil {
		return s.lifecycle.boundary(v.Phase)
	}
	return nil
}
func (s *sharedActivityStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	v, err := s.read(op)
	if errors.Is(err, os.ErrNotExist) {
		o, e := s.lifecycle.inspect(ctx, s.paths.Installation)
		if e != nil {
			return "", false, e
		}
		if o.Phase != "complete" || protocol.Hash(o.Manifest) != s.expected {
			return "", false, fmt.Errorf("shared activity predecessor drift")
		}
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	s.intent = v
	if v.Phase != "complete" {
		return "", false, nil
	}
	o, err := s.lifecycle.inspect(ctx, s.paths.Installation)
	if err != nil {
		return "", false, err
	}
	if o.Phase != "complete" || protocol.Hash(o.Manifest) != protocol.Hash(v.Current) {
		return "", false, fmt.Errorf("shared activity canonical service changed")
	}
	status, err := s.lifecycle.status(ctx, s.request.Facts.Home)
	if err != nil {
		return "", false, err
	}
	if status.Stage != "complete" || status.DesiredHash != v.DesiredHash {
		return "", false, fmt.Errorf("shared activity canonical backup changed")
	}
	return protocol.Hash(v.Current), true, nil
}
func (s *sharedActivityStep) Run(ctx context.Context, op string) error {
	v, err := s.read(op)
	if errors.Is(err, os.ErrNotExist) {
		o, e := s.lifecycle.inspect(ctx, s.paths.Installation)
		if e != nil {
			return e
		}
		if o.Phase != "complete" || protocol.Hash(o.Manifest) != s.expected {
			return fmt.Errorf("shared activity predecessor changed")
		}
		full, _ := s.request.Desired.Fingerprint()
		v = sharedActivityIntent{Version: 1, Operation: op, DesiredHash: full, Before: o.Manifest, Current: o.Manifest, Disabled: config.SharedActivityDisabled(s.request.Desired), StopGateway: !config.WorkerEnabled(s.request.Desired.Notification.Enabled), RestoreLED: s.request.Desired.Notification.StatusLEDs.Enabled, Phase: "prepared"}
		if err = retainDeclaration(s.directory, s.request.Desired, full); err != nil {
			return err
		}
		if err = retainDeclaration(s.directory, s.request.Desired, s.request.DesiredHash, 1); err != nil {
			return err
		}
		if err = s.save(v); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	_, err = s.lifecycle.reconcile(ctx, s.request.Facts.Home, s.request.Desired, s.request.HelperSHA256, backup.SharedActivityRequest{Operation: op, GatewayRoot: s.paths.Installation, BeforeGateway: s.expected}, func() error {
		var e error
		if sharedActivityPhase(v.Phase) < 2 {
			if v.Disabled && v.RestoreLED && v.Current.Mode != "uninstalled" && (v.Current.LEDOutput == nil || !v.Current.LEDOutput.Disabled) {
				v.Current, e = s.lifecycle.output(ctx, s.paths.Installation, protocol.Hash(v.Current), true)
				if e != nil {
					return e
				}
			}
			v.Phase = "led-off"
			if e = s.save(v); e != nil {
				return e
			}
		}
		if sharedActivityPhase(v.Phase) < 3 {
			if v.Disabled && v.Current.Mode == "delivery" {
				v.Current, e = s.lifecycle.pause(ctx, install.DeliveryPauseRequest{Root: s.paths.Installation, ManifestHash: protocol.Hash(v.Current)})
				if e != nil {
					return e
				}
			}
			v.Phase = "paused"
			if e = s.save(v); e != nil {
				return e
			}
		}
		if sharedActivityPhase(v.Phase) < 4 {
			if v.StopGateway && v.Current.Mode != "uninstalled" {
				v.Current, e = s.lifecycle.service(ctx, s.paths.Installation, protocol.Hash(v.Current), true)
			} else if !v.StopGateway && (v.Current.Mode == "uninstalled" || !v.Disabled) {
				v.Current, e = s.lifecycle.service(ctx, s.paths.Installation, protocol.Hash(v.Current), false)
			}
			if e != nil {
				return e
			}
			v.Phase = "service"
			if e = s.save(v); e != nil {
				return e
			}
		}
		if sharedActivityPhase(v.Phase) < 5 {
			if !v.Disabled && v.Current.DeliveryOwnerHash != "" {
				if v.Admission == nil {
					request, ready, e := s.lifecycle.review(ctx, s.paths.Installation, protocol.Hash(v.Current))
					if e != nil {
						return e
					}
					if !ready {
						return sharedActivityQualificationRequired{}
					}
					v.Admission = &request
					if e = s.save(v); e != nil {
						return e
					}
				}
				v.Current, e = s.lifecycle.ensure(ctx, *v.Admission)
				if e != nil {
					return e
				}
			}
			v.Phase = "qualified"
			if e = s.save(v); e != nil {
				return e
			}
		}
		if sharedActivityPhase(v.Phase) < 6 {
			if !v.Disabled && v.RestoreLED && v.Current.LEDOutput != nil && v.Current.LEDOutput.Disabled {
				v.Current, e = s.lifecycle.output(ctx, s.paths.Installation, protocol.Hash(v.Current), false)
				if e != nil {
					return e
				}
			}
			v.Phase = "led-restored"
			if e = s.save(v); e != nil {
				return e
			}
		}
		observed, e := s.lifecycle.inspect(ctx, s.paths.Installation)
		if e != nil {
			return e
		}
		if observed.Phase != "complete" || protocol.Hash(observed.Manifest) != protocol.Hash(v.Current) {
			return fmt.Errorf("shared activity gateway changed before backup completion")
		}
		return nil
	})
	s.intent = v
	if err != nil {
		return err
	}
	if !v.Disabled && v.Current.DeliveryOwnerHash != "" {
		if err = s.lifecycle.reports(ctx, s.request.Facts.Home, op, protocol.Hash(v.Current)); err != nil {
			return err
		}
	}
	v.Phase = "complete"
	return s.save(v)
}

// Short commands may resend original operator inputs. Activity can compare
// those bytes with retained target files, but cannot rotate or publish them.
func verifySharedActivityProvidedInputs(ctx context.Context, r ControlRequest, p ResolvedPaths) error {
	raw, err := readLimitsRuntime(p.Runtime)
	if err != nil {
		return err
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return err
	}
	paths := map[string]string{"token": input.Providers["telegram"].TokenFile, "chat_id": input.Providers["telegram"].ChatFile}
	if _, has := r.Inputs["signal_account"]; has || r.Inputs["signal_recipient"] != nil {
		o, e := install.InspectRetainedInstallation(ctx, p.Installation)
		if e != nil {
			return e
		}
		if o.Manifest.FreshSignal == nil {
			return fmt.Errorf("shared activity has no retained native inputs")
		}
		native, e := install.InspectFreshSignal(ctx, *o.Manifest.FreshSignal, false)
		if e != nil {
			return e
		}
		paths["signal_account"], paths["signal_recipient"] = native.Spec.AccountFile, native.Spec.RecipientFile
		if selected := input.Providers["signal"].SignalDelivery; selected != nil {
			paths["signal_account"], paths["signal_recipient"] = selected.AccountFile, selected.RecipientFile
		}
	}
	for field, value := range r.Inputs {
		path := paths[field]
		if path == "" {
			return fmt.Errorf("unexpected shared activity input")
		}
		retained, e := ReadCredentialInput(path)
		if e != nil || inputDigest(retained) != inputDigest(value) {
			return fmt.Errorf("shared activity cannot change retained provider inputs; apply provider changes separately")
		}
	}
	return nil
}

func verifySharedActivityHistory(d *safefs.Directory, j OperationJournal) error {
	for depth := 0; depth < 128; depth++ {
		if !sharedActivityJournal(j) {
			return nil
		}
		v, err := readSharedActivityIntent(d, j)
		if err != nil {
			return err
		}
		prior, err := sharedActivityPredecessor(d, j)
		if err != nil {
			return err
		}
		receipt, valid := canonicalNotificationReceipt(prior)
		if !valid || protocol.Hash(v.Before) != receipt {
			return fmt.Errorf("shared activity history changed its gateway predecessor")
		}
		j = prior
	}
	return fmt.Errorf("shared activity history exceeds review bound")
}
