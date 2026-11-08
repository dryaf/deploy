package workerctl

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func backupDesired(d config.DesiredWorkers) config.DesiredWorkers {
	if d.Backup == nil || d.Backup.Notifications.Worker == "" {
		d.Notification = nil
	}
	return d
}

// Retained snapshots are comparison evidence only. Return field names and
// transition classes; destination values and credential references stay private.
func notificationDiffPolicy(d config.DesiredWorkers) config.DesiredWorkers {
	scope := d.NotificationScope()
	projected := config.DesiredWorkers{Notification: scope.Notification}
	if scope.Backup != nil {
		projected.Backup = &config.BackupDeclaration{Freshness: scope.Backup.Freshness,
			Notifications: config.DesiredBackupNotifications{Worker: scope.Backup.Worker, Route: scope.Backup.Route}}
	}
	return projected
}

func plannedChanges(before, after config.DesiredWorkers) ControlResult {
	r := ControlResult{Version: 1, State: "up_to_date", Code: "ok", Next: "declared policy matches retained installation policy; status reports qualification and current activity separately"}
	r.Changes = PolicyChanges(before, after)
	if len(r.Changes) != 0 {
		r.State, r.Code = "planned", "policy_changed"
		r.Next = "review the listed changes; migration classes require their supported lifecycle and cannot be forced through ordinary apply"
	}
	return r
}

