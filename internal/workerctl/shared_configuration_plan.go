package workerctl

import (
	"reflect"
	"strings"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
)

// This classification is planning advice, never migration/admission authority.
// Canonical inspection, pending-operation and drift checks run before it. The
// existing lifecycle validators still own all writes, capacity and private input
// checks. See docs/plans/shared-configuration.md for the state/change matrix.
func sharedConfigurationFamily(changes []Change) string {
	family := ""
	other := false
	for _, change := range changes {
		next := ""
		switch {
		case change.Field == "notification_worker.enabled" || change.Field == "backup_worker.enabled" || change.Field == "backup_worker.notifications.enabled":
			next = "activity"
		case change.Field == "backup_worker.notifications.worker":
			next = "dependency"
		case change.Field == "backup_worker.notifications.route":
			next = "report-route"
		case change.Field == "backup_worker.freshness":
			next = "freshness"
		case policyFieldGroup(change.Field, "notification_worker.limits"):
			next = "limits"
		case policyFieldGroup(change.Field, "notification_worker.producers"):
			next = "producer"
		case policyFieldGroup(change.Field, "notification_worker.routes"):
			next = "routes"
		case policyFieldGroup(change.Field, "notification_worker.providers"):
			next = "providers"
		default:
			other = true
		}
		if next == "" {
			continue
		}
		if family != "" && family != next {
			return "mixed"
		}
		family = next
	}
	if family != "" && other {
		return "mixed"
	}
	return family
}
func policyFieldGroup(field, group string) bool {
	return field == group || strings.HasPrefix(field, group+".")
}
func sharedPlanAction(r ControlResult, code, next string) ControlResult {
	r.State, r.Code, r.Next = "needs_action", code, next
	return r
}
func sharedActivityChanged(before, after config.DesiredWorkers) bool {
	if before.Notification != nil && after.Notification != nil && config.WorkerEnabled(before.Notification.Enabled) != config.WorkerEnabled(after.Notification.Enabled) {
		return true
	}
	return before.Backup != nil && after.Backup != nil && (config.WorkerEnabled(before.Backup.Enabled) != config.WorkerEnabled(after.Backup.Enabled) || config.WorkerEnabled(before.Backup.Notifications.Enabled) != config.WorkerEnabled(after.Backup.Notifications.Enabled))
}
func declaredCommands(n *config.NotificationDeclaration) bool {
	return n != nil && (n.Commands.Enabled || len(n.Commands.Workers) != 0 || len(n.Commands.Conversations) != 0 || len(n.Commands.Permissions) != 0)
}
func sharedConfigurationPlan(result ControlResult, before, after config.DesiredWorkers, j OperationJournal, observed install.InstallationObservation) ControlResult {
	if len(result.Changes) == 0 || observed.Unbound || before.Notification == nil || after.Notification == nil || before.Backup == nil || before.Backup.Notifications.Worker == "" {
		return result
	}
	changes := result.Changes
	if after.Backup != nil && before.Backup.Freshness == after.Backup.Freshness {
		changes = nil
		for _, c := range result.Changes {
			// Explicit producer policy replaces the derived freshness scope.
			if c.Field != "backup_worker.freshness" {
				changes = append(changes, c)
			}
		}
	}
	family := sharedConfigurationFamily(changes)
	// Disabling a dependency removes its derived scope fields too. Those derived
	// diffs are activity consequences, not separate route/freshness requests.
	if after.Backup == nil || before.Backup.Notifications.Worker != after.Backup.Notifications.Worker {
		family = "dependency"
	}
	if sharedActivityChanged(before, after) {
		family = "activity"
	}
	if family == "" {
		return result
	}
	m := observed.Manifest
	if m.RecoveryCommitHash != "" {
		return sharedPlanAction(result, "recovered_configuration_migration_required", "shared configuration changes on recovered installations require a recovery-aware migration; retain the applied configuration, active recovered identities and recovery proofs; complete recovery review before attempting an ordinary policy update")
	}
	if m.SignalVerification != nil || m.SignalProfiles != nil {
		return sharedPlanAction(result, "signal_adoption_required", "retain the installed Signal account and inbox; this shared configuration change requires the legacy Signal adoption/qualification workflow before ordinary updates")
	}
	if m.Mode == "verification" && m.DeliveryOwnerHash != "" && !sharedActivityJournal(j) {
		return sharedPlanAction(result, "delivery_qualification_pending", "finish the retained delivery pause with its exact configuration: qualify and apply before planning another shared configuration change; command intake remains a separate admission")
	}
	switch family {
	case "activity":
		if config.ValidateSharedActivityDeclaration(before, after) != nil {
			return sharedPlanAction(result, "separate_configuration_changes_required", "apply shared activity separately from other configuration")
		}
		if (before.Notification.Commands.Enabled || after.Notification.Commands.Enabled) || m.Commands != nil && m.Commands.Mode != "drain" {
			return sharedPlanAction(result, "disable_commands_required", "set commands.enabled: false and apply separately before shared activity")
		}
		result.Next = "apply either worker to fence capture and reporting before gateway pause; restore activity and record fresh gateway qualification; command intake and backup capture activation remain separate"
		return result
	case "dependency":
		return sharedPlanAction(result, "shared_dependency_migration_required", "changing the shared notification worker requires explicit producer handoff/enrollment and old-owner disposition; retain the existing gateway and reporting owner until that workflow is available")
	case "mixed":
		return sharedPlanAction(result, "separate_configuration_changes_required", "apply one supported shared configuration family at a time and apply command, LED, artifact and backup-only edits separately; coordinated mixed edits require a supported dependency-ordered workflow; keep the exact configuration for any retained operation")
	}
	if family == "producer" {
		for _, n := range []*config.NotificationDeclaration{before.Notification, after.Notification} {
			for name := range n.Producers {
				if name != "backup-worker" {
					return sharedPlanAction(result, "producer_enrollment_required", "additional producer identities require their own enrollment/retirement workflow; retain the existing local backup producer and outstanding work")
				}
			}
		}
	}
	if family == "providers" {
		oldSignal, had := before.Notification.Providers["signal"]
		nextSignal, has := after.Notification.Providers["signal"]
		if had != has || !reflect.DeepEqual(oldSignal, nextSignal) && (m.FreshSignal == nil || declaredCommands(before.Notification) || declaredCommands(after.Notification) || config.ValidateSignalReferenceChange(oldSignal, nextSignal) != nil) {
			return sharedPlanAction(result, "signal_provider_migration_required", "Signal account, recipient, service and artifact changes require their provider migration and applicable adoption/relinking workflow; retain the current account, inbox and pending destinations")
		}
		old, had := before.Notification.Providers["telegram"]
		next, has := after.Notification.Providers["telegram"]
		if had != has && (declaredCommands(before.Notification) || declaredCommands(after.Notification)) {
			return sharedPlanAction(result, "command_provider_migration_required", "adding/removing the Telegram command provider requires its separate profile and receiver admission workflow; retain the existing bot, conversations and pending replies")
		}
		if had != has && m.FreshSignal == nil {
			return sharedPlanAction(result, "shared_configuration_prerequisites_required", "adding/removing an unused Telegram profile requires an unchanged owned Signal service; retain the current providers until ownership and route prerequisites are satisfied")
		}
		if had && has && (old.Account != next.Account || old.BotName != next.BotName || old.Conversation != next.Conversation) {
			return sharedPlanAction(result, "provider_identity_migration_required", "explicit Telegram account/name or interactive destination changes require provider identity review and command admission where applicable; retain original receiver authority and reply destinations")
		}
	}
	if m.Mode == "verification" {
		if !enrolledNotificationJournal(j) {
			return sharedPlanAction(result, "shared_configuration_review_required", "review/import the installation's completed shared coordinator history before changing policy; a matching runtime alone does not establish migration eligibility")
		}
		if family == "report-route" && config.ValidateSharedReportRouteDeclaration(before, after) == nil {
			result.Next = "select an already declared and applied backup report route; apply either worker before delivery activation; explicit producer permission must already allow it, default permission changes require settled work, then qualify the revised runtime"
		}
		kind := map[string]string{"limits": "", "producer": "producer", "routes": "routes", "providers": "providers", "freshness": "derived-freshness", "report-route": "report-route"}[family]
		scope, err := before.NotificationFingerprint()
		if err != nil || validateSharedPolicy(before, after, scope, kind) != nil {
			return sharedPlanAction(result, "shared_configuration_prerequisites_required", "prepare the selected shared update separately: retain other policy, declare/apply any needed route and producer permission first, and select private target-side Telegram files; apply reviews queue, ownership and qualification prerequisites")
		}
		return result // Existing staged guidance describes the eligible workflow.
	}
	if m.Mode != "delivery" {
		return sharedPlanAction(result, "shared_configuration_review_required", "inspect the retained installation activity and ownership before this shared configuration change")
	}
	commands := m.CommandHistoryHash != "" || m.Commands != nil || admittedCommandHistory(j)
	if !commands && (declaredCommands(before.Notification) || declaredCommands(after.Notification)) {
		return sharedPlanAction(result, "command_admission_required", "this delivering gateway has a command declaration without retained receiver admission; complete the original admission separately before command-preserving production updates; staged command editing does not authorize this migration")
	}
	if !commands {

		if !requalificationCandidate(j) {
			return sharedPlanAction(result, "shared_configuration_review_required", "review the completed delivery coordinator history before changing shared policy; a matching runtime alone does not establish migration eligibility")
		}
		kind := map[string]string{"limits": "limits", "producer": "producer", "routes": "routes", "freshness": "derived-freshness", "report-route": "report-route"}[family]
		valid := false
		if family == "providers" {
			valid = validateDeliveryPolicyDeclaration(before, after, "") == nil || observed.Manifest.FreshSignal != nil && validateDeliveryPolicyDeclaration(before, after, "provider-enrollment") == nil
		} else {
			valid = validateDeliveryPolicyDeclaration(before, after, kind) == nil
		}
		if !valid {
			return sharedPlanAction(result, "shared_configuration_prerequisites_required", "prepare one supported shared update with other settings unchanged; select private target-side Telegram references, retain owned provider identities and the active backup route; apply must review current capacity, pending work and qualification")
		}
		return result // Existing production review chooses the exact operation.
	}
	if len(before.Notification.Providers) != 1 || len(after.Notification.Providers) != 1 {
		return sharedPlanAction(result, "command_provider_migration_required", "this command-preserving production update requires the Telegram command profile migration; retaining an owned Signal service alone does not admit command updates")
	}
	if _, ok := before.Notification.Providers["telegram"]; !ok {
		return sharedPlanAction(result, "command_provider_migration_required", "Signal command configuration requires its separate intake/admission workflow; retain the existing receiver and account")
	}
	b, a := before, after
	bn, an := *before.Notification, *after.Notification
	b.Notification, a.Notification = &bn, &an
	bn.Commands.Enabled, an.Commands.Enabled = false, false
	// Completed backup-only edits can share an unchanged notification scope.
	// Apply must independently verify those backup receipts before effects.
	if rebased, err := deliveryBackupBefore(b, a, "commands"); err == nil {
		b = rebased
	}
	supported := family == "providers" && config.ValidateCommandDeliveryProviderDeclaration(b, a) == nil || family == "limits" && config.ValidateDeliveryLimitsDeclaration(b, a) == nil || family == "producer" && config.ValidateCommandDeliveryProducerDeclaration(b, a) == nil || family == "routes" && config.ValidateCommandDeliveryRoutesDeclaration(b, a) == nil || family == "report-route" && config.ValidateCommandDeliveryReportRouteDeclaration(b, a) == nil || family == "freshness" && config.ValidateCommandDerivedFreshnessDeclaration(b, a) == nil
	if supported {
		if before.Notification.Commands.Enabled || after.Notification.Commands.Enabled || m.Commands != nil && m.Commands.Mode != "drain" {
			return sharedPlanAction(result, "disable_commands_required", "set commands.enabled: false and apply it separately; then apply only one supported limits, producer, routing, provider reference/destination or derived freshness change, qualify and apply; explicitly re-enable intake afterward")
		}
		result.Next = "apply only the supported limits, producer, routes, report selection, same-bot alert references/destination or derived freshness policy; retain the receiver, permission restrictions, observations, queued work and reply destinations; effective policy changes pause for qualification and apply, equivalent declarations retain delivery; explicitly re-enable commands afterward"
		return result
	}
	label, code := "routing/freshness", "command_routing_migration_required"
	switch family {
	case "limits", "producer":
		label, code = "producer-policy/declaration", "command_producer_policy_migration_required"
	case "providers":
		label, code = "provider/destination", "command_provider_migration_required"
	}
	return sharedPlanAction(result, code, "this shared "+label+" migration is not implemented for a gateway with retained commands; disabling intake alone does not authorize it; retain the applied configuration, receiver restrictions and pending work")
}

// Backup inspection does not supply gateway command/recovery authority. Direct
// shared changes to the gateway's read-only planner instead of guessing its state.
func sharedBackupConfigurationPlan(result ControlResult, before, after config.DesiredWorkers, observed backup.SetupObservation, server string) ControlResult {
	if result.Code != "policy_changed" || !observed.SharedReportingConnected || before.Backup == nil || before.Backup.Notifications.Worker == "" {
		return result
	}
	if sharedActivityChanged(before, after) {
		return sharedPlanAction(result, "shared_activity_migration_required", "shared backup/reporting activity requires coordinated producer fencing; retain the installed shared sender and legacy-sender fence")
	}
	old, e1 := before.NotificationFingerprint()
	next, e2 := after.NotificationFingerprint()
	if e1 == nil && e2 == nil && old != next {
		return sharedPlanAction(result, "shared_gateway_review_required", "run deploy notification-worker plan "+server+" to identify the shared migration and command/recovery prerequisites; this backup inspection does not establish gateway migration authority")
	}
	return result
}
