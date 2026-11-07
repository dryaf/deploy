package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type notificationActivityLifecycle struct {
	inspect       func(context.Context, string) (install.InstallationObservation, error)
	stop, restore func(context.Context, string, string) (install.ManagedManifest, error)
	output        func(context.Context, string, string, bool) (install.ManagedManifest, error)
}

// This first activity lifecycle is deliberately limited to unbound gateways:
// stopping a gateway with producers requires their explicit dependency fence.
// The activity receipt distinguishes a YAML disable from explicit uninstall.
func reconcileNotificationActivity(ctx context.Context, r ControlRequest, paths ResolvedPaths, lifecycle notificationActivityLifecycle) (ControlResult, bool, error) {
	result := ControlResult{Version: 1}
	disabled := !config.WorkerEnabled(r.Desired.Notification.Enabled)
	d, err := safefs.Open(paths.Control)
	if errors.Is(err, os.ErrNotExist) {
		if !disabled {
			return result, false, nil
		}
		// Absence of the coordinator is not proof that the runtime is absent.
		for _, p := range []string{paths.Installation, paths.State, paths.Runtime} {
			if _, e := os.Lstat(p); !errors.Is(e, os.ErrNotExist) {
				return result, true, fmt.Errorf("unowned worker state requires import or recovery review before disable")
			}
		}
		return ControlResult{Version: 1, State: "not_installed", Code: "ok", Next: "worker is disabled in YAML; no installation created"}, true, nil
	}
	if err != nil {
		return result, true, err
	}
	defer d.Close()
	var old OperationJournal
	if err = d.ReadJSON("operation.json", &old); err != nil {
		return result, true, err
	}
	if err = validateOperationJournal(old); err != nil {
		return result, true, err
	}
	isActivity := activityJournal(old)
	if !disabled && !isActivity {
		return result, false, nil
	}
	if old.Pins.Action != "apply" || old.Pins.Worker != r.Worker || old.Pins.UID != r.Facts.UID || old.Pins.Home != r.Facts.Home || old.Pins.Artifacts["helper"] != r.HelperSHA256 || old.Pins.DesiredScope != r.DesiredScope || old.Pins.Installations["notification-worker"] != paths.Installation || old.Pins.Installations["state"] != paths.State || old.Pins.Installations["runtime"] != paths.Runtime || len(old.Children) != 1 {
		return result, true, fmt.Errorf("activity change differs from retained installation authority; explicit uninstall cannot be re-enabled by apply")
	}
	pins, predecessor := old.Pins, old
	retry := isActivity && old.Pins.DesiredHash == r.DesiredHash
	if retry {
		predecessor = OperationJournal{}
		if err = d.ReadJSON("history/"+old.Pins.PreviousOperation+".json", &predecessor); err != nil {
			return result, true, err
		}
		if validateOperationJournal(predecessor) != nil || predecessor.ID != old.Pins.PreviousOperation || predecessor.Digest() != old.Pins.Previous {
			return result, true, fmt.Errorf("activity predecessor authority changed")
		}
	} else {
		if !old.Complete {
			return result, true, fmt.Errorf("finish the pending activity with its original YAML before changing policy")
		}
		pins.Previous, pins.PreviousOperation = old.Digest(), old.ID
		pins.DesiredHash = r.DesiredHash
	}
	if !predecessor.Complete || !unboundPredecessor(predecessor) {
		return result, true, fmt.Errorf("dependent consumers require coordinated activity migration")
	}
	before, err := readDeclaration(d, predecessor.Pins.DesiredHash, predecessor.Pins.DesiredScope)
	if err != nil {
		return result, true, err
	}
	if before.Notification == nil {
		return result, true, fmt.Errorf("activity predecessor declaration missing")
	}
	next := before
	n := *before.Notification
	next.Notification = &n
	next.Notification.Enabled = r.Desired.Notification.Enabled
	hash, err := declarationFingerprint(next, r.DesiredScope)
	if err != nil || hash != r.DesiredHash || config.WorkerEnabled(before.Notification.Enabled) == !disabled {
		// After an enable, an unrelated update belongs to its specific lifecycle.
		if !disabled && !retry && config.WorkerEnabled(before.Notification.Enabled) {
			return result, false, nil
		}
		return result, true, fmt.Errorf("activity change accepts only notification_worker.enabled; apply other changes separately")
	}
	if !disabled && !activityJournal(predecessor) {
		return result, true, fmt.Errorf("re-enable requires the completed YAML disable receipt")
	}
	if !retry {
		if err = requireUnreservedNotificationPolicy(r, paths, before, predecessor); err != nil {
			return result, true, err
		}
		o, e := lifecycle.inspect(ctx, paths.Installation)
		if e != nil {
			return result, true, e
		}
		mode := "verification"
		if !disabled {
			mode = "uninstalled"
		}
		if !o.Unbound || o.Phase != "complete" || o.Manifest.Mode != mode || protocol.Hash(o.Manifest) != predecessor.Children[0].Receipt || o.Manifest.DeliveryOwnerHash != "" || o.Manifest.RecoveryCommitHash != "" {
			return result, true, fmt.Errorf("activity requires exact unbound authority; bound producers require coordinated dependency migration")
		}
		if err = verifyActivityReceipt(d, predecessor, o.Manifest); err != nil {
			return result, true, err
		}
		if err = verifyReleaseReceipt(d, predecessor, o.Manifest); err != nil {
			return result, true, err
		}
	}
	if !disabled {
		if err = verifyLEDOutputInputs(r, paths, d, before); err != nil {
			return result, true, err
		}
	}
	step := &notificationActivityStep{directory: d, paths: paths, expected: predecessor.Children[0].Receipt, disabled: disabled, desired: r.Desired, desiredHash: r.DesiredHash, desiredScope: r.DesiredScope, lifecycle: lifecycle}
	if record, e := (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step}); e != nil {
		if activityJournal(record) && !record.Complete {
			return result, true, notificationActivityPendingError{r.Server, e}
		}
		return result, true, e
	}
	result.Code, result.State = "ok", "disabled"
	result.Identity, result.LEDOutput = &step.manifest.Identity, step.manifest.LEDOutput
	result.Next = "gateway service disabled; state, credentials, fault history and any native Signal account service retained; set enabled: true and apply to restore verification"
	if !disabled {
		result.State, result.Next = "waiting_for_producers", "gateway verification restored with retained identity; producer enrollment and qualification remain required"
	}
	return result, true, nil
}

func activityJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-activity"
}

type notificationActivityIntent struct {
	Version     int                     `json:"version"`
	Operation   string                  `json:"operation"`
	Before      install.ManagedManifest `json:"before"`
	Current     install.ManagedManifest `json:"current"`
	Disabled    bool                    `json:"disabled"`
	LEDs        bool                    `json:"leds"`
	DesiredHash string                  `json:"desired_hash"`
	Phase       string                  `json:"phase"`
}

type notificationActivityStep struct {
	directory             *safefs.Directory
	paths                 ResolvedPaths
	expected, desiredHash string
	desiredScope          int
	desired               config.DesiredWorkers
	disabled              bool
	lifecycle             notificationActivityLifecycle
	manifest              install.ManagedManifest
}

func (s *notificationActivityStep) Name() string { return "notification-activity" }
func (s *notificationActivityStep) intent(op string) (notificationActivityIntent, error) {
	var v notificationActivityIntent
	err := s.directory.ReadJSON("activity-"+op+".json", &v)
	if err == nil && (v.Version != 1 || v.Operation != op || protocol.Hash(v.Before) != s.expected || v.Disabled != s.disabled || v.DesiredHash != s.desiredHash || (v.Phase != "prepared" && v.Phase != "output-disabled" && v.Phase != "restored" && v.Phase != "complete")) {
		err = fmt.Errorf("activity intent changed")
	}
	if err == nil {
		err = validateActivityIntent(v)
	}
	return v, err
}
func (s *notificationActivityStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	o, err := s.lifecycle.inspect(ctx, s.paths.Installation)
	if err != nil {
		return "", false, err
	}
	v, e := s.intent(op)
	if errors.Is(e, os.ErrNotExist) && protocol.Hash(o.Manifest) == s.expected && o.Phase == "complete" {
		return "", false, nil
	}
	if e != nil {
		return "", false, fmt.Errorf("activity receipt unavailable: %w", e)
	}
	if v.Phase != "complete" {
		return "", false, nil
	}
	if o.Phase != "complete" || !o.Unbound || protocol.Hash(o.Manifest) != protocol.Hash(v.Current) || (o.Manifest.Mode == "uninstalled") != s.disabled {
		return "", false, fmt.Errorf("canonical worker activity changed")
	}
	if _, err = readDeclaration(s.directory, s.desiredHash, s.desiredScope); err != nil {
		return "", false, err
	}
	s.manifest = o.Manifest
	return protocol.Hash(o.Manifest), true, nil
}
func (s *notificationActivityStep) Run(ctx context.Context, op string) error {
	v, err := s.intent(op)
	if errors.Is(err, os.ErrNotExist) {
		o, e := s.lifecycle.inspect(ctx, s.paths.Installation)
		if e != nil {
			return e
		}
		m := o.Manifest
		mode := "verification"
		if !s.disabled {
			mode = "uninstalled"
		}
		if !o.Unbound || o.Phase != "complete" || m.Mode != mode || protocol.Hash(m) != s.expected || m.DeliveryOwnerHash != "" || m.RecoveryCommitHash != "" {
			return fmt.Errorf("activity requires the exact unbound verification installation")
		}
		v = notificationActivityIntent{Version: 1, Operation: op, Before: m, Current: m, Disabled: s.disabled, LEDs: o.LEDs, DesiredHash: s.desiredHash, Phase: "prepared"}
		err = s.directory.WriteJSON("activity-"+op+".json", v, false)
	}
	if err != nil {
		return err
	}
	if err = retainDeclaration(s.directory, s.desired, s.desiredHash, s.desiredScope); err != nil {
		return err
	}
	save := func(phase string, m install.ManagedManifest) error {
		v.Current, v.Phase = m, phase
		if err := validateActivityIntent(v); err != nil {
			return err
		}
		return s.directory.WriteJSON("activity-"+op+".json", v, true)
	}
	if s.disabled {
		if v.Phase == "prepared" {
			m := v.Current
			if v.LEDs && (m.LEDOutput == nil || !m.LEDOutput.Disabled) {
				m, err = s.lifecycle.output(ctx, s.paths.Installation, protocol.Hash(v.Current), true)
				if err != nil {
					return err
				}
				want := v.Current
				want.LEDOutput = m.LEDOutput
				if m.LEDOutput == nil || !m.LEDOutput.Disabled || protocol.Hash(want) != protocol.Hash(m) {
					return fmt.Errorf("activity LED cleanup changed installation")
				}
			}
			if err = save("output-disabled", m); err != nil {
				return err
			}
		}
		if v.Phase == "output-disabled" {
			m, e := s.lifecycle.stop(ctx, s.paths.Installation, protocol.Hash(v.Current))
			if e != nil {
				return e
			}
			want := v.Current
			want.Mode = "uninstalled"
			if protocol.Hash(want) != protocol.Hash(m) {
				return fmt.Errorf("activity stop changed retained state")
			}
			return save("complete", m)
		}
	} else {
		if v.Phase == "prepared" {
			m, e := s.lifecycle.restore(ctx, s.paths.Installation, protocol.Hash(v.Current))
			if e != nil {
				return e
			}
			if m.Identity != v.Before.Identity || m.Installation != v.Before.Installation || m.Mode != "verification" || m.ConfigHash != v.Before.ConfigHash || m.RuntimeHash != v.Before.RuntimeHash || m.BinaryHash != v.Before.BinaryHash || m.Root != v.Before.Root || m.Paths.State != v.Before.Paths.State || protocol.Hash(m.FreshSignal) != protocol.Hash(v.Before.FreshSignal) {
				return fmt.Errorf("activity restore changed identity or policy")
			}
			if err = save("restored", m); err != nil {
				return err
			}
		}
		if v.Phase == "restored" {
			m := v.Current
			if v.LEDs && s.desired.Notification.StatusLEDs.Enabled && m.LEDOutput != nil && m.LEDOutput.Disabled {
				m, err = s.lifecycle.output(ctx, s.paths.Installation, protocol.Hash(v.Current), false)
				if err != nil {
					return err
				}
				want := v.Current
				want.LEDOutput = m.LEDOutput
				if m.LEDOutput == nil || m.LEDOutput.Disabled || protocol.Hash(want) != protocol.Hash(m) {
					return fmt.Errorf("activity LED restore changed installation")
				}
			}
			return save("complete", m)
		}
	}
	if v.Phase != "complete" {
		return fmt.Errorf("activity phase does not match desired transition")
	}
	return nil
}

