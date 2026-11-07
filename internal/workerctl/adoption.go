package workerctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type notificationAdoptionLifecycle struct {
	review   func(context.Context, string) (install.PolicyReview, error)
	boundary func(string) error
}

const notificationAdoptionStage = ".notification-worker-adoption"

type notificationAdoptionIntent struct {
	Signal   *signalAdoptionRecord   `json:"signal,omitempty"`
	Version  int                     `json:"version"`
	Journal  OperationJournal        `json:"journal"`
	Manifest install.ManagedManifest `json:"manifest"`
	Runtime  []byte                  `json:"runtime"`
	Inputs   notificationInputPins   `json:"inputs"`
}

func adoptionJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && j.Children[0].Name == "notification-adopt"
}

func validateAdoptionIntent(v notificationAdoptionIntent) error {
	j, m := v.Journal, v.Manifest
	if (v.Version != 1 && v.Version != 2) || validateOperationJournal(j) != nil || !adoptionJournal(j) || j.Complete || j.Children[0].Complete || j.Pins.Previous != "" || j.Pins.PreviousOperation != "" || j.Pins.Worker != "notification-worker" || v.Inputs.Version != v.Version || v.Inputs.Operation != j.Children[0].Operation || v.Inputs.Runtime != inputDigest(v.Runtime) || !validDigest(v.Inputs.Token) || !validDigest(v.Inputs.Chat) || m.RuntimeHash != v.Inputs.Runtime || m.BinaryHash != j.Pins.Artifacts["helper"] || m.Root != j.Pins.Installations["notification-worker"] || m.Paths.State != j.Pins.Installations["state"] {
		return fmt.Errorf("adoption intent authority changed")
	}
	var input install.RuntimeInput
	if err := safefs.DecodeJSON(v.Runtime, &input, maxControlFrame); err != nil {
		return err
	}
	resolved, err := input.Resolve()
	if err != nil || protocol.Hash(resolved) != m.ConfigHash {
		return fmt.Errorf("adoption runtime differs from managed authority")
	}
	if !input.Bootstrap || len(input.Worker.Producers) != 0 || input.Worker.Interaction != nil || len(input.Worker.LEDBindings) != 0 || input.Telegram != nil || input.Signal != nil || m.Mode != "verification" || m.Commands != nil || m.CommandHistoryHash != "" || m.DeliveryOwnerHash != "" || m.DeliveryActivation != nil || m.DeliveryRecovery != nil || m.RecoveryCommitHash != "" || m.SignalProfiles != nil || m.SignalVerification != nil || m.TelegramRenewalID != "" {
		return fmt.Errorf("adoption requires an unbound direct-provider verification gateway")
	}
	if v.Version == 1 {
		if v.Signal != nil || m.FreshSignal != nil || v.Inputs.SignalAccount != "" || v.Inputs.SignalRecipient != "" || len(input.Providers) != 1 || input.Providers["telegram"].Kind != "telegram" {
			return fmt.Errorf("Telegram adoption authority changed")
		}
	} else {
		if input.Providers["signal"].Kind != "signal" || len(input.Providers) > 2 || !validDigest(v.Inputs.SignalAccount) || !validDigest(v.Inputs.SignalRecipient) {
			return fmt.Errorf("native adoption provider authority changed")
		}
		if err := validateSignalAdoption(v); err != nil {
			return err
		}
	}
	return nil
}

func adoptionPins(r ControlRequest, paths ResolvedPaths) OperationPins {
	pins := OperationPins{DesiredScope: r.DesiredScope, Worker: r.Worker, Action: "apply", DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Installations: map[string]string{"notification-worker": paths.Installation, "state": paths.State, "runtime": paths.Runtime}, Artifacts: map[string]string{"helper": r.HelperSHA256}}
	if p, ok := r.Desired.Notification.Providers["signal"]; ok {
		pins.Artifacts["signal"] = p.ExecutableSHA256
	}
	return pins
}

