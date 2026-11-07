package backup

import (
	"database/sql"
	"fmt"
)

func requireBackupReporting(state *backupState, c WorkerInstallation) error {
	var control ArchiveControl
	err := state.get("control", &control)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if c.Policy.Notifications.Mode == "shared" || control.SharedRequired {
		return fmt.Errorf("legacy reporting is fenced by declared shared notifications")
	}
	if c.Policy.Notifications.Mode == "disabled" || control.ReportingDisabled {
		return fmt.Errorf("reporting is disabled for this installation")
	}
	return nil
}

// These lists select required evidence, not fabricated passing evidence. Version
// 2 and provider-enabled installations retain all existing qualification gates.
func workerQualificationGates(c WorkerInstallation, application bool) []string {
	gates := []string{"source_restrictions", "rootless_runtime", "storage_durability"}
	if c.Policy.Notifications.Mode != "disabled" {
		gates = append(gates, "notification_delivery", "external_monitor")
	}
	if application {
		gates = append(gates, "application_restore")
	}
	return gates
}

func workerQualificationChecks(c WorkerInstallation) []string {
	checks := []string{"storage_crash_recovery", "isolated_application_restore"}
	if c.Policy.Notifications.Mode != "disabled" {
		checks = append(checks, "notification_receipt", "missed_worker_route", "daily_delivery_route")
	}
	return checks
}

func workerPolicyActivationUnits(c WorkerInstallation, shared bool) []string {
	if c.Policy.Notifications.Mode == "disabled" {
		return []string{"deploy-backup.timer", "deploy-backup-health.timer", "deploy-backup-maintenance.timer"}
	}
	return workerActivationUnitsFor(shared)
}