type notificationActivityPendingError struct {
	Server string
	Cause  error
}

func (e notificationActivityPendingError) Error() string { return e.Cause.Error() }
func (e notificationActivityPendingError) Unwrap() error { return e.Cause }

func validateActivityIntent(v notificationActivityIntent) error {
	want := v.Before
	if v.Phase == "prepared" {
		if protocol.Hash(want) != protocol.Hash(v.Current) {
			return fmt.Errorf("prepared activity changed predecessor")
		}
		return nil
	}
	if v.Disabled {
		if v.Phase != "output-disabled" && v.Phase != "complete" {
			return fmt.Errorf("invalid disable phase")
		}
		if v.LEDs {
			if v.Current.LEDOutput == nil || !v.Current.LEDOutput.Disabled || !safefs.ValidID(v.Current.LEDOutput.Operation, "n") {
				return fmt.Errorf("disabled activity lacks output cleanup")
			}
			want.LEDOutput = v.Current.LEDOutput
		}
		if v.Phase == "complete" {
			want.Mode = "uninstalled"
		}
	} else {
		if v.Phase != "restored" && v.Phase != "complete" {
			return fmt.Errorf("invalid enable phase")
		}
		// A retained-artifact update may change only service revision paths and
		// the unit digest, followed by the separately audited output permission.
		want.Mode, want.Revision, want.UnitHash = "verification", v.Current.Revision, v.Current.UnitHash
		want.Paths.Executable, want.Paths.Config = v.Current.Paths.Executable, v.Current.Paths.Config
		if v.Phase == "complete" && v.LEDs {
			want.LEDOutput = v.Current.LEDOutput
		}
	}
	if protocol.Hash(want) != protocol.Hash(v.Current) {
		return fmt.Errorf("activity changed retained identity, policy or paths")
	}
	return nil
}