func verifyAdoptionPolicy(r ControlRequest, paths ResolvedPaths, review install.PolicyReview) (notificationInputPins, error) {
	var pins notificationInputPins
	n := r.Desired.Notification
	if n == nil || !config.WorkerEnabled(n.Enabled) || len(r.Inputs) != 0 || r.LocalLED != nil || r.LocalSignal != nil || r.LEDSHA256 != "" || (n.StatusLEDs.Enabled || review.Input.LEDSocket != "") && n.StatusLEDs.Daemon.Mode == "managed" {
		return pins, fmt.Errorf("adoption requires saved target-file references and excludes host daemon takeover")
	}
	p := n.Providers["telegram"]
	if _, ok := n.Providers["telegram"]; ok && (p.TokenInput != "" || p.ChatIDInput != "" || p.TokenFile == "" || p.ChatIDFile == "") {
		return pins, fmt.Errorf("save the reviewed target-private Telegram references before adoption")
	}
	m := review.Manifest
	if m.Root != paths.Installation || m.Paths.State != paths.State || m.Paths.Socket != paths.Socket || m.UnitDirectory != paths.UnitDirectory || m.BinaryHash != r.HelperSHA256 || !reflect.DeepEqual(m.Paths.DeniedPaths, paths.DeniedPaths) || m.RuntimeHash != inputDigest(review.Runtime) {
		return pins, fmt.Errorf("adoption cannot change installation paths, helper or runtime authority")
	}
	secrets := map[string][]byte{}
	if err := readDeclaredTelegramInputs(r, paths, secrets); err != nil {
		return pins, err
	}
	var input install.RuntimeInput
	var err error
	if _, ok := n.Providers["signal"]; ok {
		input, err = compileSignalAdoption(r, paths, review, secrets)
	} else {
		if r.SignalSHA256 != "" {
			return pins, fmt.Errorf("undeclared native artifact")
		}
		input, err = CompileUnboundNotification(r.Desired, paths, r.Facts.Home, secrets)
	}
	if err != nil {
		return pins, err
	}
	if review.Input.LEDSocket != "" {
		// Output disablement preserves the endpoint; it is separately journaled.
		if m.LEDOutput != nil && m.LEDOutput.Disabled {
			if n.StatusLEDs.Enabled {
				return pins, fmt.Errorf("adoption must retain disabled LED output")
			}
			input.LEDSocket = review.Input.LEDSocket
		} else if !n.StatusLEDs.Enabled {
			return pins, fmt.Errorf("adoption must retain existing LED output policy")
		}
		input.LEDEndpoint = review.Input.LEDEndpoint
	}
	preserveRuntimeRouteLabels(review.Input, &input)
	if protocol.Hash(input) != protocol.Hash(review.Input) {
		return pins, fmt.Errorf("YAML differs from the retained runtime; import and review before adoption")
	}
	pins = notificationInputPins{Version: 1, Runtime: inputDigest(review.Runtime), Token: inputDigest(secrets["token"]), Chat: inputDigest(secrets["chat_id"])}
	if review.Native != nil {
		pins.Version = 2
		pins.SignalAccount, pins.SignalRecipient = inputDigest(secrets["signal_account"]), inputDigest(secrets["signal_recipient"])
	}
	return pins, nil
}

