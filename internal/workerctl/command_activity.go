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

type commandActivityLifecycle struct {
	notificationCommandLifecycle
	inputs            func(ControlRequest, ResolvedPaths, *safefs.Directory, config.DesiredWorkers) error
	scope             func(context.Context, string, backup.VerificationNotificationScopeChange, func(backup.ProducerReservation) (string, error)) (string, error)
	scopeInspect      func(string, string, string, string) (string, bool, error)
	reviewEnrollment  func(context.Context, string, string, string) (install.CommandPolicyRequest, error)
	scopeEnrollment   func(context.Context, string, backup.VerificationNotificationScopeChange, func(backup.ProducerReservation) (string, error)) (string, error)
	reviewRestore     func(context.Context, string, string, string) (install.CommandPolicyRequest, error)
	reviewPermissions func(context.Context, string, string, string) (install.CommandPolicyRequest, error)
	scopePermissions  func(context.Context, string, backup.VerificationNotificationScopeChange, func(backup.ProducerReservation) (string, error)) (string, error)
}

func defaultCommandActivity() commandActivityLifecycle {
	return commandActivityLifecycle{notificationCommandLifecycle: defaultNotificationCommands(), inputs: func(r ControlRequest, p ResolvedPaths, d *safefs.Directory, before config.DesiredWorkers) error {
		return verifyLEDOutputInputs(r, p, d, before)
	}, scope: backup.ReconcileCommandNotificationScope, scopeInspect: backup.InspectVerificationNotificationScope, reviewRestore: install.ReviewCommandPermissionRestore, reviewPermissions: install.ReviewCommandNarrowing, scopePermissions: backup.ReconcileCommandPolicyScope, reviewEnrollment: install.ReviewCommandEnrollment, scopeEnrollment: backup.ReconcileCommandEnrollmentScope}
}

func commandActivityJournal(j OperationJournal) bool {
	return j.Pins.Action == "apply" && len(j.Children) == 1 && (j.Children[0].Name == "notification-command-enrollment" || j.Children[0].Name == "notification-command-activity" || j.Children[0].Name == "notification-command-permissions")
}

func validateCommandActivityPolicy(before, after config.DesiredWorkers) error {
	if before.Notification == nil || after.Notification == nil || before.Backup == nil || after.Backup == nil || before.Notification.Commands.Enabled == after.Notification.Commands.Enabled {
		return fmt.Errorf("command activity requires a changed commands.enabled setting")
	}
	n := *after.Notification
	n.Commands.Enabled = before.Notification.Commands.Enabled
	if !reflect.DeepEqual(n, *before.Notification) || !reflect.DeepEqual(before.Backup, after.Backup) {
		return fmt.Errorf("change commands.enabled separately from other worker settings")
	}
	return nil
}

func commandActivityPredecessor(d *safefs.Directory, current OperationJournal) (OperationJournal, error) {
	var prior OperationJournal
	if validateOperationJournal(current) != nil || !commandActivityJournal(current) {
		return prior, fmt.Errorf("invalid command activity journal")
	}
	if err := d.ReadJSON("history/"+current.Pins.PreviousOperation+".json", &prior); err != nil {
		return prior, err
	}
	if validateOperationJournal(prior) != nil || !prior.Complete || !admittedCommandHistory(prior) || prior.ID != current.Pins.PreviousOperation || prior.Digest() != current.Pins.Previous || prior.Pins.DesiredScope != 1 || current.Pins.DesiredScope != 1 {
		return prior, fmt.Errorf("command activity lost its completed predecessor")
	}
	before, err := readCommandDeclaration(d, prior)
	if err != nil {
		return prior, err
	}
	after, err := readCommandDeclaration(d, current)
	if err != nil {
		return prior, err
	}
	before, err = deliveryBackupBefore(before, after, "commands")
	if err != nil {
		return prior, err
	}
	validate := validateCommandActivityPolicy
	if commandPermissionsJournal(current) {
		validate = validateCommandPermissionsPolicy
	} else if commandEnrollmentJournal(current) {
		validate = validateCommandEnrollmentPolicy
	}
	if err = validate(before, after); err != nil {
		return prior, err
	}
	want := commandActivityPins(prior, after, current.Pins.DesiredHash, current.Pins.Artifacts["command-activity-qualification"])
	if commandEnrollmentJournal(current) {
		want.Artifacts["command-enrollment-overlay"] = protocol.Hash("command-enrollment-overlay-v1")
	}
	if !reflect.DeepEqual(want, current.Pins) || after.Notification.Commands.Enabled && !validDigest(current.Pins.Artifacts["command-activity-qualification"]) || !after.Notification.Commands.Enabled && current.Pins.Artifacts["command-activity-qualification"] != "" {
		return prior, fmt.Errorf("command activity changed its pinned authority")
	}
	return prior, nil
}

