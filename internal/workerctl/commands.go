package workerctl

import (
	"context"
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

type notificationCommandLifecycle struct {
	review          func(context.Context, string, string, string) (install.CommandPolicyRequest, bool, error)
	inspect         func(context.Context, install.CommandPolicyRequest) (install.ManagedManifest, bool, error)
	ensure          func(context.Context, install.CommandPolicyRequest) (install.ManagedManifest, error)
	receiverReview  func(context.Context, string, string, string, string) (backup.CommandReceiverRequest, error)
	receiverInspect func(context.Context, backup.CommandReceiverRequest) (string, bool, error)
	receiverStage   func(context.Context, backup.CommandReceiverRequest) (string, error)
	connect         func(context.Context, string, string, string) error
	backupReview    func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error)
}

func defaultNotificationCommands() notificationCommandLifecycle {
	return notificationCommandLifecycle{install.ReviewCommandAdmission, install.InspectCommandPolicy, install.EnsureCommandPolicy, backup.ReviewSharedCommandReceiver, backup.InspectSharedCommandReceiver, backup.StageSharedCommandReceiver, backup.ConnectSharedReporting, backup.InspectSetupPolicy}
}

func commandJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 2 && j.Children[0].Name == "backup-command-receiver" && j.Children[1].Name == "notification-commands"
}

func commandPredecessor(d *safefs.Directory, j OperationJournal) (OperationJournal, error) {
	var prior OperationJournal
	if validateOperationJournal(j) != nil || !commandJournal(j) {
		return prior, fmt.Errorf("invalid command coordinator history")
	}
	if err := d.ReadJSON("history/"+j.Pins.PreviousOperation+".json", &prior); err != nil {
		return prior, err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || prior.ID != j.Pins.PreviousOperation || prior.Digest() != j.Pins.Previous || len(prior.Children) != 1 || prior.Children[0].Name != "notification-delivery" {
		return prior, fmt.Errorf("command admission lost its completed delivery predecessor")
	}
	want := prior.Pins
	want.PreviousOperation, want.Previous = prior.ID, prior.Digest()
	want.Artifacts = maps.Clone(want.Artifacts)
	want.Artifacts["command-qualification"] = j.Pins.Artifacts["command-qualification"]
	want.Artifacts["command-backup-policy"] = j.Pins.Artifacts["command-backup-policy"]
	if !reflect.DeepEqual(want, j.Pins) || !validDigest(want.Artifacts["command-qualification"]) || !validDigest(want.Artifacts["command-backup-policy"]) {
		return prior, fmt.Errorf("command admission changed predecessor authority")
	}
	return prior, nil
}

// The receiver is durable before gateway intake is admitted. Each child retains
// its own domain receipt; an uncertain result is inspected before any retry.
func advanceNotificationCommands(ctx context.Context, r ControlRequest, paths ResolvedPaths, d *safefs.Directory, old OperationJournal, result ControlResult, l notificationCommandLifecycle) (ControlResult, error) {
	if r.Desired.Notification == nil || !r.Desired.Notification.Commands.Enabled || (r.Desired.Notification.Commands.Mode != "read" && r.Desired.Notification.Commands.Mode != "mutation") || r.Desired.Backup == nil || r.Desired.Backup.Notifications.Worker != r.Server {
		return result, fmt.Errorf("initial command admission requires declared local backup commands in read or mutation mode")
	}
	if validateOperationJournal(old) != nil || old.Pins.Action != "apply" || old.Pins.Worker != r.Worker || old.Pins.DesiredHash != r.DesiredHash || old.Pins.DesiredScope != r.DesiredScope || old.Pins.UID != r.Facts.UID || old.Pins.Home != r.Facts.Home || old.Pins.Artifacts["helper"] != r.HelperSHA256 || old.Pins.Installations["notification-worker"] != paths.Installation || old.Pins.Installations["state"] != paths.State || old.Pins.Installations["runtime"] != paths.Runtime {
		return result, fmt.Errorf("command admission changed pinned target, artifacts or declaration")
	}
	mode, receiverVersion := "read", 1
	if r.Desired.Notification.Commands.Mode == "mutation" {
		mode, receiverVersion = "all", 2
	}
	backupDesired, err := r.Desired.Fingerprint()
	if err != nil {
		return result, err
	}
	pins := old.Pins
	var request install.CommandPolicyRequest
	if commandJournal(old) {
		prior, e := commandPredecessor(d, old)
		if e != nil {
			return result, e
		}
		if old.Pins.Artifacts["command-backup-policy"] != backupDesired {
			if !old.Complete || l.backupReview == nil {
				return result, fmt.Errorf("finish retained command admission before applying a changed backup policy")
			}
			if err = reviewDeliveryBackup(ctx, r, r.Desired, r.DesiredHash, "commands", deliveryProviderLifecycle{backupReview: l.backupReview}); err != nil {
				return result, err
			}
		}
		request = install.CommandPolicyRequest{Root: paths.Installation, ManifestHash: prior.Children[0].Receipt, Operation: "enable-commands", Mode: mode, QualificationHash: old.Pins.Artifacts["command-qualification"]}
	} else {
		if !old.Complete || len(old.Children) != 1 || old.Children[0].Name != "notification-delivery" {
			return result, fmt.Errorf("complete shared delivery before command admission")
		}
		var ready bool
		request, ready, err = l.review(ctx, paths.Installation, old.Children[0].Receipt, mode)
		if err != nil {
			return result, err
		}
		if !ready {
			result.State, result.Code, result.Next = "needs_qualification", "command_qualification_required", "command admission requires current recorded qualification; reporting admission remains separate"
			return result, nil
		}
		if request.Root != paths.Installation || request.ManifestHash != old.Children[0].Receipt || request.Operation != "enable-commands" || request.Mode != mode || request.Policy != "" || !validDigest(request.QualificationHash) {
			return result, fmt.Errorf("command review returned different admission pins")
		}
		pins.PreviousOperation, pins.Previous = old.ID, old.Digest()
		pins.Artifacts = maps.Clone(old.Pins.Artifacts)
		pins.Artifacts["command-qualification"], pins.Artifacts["command-backup-policy"] = request.QualificationHash, backupDesired
	}
	if !commandJournal(old) {
		if err = retainDeclaration(d, r.Desired, backupDesired); err != nil {
			return result, err
		}
	}
	receiver := &backupCommandReceiverStep{version: receiverVersion, directory: d, home: r.Facts.Home, desired: backupDesired, manifest: request.ManifestHash, lifecycle: l}
	admission := &notificationCommandStep{request: request, receiver: receiver, lifecycle: l}
	if _, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{receiver, admission}); err != nil {
		return result, err
	}
	if err = l.connect(ctx, r.Facts.Home, backupDesired, protocol.Hash(admission.manifest)); err != nil {
		return result, err
	}
	result.Version, result.State, result.Code, result.Identity = 1, "commands_admitted", "ok", &admission.manifest.Identity
	result.Next = "declared commands admitted; the shared report timer processes commands; backup capture qualification and activation remain separate"
	return result, nil
}