func planNotificationControl(ctx context.Context, r ControlRequest, paths ResolvedPaths, inspect func(context.Context, string) (install.InstallationObservation, error)) (ControlResult, error) {
	result := ControlResult{Version: 1}
	d, err := safefs.Open(paths.Control)
	if err != nil {
		return result, err
	}
	defer d.Close()
	lease, err := d.Lock("operation.lock", false, false)
	if err != nil {
		return result, err
	}
	defer lease.Close()
	var j OperationJournal
	if err = d.ReadJSON("operation.json", &j); err != nil {
		return result, err
	}
	if err = validateOperationJournal(j); err != nil {
		return result, err
	}
	if j.Pins.Worker != "notification-worker" || j.Pins.UID != r.Facts.UID || j.Pins.Home != r.Facts.Home || j.Pins.Installations["notification-worker"] != paths.Installation || j.Pins.Installations["state"] != paths.State || j.Pins.Installations["runtime"] != paths.Runtime {
		return result, fmt.Errorf("retained notification target or paths differ; inspect the old installation before path migration")
	}
	if !j.Complete {
		return ControlResult{Version: 1, State: "needs_action", Code: "pending_operation", Next: "finish the retained operation with its original YAML before planning another change"}, nil
	}
	observed, err := inspect(ctx, paths.Installation)
	if err != nil {
		return result, err
	}
	if receipt, valid := canonicalNotificationReceipt(j); observed.Phase != "complete" || !valid || receipt != protocol.Hash(observed.Manifest) {
		return result, fmt.Errorf("canonical notification installation differs from retained policy authority")
	}
	if deliveryProviderJournal(j) {
		if e := verifyDeliveryProviderReceipt(d, j, observed.Manifest, nil); e != nil {
			return result, e
		}
	}
	if commandDeliveryPolicyJournal(j) || commandDeclarationJournal(j) {
		if _, err = retainedCommandReceiver(ctx, d, j, defaultNotificationCommands()); err != nil {
			return result, err
		}
	}
	if requalificationJournal(j) {
		_, v, e := requalificationPredecessor(d, j)
		if e != nil {
			return result, e
		}
		if e = validateRequalificationManifest(v.Before, observed.Manifest); e != nil {
			return result, e
		}
		if e = verifyMappingScope(d, j, nil); e != nil {
			return result, e
		}
	}
	if deliveryOutputJournal(j) {
		if err = verifyDeliveryOutputReceipt(d, j, observed.Manifest, backup.InspectVerificationNotificationScope); err != nil {
			return result, err
		}
		if admittedCommandHistory(j) {
			if _, err = retainedCommandReceiver(ctx, d, j, defaultNotificationCommands()); err != nil {
				return result, err
			}
		}
	}
	if commandPauseJournal(j) || commandDeliveryJournal(j) {
		if err = verifyCommandRenewalReceipt(ctx, d, j, observed.Manifest, defaultCommandRequalification("")); err != nil {
			return result, err
		}
	}
	if commandActivityJournal(j) {
		if err = verifyCommandActivityReceipt(ctx, d, j, observed.Manifest, defaultCommandActivity()); err != nil {
			return result, err
		}
	}
	if err = verifyActivityReceipt(d, j, observed.Manifest); err != nil {
		return result, err
	}
	if err = verifyReleaseReceipt(d, j, observed.Manifest); err != nil {
		return result, err
	}
	if observed.Manifest.Mode == "uninstalled" && !activityJournal(j) {
		if err = verifyRemovalLEDReceipt(ctx, d, j, paths.Installation, inspect); err != nil {
			return result, err
		}
		return ControlResult{Version: 1, State: "needs_action", Code: "reenrollment_required", Next: "installation is removed; retained identity and history require explicit reenrollment"}, nil
	}
	if j.Pins.Action != "apply" {
		return result, fmt.Errorf("notification policy comparison requires a completed apply receipt")
	}
	before, err := readDeclaration(d, j.Pins.DesiredHash, j.Pins.DesiredScope)
	if sharedActivityJournal(j) {
		before, err = readCommandDeclaration(d, j)
	}
	if err != nil {
		return result, err
	}
	result = plannedChanges(notificationDiffPolicy(before), notificationDiffPolicy(r.Desired))
	if observed.Unbound && observed.Manifest.Mode == "verification" && len(result.Changes) > 0 {
		onlyLimits, onlyRoutes, onlyProviders := true, true, true
		for _, change := range result.Changes {
			if !strings.HasPrefix(change.Field, "notification_worker.providers.telegram.") {
				onlyProviders = false
			}
			if !strings.HasPrefix(change.Field, "notification_worker.routes.") {
				onlyRoutes = false
			}
			if !strings.HasPrefix(change.Field, "notification_worker.limits.") {
				onlyLimits = false
			}
		}
		if onlyRoutes {
			result.Next = "apply the reviewed route change before producer reservation; existing credentials remain pinned, empty production history is required, and qualification remains separate"
		}
		if onlyLimits {
			result.Next = "apply the reviewed limits change; existing database and free-space reserves are checked, verification restarts with the same identity, and qualification remains separate"
		}
		if onlyProviders && len(before.Notification.Providers) == len(r.Desired.Notification.Providers) && (len(before.Notification.Providers) == 1 || observed.Manifest.FreshSignal != nil) {
			result.Next = "before producer reservation, save new private target files and select their token_file/chat_id_file references; retain the old files and any unchanged native Signal account/service, then apply and explicitly test the selected destination"
		}
	}
	if observed.Unbound && observed.Manifest.Mode == "verification" && len(result.Changes) > 0 && validateCommandDraftDeclaration(before, r.Desired) == nil {
		result.Next = "apply the command declaration before backup producer reservation; retain the running gateway and credentials, then install the backup and qualify before command admission"
	}
	if !observed.Unbound && observed.Manifest.Mode == "verification" && enrolledNotificationJournal(j) && j.Pins.DesiredScope == 1 && r.DesiredScope == 1 && len(result.Changes) > 0 && validateRemappingPolicy(before, r.Desired, j.Pins.DesiredHash) == nil {
		result.Next = "apply the reviewed LED binding change while backup reporting is still staged; the gateway and backup retain their producer identity and advance policy together; keep other policy unchanged and qualify the new mapping before admission"
	}
	if !observed.Unbound && observed.Manifest.Mode == "verification" && enrolledNotificationJournal(j) && j.Pins.DesiredScope == 1 && r.DesiredScope == 1 && len(result.Changes) > 0 && validateEnrolledOutputPolicy(before, r.Desired, j.Pins.DesiredHash) == nil {
		result.Next = "apply the reviewed LED output permission while backup reporting is still staged; retain bindings, daemon and unresolved incidents, then qualify the current output policy before delivery admission"
	}
	if !observed.Unbound && observed.Manifest.Mode == "verification" && enrolledNotificationJournal(j) && j.Pins.DesiredScope == 1 && r.DesiredScope == 1 && validateSharedLimitsPolicy(before, r.Desired, j.Pins.DesiredHash) == nil {
		result.Next = "apply shared limits before delivery activation, with unchanged LED policy and command admission still pending; queued work and producer identity remain; limits smaller than retained data are refused"
	}
	if !observed.Unbound && observed.Manifest.Mode == "verification" && enrolledNotificationJournal(j) && j.Pins.DesiredScope == 1 && r.DesiredScope == 1 && validateSharedPolicy(before, r.Desired, j.Pins.DesiredHash, "freshness") == nil {
		result.Next = "apply only the local backup producer's max_silence_seconds/max_evidence_seconds before delivery activation; preserve other producer settings; existing observation times remain and changed effective policy requires qualification"
	}
	if !observed.Unbound && observed.Manifest.Mode == "verification" && enrolledNotificationJournal(j) && j.Pins.DesiredScope == 1 && r.DesiredScope == 1 {
		if validateSharedPolicy(before, r.Desired, j.Pins.DesiredHash, "command-activity") == nil {
			result.Next = "apply commands.enabled before delivery activation with the rest of the policy unchanged; preserve aliases and backup identity, restore the retained policy when ready, and qualify before receiver admission"
		}
		if validateSharedPolicy(before, r.Desired, j.Pins.DesiredHash, "command-policy") == nil {
			result.Next = "apply the revised command policy before delivery activation; retain backup identity and alias reservations, then qualify and review the receiver before intake admission"
		}
		if validateSharedPolicy(before, r.Desired, j.Pins.DesiredHash, "commands") == nil {
			result.Next = "apply the declared Telegram command policy before delivery activation; retain the backup reservation and alert credentials, then qualify and admit the receiver separately"
		}
		if validateSharedPolicy(before, r.Desired, j.Pins.DesiredHash, "routes") == nil {
			result.Next = "apply routes among existing providers before delivery activation; settle pending messages first, preserve producer subscriptions, then qualify the revised configuration"
		}
		if validateSharedPolicy(before, r.Desired, j.Pins.DesiredHash, "providers") == nil {
			result.Next = "save new private Telegram files and apply their token_file/chat_id_file references before delivery activation; retain all old files and any unchanged native Signal service; settle the queue, keep configured commands unadmitted and retain their bot account, then qualify the selected destination"
		}
		if validateSharedPolicy(before, r.Desired, j.Pins.DesiredHash, "producer") == nil {
			result.Next = "apply the existing local backup producer policy before delivery activation; identity and observation times remain; quota reductions must fit retained work, other access-policy changes require a settled queue, and changed effective policy requires qualification"
		}
	}
	if observed.Manifest.Mode == "delivery" && requalificationCandidate(j) && j.Pins.DesiredScope == 1 && r.DesiredScope == 1 && validateRequalificationBefore(observed.Manifest) == nil && config.ValidateDeliveryProviderDeclaration(before, r.Desired) == nil {
		result.Next = "save new private Telegram token/chat files, then apply; same-destination rotations retain queued work, while a changed bot or destination requires a settled queue; delivery pauses and retains reporting ownership; keep old files, qualify the selected credentials and destination, then apply"
	}
	if observed.Manifest.Mode == "delivery" && requalificationCandidate(j) && validateRequalificationBefore(observed.Manifest) == nil && config.ValidateDeliveryLimitsDeclaration(before, r.Desired) == nil {
		result.Next = "apply notification limits with other settings unchanged; retained queue must fit; delivery pauses and needs fresh qualification before the final apply"
	}
	if observed.Manifest.Mode == "delivery" && requalificationCandidate(j) && validateRequalificationBefore(observed.Manifest) == nil && config.ValidateDerivedFreshnessDeclaration(before, r.Desired) == nil {
		result.Next = "apply backup freshness separately; capture is staged and delivery pauses while both runtimes and the archive checkpoint update; qualify and apply notifications, then qualify and activate backup capture"
	}
	if observed.Manifest.Mode == "delivery" && requalificationCandidate(j) && validateRequalificationBefore(observed.Manifest) == nil && config.ValidateDeliveryProducerDeclaration(before, r.Desired) == nil {
		result.Next = "apply existing producer policy separately from other settings; quota reductions must fit retained work and access changes require a settled queue; delivery pauses for fresh qualification"
	}
	if observed.Manifest.Mode == "delivery" && requalificationCandidate(j) && validateRequalificationBefore(observed.Manifest) == nil && config.ValidateDeliveryProviderEnrollmentDeclaration(before, r.Desired) == nil {
		result.Next = "apply only the unused Telegram provider addition/removal beside unchanged owned Signal; settle pending work first; delivery pauses for fresh qualification"
	}
	if observed.Manifest.Mode == "delivery" && requalificationCandidate(j) && validateRequalificationBefore(observed.Manifest) == nil && config.ValidateDeliveryRoutesDeclaration(before, r.Desired) == nil {
		result.Next = "apply notification routes among existing providers with producer permissions unchanged; settle pending messages first; delivery pauses for fresh qualification"
	}

	if observed.Manifest.Mode == "delivery" && requalificationCandidate(j) && validateRequalificationBefore(observed.Manifest) == nil && config.ValidateDeliveryDeclarationChange(before, r.Desired) == nil {
		same, e := reviewDeliveryDeclarationRuntime(r, paths, d, j)
		if e != nil {
			return result, e
		}
		if same {
			result.Next = "record the YAML declaration without restarting delivery or renewing qualification; the effective configuration is unchanged"
		}
	}
	if observed.Manifest.Mode == "delivery" && deliveryOutputCandidate(j) && validateDeliveryOutputPolicy(before, r.Desired, j.Pins.DesiredHash) == nil {
		result.Next = "apply LED output permission with other settings unchanged; bindings and incidents remain; re-enable requires the original enabled-output qualification to remain valid"
	}
	commandBefore := before
	if normalized, e := deliveryBackupBefore(before, r.Desired, "commands"); e == nil {
		commandBefore = normalized
	}
	if admittedCommandHistory(j) && j.Pins.DesiredScope == 1 && r.DesiredScope == 1 && validateCommandActivityPolicy(commandBefore, r.Desired) == nil {
		result.Next = "apply the reviewed commands.enabled change with other settings unchanged; disabling stops new intake while reporting and saved command results remain; re-enabling requires current delivery qualification"
	}
	if observed.Manifest.Mode == "delivery" && admittedCommandHistory(j) && validateCommandRequalificationBefore(observed.Manifest) == nil && config.ValidateCommandDeliveryFreshnessDeclaration(commandBefore, r.Desired) == nil {
		result.Next = "apply only the existing backup producer heartbeat/evidence timing with commands disabled; original observations, receiver and queued work remain; qualify and apply to resume delivery, then explicitly enable commands"
	}
	if admittedCommandHistory(j) && validateCommandEnrollmentPolicy(commandBefore, r.Desired) == nil {
		result.Next = "apply command enrollment with intake disabled; added rights or redirected audiences are rejected; enable separately after completion"
	}
	if admittedCommandHistory(j) && validateCommandPermissionsPolicy(commandBefore, r.Desired) == nil {
		result.Next = "apply the reviewed permission change with intake disabled and other settings unchanged; reductions and restoration within originally admitted receiver authority are supported; enable separately when ready"
	}
	result = sharedConfigurationPlan(result, before, r.Desired, j, observed)
	if result.State == "planned" && admittedCommandHistory(j) && validateCommandRequalificationBefore(observed.Manifest) == nil && config.ValidateCommandDeliveryProducerDeclaration(commandBefore, r.Desired) == nil {
		same, e := reviewPolicyDeclarationRuntime(r, paths, d, j, "command-declaration")
		if e != nil {
			return result, e
		}
		if same {
			result.Next = "record the equivalent producer declaration in both workers; delivery, qualification, receiver restrictions, queued work and observations remain unchanged; enable commands separately when wanted"
		}
	}
	if result.State == "planned" && observed.Manifest.Mode == "delivery" && config.ValidateDeliveryReportRouteDeclaration(commandBefore, r.Desired) == nil {
		kind := "report-route-declaration"
		if admittedCommandHistory(j) {
			kind = "command-report-route-declaration"
		}
		same, e := reviewPolicyDeclarationRuntime(r, paths, d, j, kind)
		if e != nil {
			return result, e
		}
		if same {
			result.Next = "record the already allowed backup report route; runtime, delivery and qualification remain; frozen reports must settle and command intake stays disabled"
		} else {
			result.Next = "apply the selected backup report route through either worker; settle frozen reports and delivery messages; qualify the revised notification policy and apply; backup capture qualification remains separate"
		}
	}
	if j.Pins.DesiredScope != r.DesiredScope {
		result.State, result.Code = "needs_action", "policy_scope_conversion_required"
		result.Next = "retained installation uses the earlier whole-policy fingerprint; explicit import/conversion is required, even when the displayed policy is unchanged"
		if observed.Unbound && j.Pins.DesiredScope == 0 && r.DesiredScope == 1 {
			result.State = "planned"
			result.Next = "apply the unchanged notification policy to convert its retained scope without replacing identity or restarting"
		}
	} else if j.Pins.Artifacts["helper"] != r.HelperSHA256 {
		result.State, result.Code = "needs_action", "artifact_update_required"
		result.Next = "selected helper differs from the installed release; review an artifact update before applying"
		if observed.Unbound {
			result.State = "planned"
			result.Next = "apply the reviewed helper change with other notification policy unchanged; verification is restarted once, identity and history retained"
		}
	}
	if observed.Unbound && (len(result.Changes) != 0 || j.Pins.DesiredScope != r.DesiredScope || j.Pins.Artifacts["helper"] != r.HelperSHA256) {
		if err = requireUnreservedNotificationPolicy(r, paths, before, j); err != nil {
			if !errors.Is(err, errReservedNotificationConsumer) {
				return result, err
			}
			blocked := controlFailure(err)
			result.State, result.Code, result.Next = blocked.State, blocked.Code, blocked.Next
		}
	}
	return result, nil
}