func commandActivityPins(prior OperationJournal, desired config.DesiredWorkers, scope, qualification string) OperationPins {
	pins := prior.Pins
	pins.PreviousOperation, pins.Previous, pins.DesiredHash = prior.ID, prior.Digest(), scope
	pins.Artifacts = maps.Clone(prior.Pins.Artifacts)
	pins.Artifacts["command-backup-policy"], _ = desired.Fingerprint()
	delete(pins.Artifacts, "command-activity-qualification")
	if qualification != "" {
		pins.Artifacts["command-activity-qualification"] = qualification
	}
	return pins
}

// Every toggle retains the original receiver, SQL results and delivery ancestor.
// Walking only these checked records cannot authorize a new mode or broader permissions.
func retainedCommandReceiver(ctx context.Context, d *safefs.Directory, j OperationJournal, l notificationCommandLifecycle) (backup.CommandReceiverRequest, error) {
	for depth := 0; sharedActivityJournal(j) || commandActivityJournal(j) || deliveryOutputJournal(j) || commandPauseJournal(j) || commandDeliveryJournal(j) || commandDeliveryPolicyJournal(j) || commandDeclarationJournal(j); depth++ {
		if depth >= 128 {
			return backup.CommandReceiverRequest{}, fmt.Errorf("command history exceeds review bound")
		}
		var err error
		if sharedActivityJournal(j) {
			if _, err = readSharedActivityIntent(d, j); err != nil {
				return backup.CommandReceiverRequest{}, err
			}
			j, err = sharedActivityPredecessor(d, j)
		} else if commandDeliveryPolicyJournal(j) || commandDeclarationJournal(j) {
			if _, err = readDeliveryProviderIntent(d, j); err != nil {
				return backup.CommandReceiverRequest{}, err
			}
			j, err = deliveryProviderPredecessor(d, j)
		} else if commandPauseJournal(j) || commandDeliveryJournal(j) {
			j, err = commandRenewalPredecessor(d, j)
		} else if deliveryOutputJournal(j) {
			if _, err = readDeliveryOutputIntent(d, j); err != nil {
				return backup.CommandReceiverRequest{}, err
			}
			j, err = deliveryOutputPredecessor(d, j)
		} else {
			j, err = commandActivityPredecessor(d, j)
		}
		if err != nil {
			return backup.CommandReceiverRequest{}, err
		}
	}
	if !j.Complete {
		return backup.CommandReceiverRequest{}, fmt.Errorf("finish initial command admission first")
	}
	if _, err := commandPredecessor(d, j); err != nil {
		return backup.CommandReceiverRequest{}, err
	}
	desired, err := readCommandDeclaration(d, j)
	if err != nil {
		return backup.CommandReceiverRequest{}, err
	}
	if desired.Notification == nil || !desired.Notification.Commands.Enabled || (desired.Notification.Commands.Mode != "read" && desired.Notification.Commands.Mode != "mutation") {
		return backup.CommandReceiverRequest{}, fmt.Errorf("original command declaration is unavailable")
	}
	version := 1
	if desired.Notification.Commands.Mode == "mutation" {
		version = 2
	}
	step := backupCommandReceiverStep{version: version, directory: d, lifecycle: l}
	pin, done, err := step.Inspect(ctx, j.Children[0].Operation)
	if err != nil {
		return step.request, err
	}
	if !done || pin != j.Children[0].Receipt {
		return step.request, fmt.Errorf("original command receiver authority changed")
	}
	return step.request, nil
}

func commandActivityRequest(paths ResolvedPaths, prior OperationJournal, desired config.DesiredWorkers, qualification string) install.CommandPolicyRequest {
	receipt, _ := canonicalNotificationReceipt(prior)
	r := install.CommandPolicyRequest{Root: paths.Installation, ManifestHash: receipt, Operation: "disable-commands"}
	if desired.Notification.Commands.Enabled {
		r.Operation, r.Mode, r.QualificationHash = "enable-commands", "read", qualification
		if desired.Notification.Commands.Mode == "mutation" {
			r.Mode = "all"
		}
	}
	return r
}