func verifyActivityReceipt(d *safefs.Directory, j OperationJournal, m install.ManagedManifest) error {
	if !activityJournal(j) {
		return nil
	}
	var before OperationJournal
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &before); err != nil {
		return err
	}
	if validateOperationJournal(before) != nil || !before.Complete || before.Digest() != j.Pins.Previous || before.ID != j.Pins.PreviousOperation || len(before.Children) != 1 {
		return fmt.Errorf("activity predecessor missing or changed")
	}
	var v notificationActivityIntent
	if err := d.ReadJSON("activity-"+j.Children[0].Operation+".json", &v); err != nil {
		return err
	}
	if v.Version != 1 || v.Operation != j.Children[0].Operation || v.DesiredHash != j.Pins.DesiredHash || v.Phase != "complete" || protocol.Hash(v.Before) != before.Children[0].Receipt || protocol.Hash(v.Current) != protocol.Hash(m) || protocol.Hash(m) != j.Children[0].Receipt {
		return fmt.Errorf("activity completion authority differs")
	}
	return validateActivityIntent(v)
}

func activityStoppedPredecessor(d *safefs.Directory, j OperationJournal) (bool, error) {
	if !activityJournal(j) {
		return false, nil
	}
	var v notificationActivityIntent
	if err := d.ReadJSON("activity-"+j.Children[0].Operation+".json", &v); err != nil {
		return false, err
	}
	if err := verifyActivityReceipt(d, j, v.Current); err != nil {
		return false, err
	}
	return v.Disabled, nil
}

func continueActivityEnrollment(ctx context.Context, r ControlRequest, paths ResolvedPaths, executable string, result ControlResult) (ControlResult, error) {
	d, err := safefs.Open(paths.Control)
	if err != nil {
		return result, err
	}
	defer d.Close()
	var old OperationJournal
	if err = d.ReadJSON("operation.json", &old); err != nil {
		return result, err
	}
	info, err := os.Lstat(paths.Runtime)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxControlFrame {
		return result, fmt.Errorf("retained activity runtime unavailable")
	}
	raw, err := readPrivateGeneratedInput(paths.Runtime, int(info.Size()))
	if err != nil {
		return result, err
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return result, err
	}
	return reconcileReservedBackup(ctx, r, paths, executable, d, old, input, result)
}