func planBackupControl(ctx context.Context, r ControlRequest, inspect func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error)) (ControlResult, error) {
	result := ControlResult{Version: 1}
	if r.Desired.Backup == nil {
		return result, fmt.Errorf("backup declaration missing")
	}
	if err := validateSourceTargets(r); err != nil {
		return result, err
	}
	// Check the same path restrictions as apply without requiring complete setup
	// inputs. In particular, disabled policy still needs a useful change plan.
	if _, err := resolveBackupPaths(r.Desired.Backup.Paths, r.Facts); err != nil {
		return result, err
	}
	observed, before, err := inspect(ctx, r.Facts.Home)
	if err != nil {
		return result, err
	}
	if observed.Stage == "notification_policy_pending" {
		return ControlResult{Version: 1, State: "resumable", Code: "notification_policy_pending", Next: "deploy notification-worker resume " + r.Server + " with the exact retained notification policy declaration"}, nil
	}
	if observed.Stage == "source_replacement_pending" {
		return ControlResult{Version: 1, State: "resumable", Code: "source_replacement_pending", Next: backup.ErrSourceReplacementPending.Error()}, nil
	}
	if observed.Stage == "source_retirement_pending" {
		return ControlResult{Version: 1, State: "resumable", Code: "source_retirement_pending", Next: "resume the exact source-retirement YAML and retain the old source administrator endpoints; archives and incident dispositions remain retained; activation waits for revocation acknowledgements"}, nil
	}
	if observed.Stage == "source_enrollment_pending" {
		return ControlResult{Version: 1, State: "resumable", Code: "source_enrollment_pending", Next: "resume the exact source-addition declaration; local rollback retains new remote authorizations and cannot cancel enrollment"}, nil
	}
	if observed.Stage != "complete" {
		return ControlResult{Version: 1, State: "needs_action", Code: "installation_review_required", Next: "finish pending setup/removal or review legacy adoption before applying changed policy"}, nil
	}
	if before == nil {
		return ControlResult{Version: 1, State: "needs_action", Code: "policy_snapshot_unavailable", Next: "earlier installation has no retained declaration snapshot; import/review is required for a complete field diff"}, nil
	}
	result = plannedChanges(*before, backupDesired(r.Desired))
	if observed.Paths != nil {
		layout, e := backup.ResolveInstallationPaths(r.Facts.Home, r.Facts.ConfigHome, r.Facts.StateHome, r.Facts.DataHome, r.Desired.Backup.Paths)
		if e != nil {
			return result, e
		}
		if !reflect.DeepEqual(layout, *observed.Paths) {
			result.State, result.Code = "needs_action", "path_migration_required"
			result.Next = "restore the installed declaration and target XDG roots; moving retained state requires an explicit path migration"
			return result, nil
		}
	}
	// A disabled/standalone backup omits gateway policy from its own digest.
	// That scoping rule is not a request to remove the notification worker.
	if before.Notification == nil || backupDesired(r.Desired).Notification == nil {
		filtered := result.Changes[:0]
		for _, change := range result.Changes {
			if !strings.HasPrefix(change.Field, "notification_worker") {
				filtered = append(filtered, change)
			}
		}
		result.Changes = filtered
		if len(filtered) == 0 {
			result.State, result.Code = "up_to_date", "ok"
			result.Next = "backup policy matches retained policy; unrelated notification policy is outside this backup's scope"
		}
	}
	endpoints, err := sourcePlanChanges(r.SourceTargets, observed.SourceTargets)
	if err != nil {
		return result, err
	}
	result.Changes = append(result.Changes, endpoints...)
	sort.Slice(result.Changes, func(i, j int) bool { return result.Changes[i].Field < result.Changes[j].Field })
	if len(endpoints) > 0 {
		result.State, result.Code = "needs_action", "source_migration_required"
		result.Next = "apply endpoint changes separately from policy/artifacts: the new address must retain the exact source identity, authorized key and helper; apply inspects those without enrollment and stages renewed qualification; source replacement needs a separate migration; plan has not contacted any source"
	} else if observed.HelperSHA256 != "" && observed.HelperSHA256 != r.HelperSHA256 {
		result.State, result.Code = "needs_action", "artifact_update_required"
		result.Next = "selected helper differs from the installed release; review matching worker/source images and helper artifacts before applying"
	} else if hasSourceRetirementChanges(result.Changes) {
		result.Next = "source removal requires retire_sources with a reason for each removed alias; retain old source administrator endpoints until apply completes; apply preserves archives and incident history, revokes exact source access and stages the remaining schedules; shared reporting retains its unchanged gateway scope and producer"
	} else if onlyBackupMonitoringChanges(result.Changes) {
		result.Next = "apply validates the private HTTPS monitoring files and stages the updated policy without sending heartbeats; keep previous files for rollback, qualify the checks and explicitly reactivate schedules"
	} else if onlyBackupTelegramReferences(result.Changes) {
		result.Next = "apply supports new private token_file and/or chat_id_file references for the same Telegram bot and destination in standalone reporting; keep all old and new files through update/rollback, then qualify and explicitly reactivate schedules; a different bot or destination requires migration"
	} else if len(result.Changes) == 1 && result.Changes[0].Field == "backup_worker.sources" && before.Backup != nil && before.Backup.Notifications.Signal == nil {
		seen := map[string]bool{}
		for _, alias := range r.Desired.Backup.Sources {
			seen[alias] = true
		}
		adding := len(seen) > len(before.Backup.Sources)
		for _, alias := range before.Backup.Sources {
			adding = adding && seen[alias]
		}
		oldScope, oldErr := before.NotificationFingerprint()
		newScope, newErr := r.Desired.NotificationFingerprint()
		adding = adding && oldErr == nil && newErr == nil && oldScope == newScope
		if adding {
			result.Next = "apply enrolls added sources with retained keys and unchanged shared producer ownership; keep existing source endpoints, trust, gateway policy and artifacts unchanged, then qualify and reactivate the schedule"
		}
	}
	result = sharedBackupConfigurationPlan(result, *before, r.Desired, observed, r.Server)
	return result, nil
}