func reconcileCommandActivity(ctx context.Context, r ControlRequest, paths ResolvedPaths, l commandActivityLifecycle) (ControlResult, bool, error) {
	result := ControlResult{Version: 1}
	d, err := safefs.Open(paths.Control)
	if errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	}
	if err != nil {
		return result, true, err
	}
	defer d.Close()
	var current OperationJournal
	if err = d.ReadJSON("operation.json", &current); errors.Is(err, os.ErrNotExist) {
		return result, false, nil
	} else if err != nil {
		return result, true, err
	}
	if !admittedCommandHistory(current) {
		return result, false, nil
	}
	if validateOperationJournal(current) != nil {
		return result, true, fmt.Errorf("invalid command coordinator")
	}
	retry := commandActivityJournal(current) && current.Pins.DesiredHash == r.DesiredHash
	if !retry && current.Pins.DesiredHash == r.DesiredHash {
		return result, false, nil
	}
	if !retry && !current.Complete {
		return result, true, fmt.Errorf("resume the retained command operation before changing YAML")
	}
	prior := current
	if retry {
		prior, err = commandActivityPredecessor(d, current)
		if err != nil {
			return result, true, err
		}
	}
	before, err := readCommandDeclaration(d, prior)
	if err != nil {
		return result, true, err
	}
	if r.Desired.Notification == nil || before.Notification == nil {
		return result, true, fmt.Errorf("command declaration missing")
	}
	connectPolicy, err := r.Desired.Fingerprint()
	if err != nil {
		return result, true, err
	}
	if retry && current.Pins.Artifacts["command-backup-policy"] != connectPolicy {
		if !current.Complete || l.backupReview == nil {
			return result, true, fmt.Errorf("finish retained command activity before applying a changed backup policy")
		}
		retained, e := readCommandDeclaration(d, current)
		if e != nil {
			return result, true, e
		}
		if err = reviewDeliveryBackup(ctx, r, retained, current.Pins.DesiredHash, "commands", deliveryProviderLifecycle{backupReview: l.backupReview}); err != nil {
			return result, true, err
		}
		// Inspect the completed operation with its original declaration. Only the
		// reporting continuation uses the separately verified current backup policy.
		r.Desired = retained
	}
	permissionsChanged := !reflect.DeepEqual(before.Notification.Commands.Permissions, r.Desired.Notification.Commands.Permissions)
	enrollmentChanged := before.Notification.Commands.Mode != r.Desired.Notification.Commands.Mode || !reflect.DeepEqual(before.Notification.Commands.Workers, r.Desired.Notification.Commands.Workers) || !reflect.DeepEqual(before.Notification.Commands.Conversations, r.Desired.Notification.Commands.Conversations)
	enrollment := enrollmentChanged || permissionsChanged && prior.Pins.Artifacts["command-enrollment-overlay"] == protocol.Hash("command-enrollment-overlay-v1") || retry && commandEnrollmentJournal(current)
	if !retry && !enrollment && !permissionsChanged && r.Desired.Notification.Commands.Enabled == before.Notification.Commands.Enabled {
		return result, false, nil
	}
	if !prior.Complete || r.DesiredScope != 1 || prior.Pins.DesiredScope != 1 || prior.Pins.Worker != r.Worker || prior.Pins.UID != r.Facts.UID || prior.Pins.Home != r.Facts.Home || prior.Pins.Artifacts["helper"] != r.HelperSHA256 || prior.Pins.Installations["notification-worker"] != paths.Installation || prior.Pins.Installations["state"] != paths.State || prior.Pins.Installations["runtime"] != paths.Runtime {
		return result, true, fmt.Errorf("command activity requires unchanged target, paths and helper")
	}
	if !retry && !reflect.DeepEqual(before.Backup, r.Desired.Backup) {
		if err = reviewDeliveryBackup(ctx, r, before, prior.Pins.DesiredHash, "commands", deliveryProviderLifecycle{backupReview: l.backupReview}); err != nil {
			return result, true, err
		}
	}
	before, err = deliveryBackupBefore(before, r.Desired, "commands")
	if err != nil {
		return result, true, err
	}
	validate := validateCommandActivityPolicy
	if permissionsChanged {
		validate = validateCommandPermissionsPolicy
	}
	if enrollment {
		validate = validateCommandEnrollmentPolicy
	}
	if err = validate(before, r.Desired); err != nil {
		return result, true, err
	}
	if err = l.inputs(r, paths, d, before); err != nil {
		return result, true, err
	}
	receiver, err := retainedCommandReceiver(ctx, d, prior, l.notificationCommandLifecycle)
	if err != nil {
		return result, true, err
	}
	qualification := ""
	if retry {
		qualification = current.Pins.Artifacts["command-activity-qualification"]
	}
	request := commandActivityRequest(paths, prior, r.Desired, qualification)
	if permissionsChanged || enrollment {
		request, err = commandPolicyRequest(paths, prior, r.Desired, receiver, enrollment, before)
		if err != nil {
			return result, true, err
		}
		if request.Operation == "restore-command-permissions" {
			enrollment = true
		}
		if !retry {
			review := l.reviewPermissions
			if enrollment {
				review = l.reviewEnrollment
			}
			if request.Operation == "restore-command-permissions" {
				review = l.reviewRestore
			}
			if review == nil {
				return result, true, fmt.Errorf("command restoration review unavailable")
			}
			reviewed, e := review(ctx, paths.Installation, request.ManifestHash, request.Policy)
			if e != nil {
				return result, true, e
			}
			if request != reviewed {
				return result, true, fmt.Errorf("permission review changed pinned policy")
			}
		}
	}
	if !retry && r.Desired.Notification.Commands.Enabled {
		reviewed, ready, e := l.review(ctx, paths.Installation, request.ManifestHash, request.Mode)
		if e != nil {
			return result, true, e
		}
		if !ready {
			result.State, result.Code, result.Next = "needs_qualification", "command_qualification_required", "commands remain disabled; current delivery qualification is required before re-enabling; inspect notification-worker status before renewing delivery admission"
			return result, true, nil
		}
		qualification = reviewed.QualificationHash
		request.QualificationHash = qualification
		if !validDigest(qualification) || request != reviewed {
			return result, true, fmt.Errorf("command review changed activity pins")
		}
	}
	if !retry && deliveryOutputJournal(prior) {
		m, _, e := l.inspect(ctx, request)
		if e != nil {
			return result, true, e
		}
		if e = verifyDeliveryOutputReceipt(d, prior, m, l.scopeInspect); e != nil {
			return result, true, e
		}
	}
	pins := commandActivityPins(prior, r.Desired, r.DesiredHash, qualification)
	if enrollment {
		pins.Artifacts["command-enrollment-overlay"] = protocol.Hash("command-enrollment-overlay-v1")
	}
	step := &commandActivityStep{request: request, control: r, prior: prior, receiver: receiver, lifecycle: l}
	if err = retainDeclaration(d, r.Desired, r.DesiredHash, 1); err != nil {
		return result, true, err
	}
	if !retry {
		if err = retainDeclaration(d, r.Desired, pins.Artifacts["command-backup-policy"]); err != nil {
			return result, true, err
		}
	}
	if _, err = (JournalCoordinator{Directory: d}).Apply(ctx, pins, []LifecycleStep{step}); err != nil {
		return result, true, commandActivityPendingError{r.Server, err}
	}
	if err = l.connect(ctx, r.Facts.Home, connectPolicy, protocol.Hash(step.manifest)); err != nil {
		return result, true, commandActivityPendingError{r.Server, err}
	}
	result.Code, result.State, result.Identity = "ok", "commands_disabled", &step.manifest.Identity
	result.Next = "new command intake disabled; reporting, receiver and command results retained"
	if enrollment {
		result.Next = "command enrollment updated; intake remains disabled; reporting and saved results retained"
		if request.Operation == "restore-command-permissions" {
			result.Next = "originally admitted command permissions restored; intake remains disabled; reporting and saved results retained"
		}
	}
	if permissionsChanged && !enrollment {
		result.Next = "command permissions reduced; intake remains disabled; reporting and saved command results retained"
	}
	if r.Desired.Notification.Commands.Enabled {
		result.State, result.Next = "commands_admitted", "declared commands re-enabled with the retained receiver; reporting and command results retained"
	}
	return result, true, nil
}