// A complete pending outer journal is published atomically. Staging can be
// retried before publication, but a missing journal in the public control root
// remains recovery, never permission to take over existing runtime state.
func stageNotificationAdoption(ctx context.Context, r ControlRequest, paths ResolvedPaths, l notificationAdoptionLifecycle) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	review, err := l.review(ctx, paths.Installation)
	if err != nil {
		return err
	}
	inputs, err := verifyAdoptionPolicy(r, paths, review)
	if err != nil {
		return err
	}
	if b := r.Desired.Backup; b != nil && b.Notifications.Worker != "" {
		_, present, err := backup.InspectReservedProducer(r.Facts.Home, r.DesiredHash, paths.Installation, b.Notifications.Route)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("existing backup reservation requires shared adoption")
		}
	}
	parent, err := ensureConfigDirectory(filepath.Dir(paths.Control))
	if err != nil {
		return err
	}
	defer parent.Close()
	stageName := notificationAdoptionStage
	if err = parent.Mkdir(stageName); err != nil {
		return err
	}
	stage, err := ensureConfigDirectory(filepath.Join(filepath.Dir(paths.Control), stageName))
	if err != nil {
		return err
	}
	defer stage.Close()
	if info, e := stage.File().Stat(); e != nil || info.Mode().Perm() != 0700 {
		return fmt.Errorf("adoption staging directory must be private")
	}
	lock, err := stage.Lock("operation.lock", true, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	var intent notificationAdoptionIntent
	if err = stage.ReadJSON("adoption.json", &intent); errors.Is(err, os.ErrNotExist) {
		// Only a previously empty staging area may reserve adoption. Missing
		// intent after any dependent publication is lost authority, not a retry.
		entries, e := stage.File().ReadDir(-1)
		if e != nil {
			return e
		}
		for _, entry := range entries {
			if entry.Name() != "operation.lock" && !safefs.IsJSONTemporary(entry.Name()) {
				return fmt.Errorf("staged adoption intent missing; recovery required")
			}
		}
		if len(entries) > 64 {
			return fmt.Errorf("adoption staging requires temporary-file review")
		}
		id, e := safefs.NewID("n")
		if e != nil {
			return e
		}
		child, e := safefs.NewID("n")
		if e != nil {
			return e
		}
		inputs.Operation = child
		intent = notificationAdoptionIntent{Version: 1, Journal: OperationJournal{Version: 1, ID: id, Pins: adoptionPins(r, paths), Children: []ChildReference{{Name: "notification-adopt", Operation: child}}}, Manifest: review.Manifest, Runtime: review.Runtime, Inputs: inputs}
		if inputs.Version == 2 {
			intent.Version = 2
			intent.Signal, err = newSignalAdoption(r, paths, review)
			if err != nil {
				return err
			}
		}
		if err = validateAdoptionIntent(intent); err != nil {
			return err
		}
		if err = stage.WriteJSON("adoption.json", intent, false); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	inputs.Operation = intent.Inputs.Operation
	if err = validateAdoptionIntent(intent); err != nil {
		return err
	}
	if !reflect.DeepEqual(intent.Journal.Pins, adoptionPins(r, paths)) || intent.Inputs != inputs || protocol.Hash(intent.Manifest) != protocol.Hash(review.Manifest) {
		return fmt.Errorf("pending adoption differs from reviewed policy or installation")
	}
	if err = adoptionBoundary(l, "adoption-staged"); err != nil {
		return err
	}
	if err = retainDeclaration(stage, r.Desired, r.DesiredHash, r.DesiredScope); err != nil {
		return err
	}
	if err = retainAdoptionStageRecord(stage, "notification-inputs.json", intent.Inputs); err != nil {
		return err
	}
	if err = retainAdoptionStageRecord(stage, "operation.json", intent.Journal); err != nil {
		return err
	}
	if err = stageSignalAdoption(stage, intent); err != nil {
		return err
	}
	if err = adoptionBoundary(l, "adoption-ready"); err != nil {
		return err
	}
	// Recheck the canonical domain after staging; no service mutation occurs.
	current, err := l.review(ctx, paths.Installation)
	if err != nil {
		return err
	}
	checked, err := verifyAdoptionPolicy(r, paths, current)
	checked.Operation = inputs.Operation
	if err != nil {
		return err
	}
	if intent.Signal != nil && (current.Native == nil || protocol.Hash(current.Native) != protocol.Hash(intent.Signal.Manifest)) {
		return fmt.Errorf("native service changed during adoption")
	}
	if protocol.Hash(current.Manifest) != protocol.Hash(intent.Manifest) || checked != inputs {
		return fmt.Errorf("installation changed during adoption review")
	}
	if err = parent.Rename(stageName, parent, filepath.Base(paths.Control)); err != nil {
		return err
	}
	return adoptionBoundary(l, "adoption-published")
}

func adoptionBoundary(l notificationAdoptionLifecycle, at string) error {
	if l.boundary != nil {
		return l.boundary(at)
	}
	return nil
}

func reconcileNotificationAdoption(ctx context.Context, r ControlRequest, paths ResolvedPaths, l notificationAdoptionLifecycle) (ControlResult, bool, error) {
	result := ControlResult{Version: 1}
	d, err := safefs.Open(paths.Control)
	if errors.Is(err, os.ErrNotExist) {
		if r.Action != "adopt" {
			stage, e := safefs.Open(filepath.Join(filepath.Dir(paths.Control), notificationAdoptionStage))
			if errors.Is(e, os.ErrNotExist) {
				return result, false, nil
			}
			if e != nil {
				return result, true, e
			}
			var v notificationAdoptionIntent
			e = stage.ReadJSON("adoption.json", &v)
			stage.Close()
			if e != nil {
				return result, true, fmt.Errorf("adoption staging incomplete; repeat explicit adopt with the reviewed YAML")
			}
			if e = validateAdoptionIntent(v); e != nil {
				return result, true, e
			}
			if !reflect.DeepEqual(v.Journal.Pins, adoptionPins(r, paths)) {
				return result, true, fmt.Errorf("pending adoption pins differ; restore reviewed YAML before resume")
			}
		}
		if err = stageNotificationAdoption(ctx, r, paths, l); err != nil {
			return result, true, err
		}
		d, err = safefs.Open(paths.Control)
	}
	if err != nil {
		return result, true, err
	}
	defer d.Close()
	var j OperationJournal
	if err = d.ReadJSON("operation.json", &j); err != nil {
		if errors.Is(err, os.ErrNotExist) && r.Action != "adopt" {
			// Fresh Signal setup can have a completed or interrupted native child
			// before its first gateway journal. It still has to pass the normal
			// child coordinator's checks; existing gateway state is never adopted.
			if _, e := os.Lstat(filepath.Join(paths.Control, "adoption.json")); errors.Is(e, os.ErrNotExist) {
				if _, e = os.Lstat(filepath.Join(paths.Control, "signal", "operation.json")); e == nil {
					absent := true
					for _, path := range []string{paths.Runtime, paths.State, paths.Installation} {
						if _, e = os.Lstat(path); !errors.Is(e, os.ErrNotExist) {
							absent = false
						}
					}
					if absent {
						return result, false, nil
					}
				}
			}
		}
		return result, true, fmt.Errorf("coordinator journal unavailable; recovery required")
	}
	if err = validateOperationJournal(j); err != nil {
		return result, true, err
	}
	if !adoptionJournal(j) {
		if r.Action == "adopt" {
			return result, true, fmt.Errorf("installation already has coordinator authority; use apply or resume")
		}
		return result, false, nil
	}
	if !reflect.DeepEqual(j.Pins, adoptionPins(r, paths)) {
		if j.Complete && r.Action != "adopt" {
			return result, false, nil
		}
		return result, true, fmt.Errorf("finish the pinned adoption before changing policy or artifacts")
	}
	s := &notificationAdoptionStep{directory: d, request: r, paths: paths, lifecycle: l}
	if _, err = (JournalCoordinator{Directory: d, Boundary: l.boundary}).Apply(ctx, j.Pins, []LifecycleStep{s}); err != nil {
		return result, true, err
	}
	result.State, result.Code = "waiting_for_producers", "ok"
	result.Identity, result.LEDOutput = &s.manifest.Identity, s.manifest.LEDOutput
	result.Next = "existing gateway adopted without restart; producer enrollment and qualification remain separate"
	return result, true, nil
}

type notificationAdoptionStep struct {
	directory *safefs.Directory
	request   ControlRequest
	paths     ResolvedPaths
	lifecycle notificationAdoptionLifecycle
	manifest  install.ManagedManifest
}

func (s *notificationAdoptionStep) Name() string { return "notification-adopt" }
func (s *notificationAdoptionStep) verify(ctx context.Context, op string) (notificationAdoptionIntent, error) {
	var v notificationAdoptionIntent
	if err := s.directory.ReadJSON("adoption.json", &v); err != nil {
		return v, err
	}
	if err := validateAdoptionIntent(v); err != nil {
		return v, err
	}
	if op != v.Inputs.Operation || !reflect.DeepEqual(v.Journal.Pins, adoptionPins(s.request, s.paths)) {
		return v, fmt.Errorf("adoption operation changed")
	}
	review, err := s.lifecycle.review(ctx, s.paths.Installation)
	if err != nil {
		return v, err
	}
	inputs, err := verifyAdoptionPolicy(s.request, s.paths, review)
	inputs.Operation = op
	if err != nil {
		return v, err
	}
	if inputs != v.Inputs || protocol.Hash(review.Manifest) != protocol.Hash(v.Manifest) {
		return v, fmt.Errorf("adoption domain or private inputs changed")
	}
	if v.Signal != nil && (review.Native == nil || protocol.Hash(review.Native) != protocol.Hash(v.Signal.Manifest)) {
		return v, fmt.Errorf("adopted native service changed")
	}
	if err = verifySignalAdoptionFiles(s.directory, v); err != nil {
		return v, err
	}
	var actual notificationInputPins
	if err = s.directory.ReadJSON("notification-inputs.json", &actual); err != nil {
		return v, err
	}
	if actual != v.Inputs {
		return v, fmt.Errorf("adoption private input authority changed")
	}
	_, err = readDeclaration(s.directory, s.request.DesiredHash, s.request.DesiredScope)
	return v, err
}
func (s *notificationAdoptionStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	v, err := s.verify(ctx, op)
	if err != nil {
		return "", false, err
	}
	raw, err := readLimitsRuntime(s.paths.Runtime)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if inputDigest(raw) != v.Inputs.Runtime {
		return "", false, fmt.Errorf("adoption runtime publication differs")
	}
	s.manifest = v.Manifest
	return protocol.Hash(v.Manifest), true, nil
}
func (s *notificationAdoptionStep) Run(ctx context.Context, op string) error {
	v, err := s.verify(ctx, op)
	if err != nil {
		return err
	}
	if err = publishPrivateInput(s.paths.Runtime, v.Runtime); err != nil {
		return err
	}
	return adoptionBoundary(s.lifecycle, "adoption-runtime")
}

func verifyAdoptionReceipt(d *safefs.Directory, j OperationJournal, m install.ManagedManifest) error {
	var v notificationAdoptionIntent
	if err := d.ReadJSON("adoption.json", &v); err != nil {
		return err
	}
	if err := validateAdoptionIntent(v); err != nil {
		return err
	}
	if !reflect.DeepEqual(j.Pins, v.Journal.Pins) || j.ID != v.Journal.ID || j.Children[0].Operation != v.Inputs.Operation || protocol.Hash(m) != protocol.Hash(v.Manifest) || j.Children[0].Receipt != protocol.Hash(m) {
		return fmt.Errorf("adoption completion receipt changed")
	}
	var pins notificationInputPins
	if err := d.ReadJSON("notification-inputs.json", &pins); err != nil {
		return err
	}
	if pins != v.Inputs {
		return fmt.Errorf("adoption input receipt changed")
	}
	raw, err := readLimitsRuntime(j.Pins.Installations["runtime"])
	if err != nil || inputDigest(raw) != pins.Runtime {
		return fmt.Errorf("adopted runtime changed")
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		return err
	}
	var credentials []struct{ path, hash string }
	if p, ok := input.Providers["telegram"]; ok {
		credentials = append(credentials, struct{ path, hash string }{p.TokenFile, pins.Token}, struct{ path, hash string }{p.ChatFile, pins.Chat})
	}
	if v.Signal != nil {
		if err = verifySignalAdoptionFiles(d, v); err != nil {
			return err
		}
		spec := v.Signal.Manifest.Spec
		credentials = append(credentials, struct{ path, hash string }{spec.AccountFile, pins.SignalAccount}, struct{ path, hash string }{spec.RecipientFile, pins.SignalRecipient})
	}
	for _, item := range credentials {
		value, e := ReadCredentialInput(item.path)
		if e != nil || inputDigest(value) != item.hash {
			return fmt.Errorf("adopted credential changed")
		}
	}
	if _, err = readDeclaration(d, j.Pins.DesiredHash, j.Pins.DesiredScope); err != nil {
		return err
	}
	return nil
}

func retainAdoptionStageRecord[T any](d *safefs.Directory, name string, want T) error {
	var saved T
	err := d.ReadJSON(name, &saved)
	if errors.Is(err, os.ErrNotExist) {
		return d.WriteJSON(name, want, false)
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(saved, want) {
		return fmt.Errorf("staged adoption record changed")
	}
	return nil
}