func onlyBackupTelegramReferences(changes []Change) bool {
	if len(changes) == 0 {
		return false
	}
	for _, c := range changes {
		if c.Field != "backup_worker.notifications.telegram.token_file" && c.Field != "backup_worker.notifications.telegram.chat_id_file" {
			return false
		}
	}
	return true
}

func onlyBackupMonitoringChanges(changes []Change) bool {
	if len(changes) == 0 {
		return false
	}
	for _, c := range changes {
		if !strings.HasPrefix(c.Field, "backup_worker.monitoring.") {
			return false
		}
	}
	return true
}

// A live plan observes the target without invoking any apply lifecycle. The SSH
// transport may stage its temporary inspector; it never writes retained policy,
// credentials or journals, and never enables services or sends diagnostics.
func (c CLI) inspectPlan(ctx context.Context, v Invocation, inv config.ServerInventory, facts TargetFacts, p Plan) (Plan, error) {
	inspector, ok := c.Inspector.(StatusInspector)
	if !ok {
		p.Unresolved = append(p.Unresolved, "read-only installed-policy inspection unavailable")
		return p, nil
	}
	status, err := inspector.Status(ctx, inv.Servers[v.Server], p.Worker, *p.Paths)
	if err != nil {
		return p, err
	}
	metadata := ApplyStatus(p, status)
	p.Observed = metadata.Observed
	if metadata.Code == "not_installed" {
		p.Unresolved = []string{"provider files and qualification remain unchecked until explicit setup"}
		return p, nil
	}
	if metadata.Code != "admission_unverified" {
		return metadata, nil
	}
	if p.Artifacts == nil {
		p.Unresolved = append(p.Unresolved, "matching helper required to inspect retained policy")
		return p, nil
	}
	if c.Controller == nil {
		c.Controller = SSHController{}
	}
	desired := *inv.Servers[v.Server].Desired
	if p.Worker == "backup-worker" {
		desired = backupDesired(desired)
	}
	r := ControlRequest{Version: 1, Worker: p.Worker, Server: v.Server, Action: "plan", Facts: facts, Desired: desired, DesiredScope: p.DesiredScope, DesiredHash: p.DesiredHash}
	if p.Worker == "backup-worker" {
		var err error
		r.SourceTargets, err = declaredSourceTargets(inv, desired)
		if err != nil {
			return p, err
		}
	}
	result, err := c.Controller.Apply(ctx, inv.Servers[v.Server], p.Artifacts.Helper, r)
	if err != nil {
		return p, err
	}
	if result.Version != 1 || result.State == "" || result.Code == "" {
		return p, fmt.Errorf("invalid policy inspection response")
	}
	p.State, p.Code, p.Next, p.Changes = result.State, result.Code, result.Next, result.Changes
	p.Unresolved = []string{"plan does not grant delivery admission or qualify runtime/provider/hardware observations"}
	if p.Worker == "backup-worker" {
		p.Unresolved = append(p.Unresolved, "source host-key trust and per-source helper artifacts are checked during apply")
	}
	if result.Code == "ok" || result.State == "needs_action" {
		p.Actions = nil
	}
	if len(p.Missing) != 0 {
		p.State, p.Code = "needs_input", "missing_input"
	}
	return p, nil
}

func hasSourceRetirementChanges(changes []Change) bool {
	for _, change := range changes {
		if strings.HasPrefix(change.Field, "backup_worker.retire_sources") {
			return true
		}
	}
	return false
}