type commandActivityPendingError struct {
	Server string
	Cause  error
}

func (e commandActivityPendingError) Error() string { return e.Cause.Error() }
func (e commandActivityPendingError) Unwrap() error { return e.Cause }

type commandActivityStep struct {
	request   install.CommandPolicyRequest
	control   ControlRequest
	prior     OperationJournal
	receiver  backup.CommandReceiverRequest
	lifecycle commandActivityLifecycle
	manifest  install.ManagedManifest
}

func (s *commandActivityStep) Name() string {
	if s.request.Operation == "migrate-commands" || s.request.Operation == "restore-command-permissions" {
		return "notification-command-enrollment"
	}
	if s.request.Operation == "narrow-commands" {
		return "notification-command-permissions"
	}
	return "notification-command-activity"
}
func (s *commandActivityStep) inspectGateway(ctx context.Context) (string, bool, error) {
	m, done, err := s.lifecycle.inspect(ctx, s.request)
	if err != nil || !done {
		return "", done, err
	}
	mode := s.request.Mode
	if s.request.Operation != "enable-commands" {
		mode = "drain"
	}
	if m.Root != s.request.Root || m.Mode != "delivery" || m.Identity != s.receiver.Identity || m.DeliveryActivation == nil || m.Commands == nil || m.Commands.Mode != mode {
		return "", false, fmt.Errorf("command activity returned different gateway authority")
	}
	s.manifest = m
	return protocol.Hash(m), true, nil
}
func (s *commandActivityStep) Inspect(ctx context.Context, op string) (string, bool, error) {
	receipt, done, err := s.inspectGateway(ctx)
	if err != nil || !done {
		return receipt, done, err
	}
	confirmed, done, err := s.lifecycle.scopeInspect(s.control.Facts.Home, op, s.request.ManifestHash, s.control.DesiredHash)
	if err != nil || !done {
		return "", false, err
	}
	if confirmed != receipt {
		return "", false, fmt.Errorf("command activity and backup declaration receipts differ")
	}
	return receipt, true, nil
}
func (s *commandActivityStep) Run(ctx context.Context, op string) error {
	scope := s.lifecycle.scope
	if s.request.Operation == "narrow-commands" {
		scope = s.lifecycle.scopePermissions
	} else if s.request.Operation == "migrate-commands" || s.request.Operation == "restore-command-permissions" {
		scope = s.lifecycle.scopeEnrollment
	}
	_, err := scope(ctx, s.control.Facts.Home, backup.VerificationNotificationScopeChange{Operation: op, BeforeScope: s.prior.Pins.DesiredHash, BeforeGateway: s.request.ManifestHash, Desired: s.control.Desired}, func(p backup.ProducerReservation) (string, error) {
		if p.Producer != s.receiver.Producer {
			return "", fmt.Errorf("command activity backup producer changed")
		}
		if receipt, done, err := s.inspectGateway(ctx); err != nil || done {
			return receipt, err
		}
		if _, err := s.lifecycle.ensure(ctx, s.request); err != nil {
			return "", err
		}
		receipt, done, err := s.inspectGateway(ctx)
		if err != nil {
			return "", err
		}
		if !done {
			return "", fmt.Errorf("command activity has no gateway completion receipt")
		}
		return receipt, nil
	})
	return err
}