type backupCommandReceiverStep struct {
	version                 int
	directory               *safefs.Directory
	home, desired, manifest string
	lifecycle               notificationCommandLifecycle
	request                 backup.CommandReceiverRequest
	complete                bool
}

func (s *backupCommandReceiverStep) Name() string { return "backup-command-receiver" }
func (s *backupCommandReceiverStep) read(op string) (backup.CommandReceiverRequest, error) {
	var v backup.CommandReceiverRequest
	err := s.directory.ReadJSON("command-receiver-"+op+".json", &v)
	if err == nil && (v.Operation != op || v.Version != s.version || !v.Identity.Valid() || !v.Producer.Valid()) {
		err = fmt.Errorf("retained receiver request changed")
	}
	return v, err
}
func (s *backupCommandReceiverStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	r, err := s.read(op)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	pin, complete, err := s.lifecycle.receiverInspect(ctx, r)
	if err == nil {
		s.request, s.complete = r, complete
	}
	return pin, complete, err
}
func (s *backupCommandReceiverStep) Run(ctx context.Context, op string) error {
	r, err := s.read(op)
	if os.IsNotExist(err) {
		r, err = s.lifecycle.receiverReview(ctx, s.home, s.desired, s.manifest, op)
		if err != nil {
			return err
		}
		if r.Operation != op || r.Version != s.version || !r.Identity.Valid() || !r.Producer.Valid() {
			return fmt.Errorf("receiver review changed retained operation")
		}
		pin, complete, e := s.lifecycle.receiverInspect(ctx, r)
		if e != nil {
			return e
		}
		if pin != "" || complete {
			return fmt.Errorf("receiver setup exists without its coordinator request; recovery required")
		}
		err = s.directory.WriteJSON("command-receiver-"+op+".json", r, false)
	}
	if err != nil {
		return err
	}
	reviewed, err := s.lifecycle.receiverReview(ctx, s.home, s.desired, s.manifest, op)
	if err != nil {
		return err
	}
	if protocol.Hash(reviewed) != protocol.Hash(r) {
		return fmt.Errorf("retained receiver request differs from current completed reporting")
	}
	_, err = s.lifecycle.receiverStage(ctx, r)
	return err
}

type notificationCommandStep struct {
	request   install.CommandPolicyRequest
	receiver  *backupCommandReceiverStep
	lifecycle notificationCommandLifecycle
	manifest  install.ManagedManifest
}

func (s *notificationCommandStep) Name() string { return "notification-commands" }
func (s *notificationCommandStep) Inspect(ctx context.Context, _ string) (string, bool, error) {
	if !s.receiver.complete {
		return "", false, fmt.Errorf("command receiver must complete before admission")
	}
	m, complete, err := s.lifecycle.inspect(ctx, s.request)
	if err != nil || !complete {
		return "", complete, err
	}
	if m.Root != s.request.Root || m.Mode != "delivery" || m.Identity != s.receiver.request.Identity || m.DeliveryActivation == nil || m.Commands == nil || m.Commands.Mode != s.request.Mode {
		return "", false, fmt.Errorf("command admission returned different gateway authority")
	}
	s.manifest = m
	return protocol.Hash(m), true, nil
}
func (s *notificationCommandStep) Run(ctx context.Context, _ string) error {
	if !s.receiver.complete {
		return fmt.Errorf("command receiver must complete before admission")
	}
	_, err := s.lifecycle.ensure(ctx, s.request)
	return err
}