func verifyCommandActivityReceipt(ctx context.Context, d *safefs.Directory, current OperationJournal, m install.ManagedManifest, l commandActivityLifecycle) error {
	prior, err := commandActivityPredecessor(d, current)
	if err != nil {
		return err
	}
	desired, err := readCommandDeclaration(d, current)
	if err != nil {
		return err
	}
	receiver, err := retainedCommandReceiver(ctx, d, current, l.notificationCommandLifecycle)
	if err != nil {
		return err
	}
	request := commandActivityRequest(ResolvedPaths{Installation: m.Root}, prior, desired, current.Pins.Artifacts["command-activity-qualification"])
	if commandPermissionsJournal(current) || commandEnrollmentJournal(current) {
		before, e := readCommandDeclaration(d, prior)
		if e != nil {
			return e
		}
		request, err = commandPolicyRequest(ResolvedPaths{Installation: m.Root}, prior, desired, receiver, commandEnrollmentJournal(current), before)
		if err != nil {
			return err
		}
	}
	step := commandActivityStep{request: request, control: ControlRequest{Facts: TargetFacts{Home: current.Pins.Home}, DesiredHash: current.Pins.DesiredHash}, prior: prior, receiver: receiver, lifecycle: l}
	pin, done, err := step.Inspect(ctx, current.Children[0].Operation)
	if err != nil {
		return err
	}
	if !done || pin != current.Children[0].Receipt || pin != protocol.Hash(m) {
		return fmt.Errorf("command activity authority changed")
	}
	return nil
}
