package workerctl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func sharedPlanFixture(t *testing.T) config.DesiredWorkers {
	t.Helper()
	r, _ := bootstrapControlFixture(t)
	d := r.Desired
	p := d.Notification.Providers["telegram"]
	p.TokenInput, p.ChatIDInput = "", ""
	p.TokenFile, p.ChatIDFile = "/private/old-token", "/private/old-chat"
	d.Notification.Providers["telegram"] = p
	d.Notification.Routes["audit"] = config.NotificationRoute{Primary: "telegram"}
	d.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {Routes: []string{"alerts", "audit"}, MaxMessages: 1000, MaxSilenceSeconds: 120, MaxEvidenceSeconds: 93600, PolicyHealth: true}}
	return d
}
func cloneSharedPlanPolicy(t *testing.T, d config.DesiredWorkers) config.DesiredWorkers {
	t.Helper()
	raw, err := json.Marshal(d)
	testutil.Must(t, err)
	var out config.DesiredWorkers
	testutil.Must(t, json.Unmarshal(raw, &out))
	return out
}
func sharedPlanState(d *config.DesiredWorkers, state string) (OperationJournal, install.InstallationObservation) {
	j := OperationJournal{Pins: OperationPins{Action: "apply"}, Children: []ChildReference{{Name: "notification-enrollment"}}}
	m := install.ManagedManifest{Mode: "verification"}
	if state != "V0" && state != "VU" {
		m.Mode, m.DeliveryOwnerHash = "delivery", protocol.Hash("owner")
		j.Children[0].Name = "notification-delivery"
	}
	if state == "VU" || state == "AU" {
		d.Notification.Commands.Workers = map[string]string{"backup": "backup-worker"}
	}
	if state == "AD" || state == "AE" || state == "RD" || state == "RE" {
		m.CommandHistoryHash = protocol.Hash("command-head")
		d.Notification.Commands.Workers = map[string]string{"backup": "backup-worker"}
		j.Children = []ChildReference{{Name: "backup-command-receiver"}, {Name: "notification-commands"}}
		if state == "AE" || state == "RE" {
			d.Notification.Commands.Enabled = true
			m.Commands = &install.ManagedCommandAdmission{Mode: "read"}
		}
	}
	if state == "Q" {
		m.Mode = "verification"
	}
	if state == "R0" || state == "RD" || state == "RE" {
		m.RecoveryCommitHash = protocol.Hash("recovery")
	}
	return j, install.InstallationObservation{Manifest: m, Phase: "complete"}
}

func TestSharedConfigurationMigrationMatrix(t *testing.T) {
	const ok = "policy_changed"
	const admission = "command_admission_required"
	const disable = "disable_commands_required"
	const producer = "policy_changed"
	const routing = "command_routing_migration_required"
	const recovered = "recovered_configuration_migration_required"
	const qualification = "delivery_qualification_pending"
	states := []string{"V0", "VU", "A0", "AU", "AD", "AE", "Q", "R0", "RD", "RE"}
	cases := []struct {
		name    string
		prepare func(*config.DesiredWorkers)
		change  func(*config.DesiredWorkers)
		want    []string
	}{
		{"limits", nil, func(d *config.DesiredWorkers) { d.Notification.Limits.MaxMessages++ }, []string{ok, ok, ok, admission, ok, disable, qualification, recovered, recovered, recovered}},
		{"timing", nil, func(d *config.DesiredWorkers) {
			p := d.Notification.Producers["backup-worker"]
			p.MaxSilenceSeconds++
			d.Notification.Producers["backup-worker"] = p
		}, []string{ok, ok, ok, admission, ok, disable, qualification, recovered, recovered, recovered}},
		{"quota", nil, func(d *config.DesiredWorkers) {
			p := d.Notification.Producers["backup-worker"]
			p.MaxMessages--
			d.Notification.Producers["backup-worker"] = p
		}, []string{ok, ok, ok, admission, producer, disable, qualification, recovered, recovered, recovered}},
		{"explicit-declaration", func(d *config.DesiredWorkers) { d.Notification.Producers = nil }, func(d *config.DesiredWorkers) {
			d.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {Routes: []string{"alerts"}, MaxMessages: 1000, MaxSilenceSeconds: 120, MaxEvidenceSeconds: 93600}}
		}, []string{ok, ok, ok, admission, producer, disable, qualification, recovered, recovered, recovered}},
		{"routes", nil, func(d *config.DesiredWorkers) { delete(d.Notification.Routes, "audit") }, []string{ok, ok, ok, admission, ok, disable, qualification, recovered, recovered, recovered}},
		{"telegram-files", nil, func(d *config.DesiredWorkers) {
			p := d.Notification.Providers["telegram"]
			p.TokenFile = "/private/new-token"
			d.Notification.Providers["telegram"] = p
		}, []string{ok, ok, ok, admission, ok, disable, qualification, recovered, recovered, recovered}},
		{"telegram-metadata", nil, func(d *config.DesiredWorkers) {
			p := d.Notification.Providers["telegram"]
			p.Account = "999-private-account"
			d.Notification.Providers["telegram"] = p
		}, []string{"provider_identity_migration_required", "provider_identity_migration_required", "provider_identity_migration_required", "provider_identity_migration_required", "provider_identity_migration_required", "provider_identity_migration_required", qualification, recovered, recovered, recovered}},
		{"signal-destination", func(d *config.DesiredWorkers) {
			d.Notification.Providers["signal"] = config.ProviderDeclaration{AccountFile: "/private/signal-account", RecipientFile: "/private/signal-recipient"}
		}, func(d *config.DesiredWorkers) {
			p := d.Notification.Providers["signal"]
			p.RecipientFile = "/private/new-signal-recipient"
			d.Notification.Providers["signal"] = p
		}, []string{"signal_provider_migration_required", "signal_provider_migration_required", "signal_provider_migration_required", "signal_provider_migration_required", "signal_provider_migration_required", "signal_provider_migration_required", qualification, recovered, recovered, recovered}},
		{"derived-freshness", func(d *config.DesiredWorkers) { d.Notification.Producers = nil }, func(d *config.DesiredWorkers) { d.Backup.Freshness = "48h" }, []string{ok, ok, ok, admission, ok, disable, qualification, recovered, recovered, recovered}},
		{"report-route", nil, func(d *config.DesiredWorkers) { d.Backup.Notifications.Route = "audit" }, []string{ok, ok, ok, admission, ok, disable, qualification, recovered, recovered, recovered}},
		{"activity", nil, func(d *config.DesiredWorkers) { off := false; d.Backup.Enabled = &off }, []string{ok, ok, ok, ok, ok, disable, qualification, recovered, recovered, recovered}},
		{"dependency", nil, func(d *config.DesiredWorkers) { d.Backup.Notifications.Worker = "other-gateway" }, []string{"shared_dependency_migration_required", "shared_dependency_migration_required", "shared_dependency_migration_required", "shared_dependency_migration_required", "shared_dependency_migration_required", "shared_dependency_migration_required", qualification, recovered, recovered, recovered}},
		{"mixed", nil, func(d *config.DesiredWorkers) {
			d.Notification.Limits.MaxMessages++
			delete(d.Notification.Routes, "audit")
		}, []string{"separate_configuration_changes_required", "separate_configuration_changes_required", "separate_configuration_changes_required", "separate_configuration_changes_required", "separate_configuration_changes_required", "separate_configuration_changes_required", qualification, recovered, recovered, recovered}},
	}
	for _, tc := range cases {
		for i, state := range states {
			t.Run(tc.name+"/"+state, func(t *testing.T) {
				before := sharedPlanFixture(t)
				if tc.prepare != nil {
					tc.prepare(&before)
				}
				j, o := sharedPlanState(&before, state)
				after := cloneSharedPlanPolicy(t, before)
				tc.change(&after)
				originalBefore, originalAfter, originalManifest, originalJournal := protocol.Hash(before), protocol.Hash(after), protocol.Hash(o), protocol.Hash(j)
				result := plannedChanges(notificationDiffPolicy(before), notificationDiffPolicy(after))
				originalChanges := protocol.Hash(result.Changes)
				got := sharedConfigurationPlan(result, before, after, j, o)
				if got.Code != tc.want[i] {
					t.Fatalf("want %s, got %+v", tc.want[i], got)
				}
				if (got.State == "needs_action") != (tc.want[i] != ok) || got.Next == "" {
					t.Fatal("missing required action", got)
				}
				raw, err := json.Marshal(got)
				testutil.Must(t, err)
				if strings.Contains(string(raw), "/private/") || strings.Contains(string(raw), "999-private-account") {
					t.Fatal("plan leaked private policy", string(raw))
				}
				if originalBefore != protocol.Hash(before) || originalAfter != protocol.Hash(after) || originalManifest != protocol.Hash(o) || originalJournal != protocol.Hash(j) || originalChanges != protocol.Hash(result.Changes) {
					t.Fatal("read-only classification mutated its inputs")
				}
			})
		}
	}
}

func TestSharedConfigurationPlanAuthorityAndNoopPrecedence(t *testing.T) {
	for _, state := range []string{"V0", "VU", "A0", "AU", "AD", "AE", "Q", "R0", "RD", "RE"} {
		t.Run(state, func(t *testing.T) {
			d := sharedPlanFixture(t)
			j, o := sharedPlanState(&d, state)
			r := plannedChanges(notificationDiffPolicy(d), notificationDiffPolicy(d))
			if got := sharedConfigurationPlan(r, d, d, j, o); !reflect.DeepEqual(got, r) {
				t.Fatal("no-op acquired migration requirements", got)
			}
			next := cloneSharedPlanPolicy(t, d)
			next.Notification.Limits.MaxMessages++
			r = plannedChanges(notificationDiffPolicy(d), notificationDiffPolicy(next))
			o.Unbound = true
			if got := sharedConfigurationPlan(r, d, next, j, o); !reflect.DeepEqual(got, r) {
				t.Fatal("shared guidance changed unbound workflow")
			}
		})
	}
}

func TestSharedConfigurationPlanRetainsReadOnlyDriftAndPendingChecks(t *testing.T) {
	r, p := bootstrapControlFixture(t)
	r.Action, r.Inputs, r.DesiredScope = "plan", nil, 1
	r.Desired = sharedPlanFixture(t)
	r.DesiredHash, _ = r.Desired.NotificationFingerprint()
	d, err := ensureConfigDirectory(p.Control)
	testutil.Must(t, err)
	defer d.Close()
	testutil.Must(t, retainDeclaration(d, r.Desired, r.DesiredHash, 1))
	lock, err := d.Lock("operation.lock", true, true)
	testutil.Must(t, err)
	lock.Close()
	m := install.ManagedManifest{Mode: "delivery", DeliveryOwnerHash: protocol.Hash("owner"), RecoveryCommitHash: protocol.Hash("recovery")}
	j := OperationJournal{Version: 1, ID: testutil.ID(t, "n"), Complete: true, Pins: OperationPins{Worker: r.Worker, Action: "apply", DesiredScope: 1, DesiredHash: r.DesiredHash, UID: r.Facts.UID, Home: r.Facts.Home, Artifacts: map[string]string{"helper": r.HelperSHA256}, Installations: map[string]string{"notification-worker": p.Installation, "state": p.State, "runtime": p.Runtime}}, Children: []ChildReference{{Name: "notification-delivery", Operation: testutil.ID(t, "n"), Complete: true, Receipt: protocol.Hash(m)}}}
	testutil.Must(t, d.WriteJSON("operation.json", j, false))
	r.Desired = cloneSharedPlanPolicy(t, r.Desired)
	r.Desired.Notification.Limits.MaxMessages++
	r.DesiredHash, _ = r.Desired.NotificationFingerprint()
	inspect := func(context.Context, string) (install.InstallationObservation, error) {
		return install.InstallationObservation{Manifest: m, Phase: "complete"}, nil
	}
	files := planTree(t, r.Facts.Home)
	for range 2 {
		got, err := planNotificationControl(context.Background(), r, p, inspect)
		testutil.Must(t, err)
		if got.Code != "recovered_configuration_migration_required" {
			t.Fatal(got)
		}
	}
	if !reflect.DeepEqual(files, planTree(t, r.Facts.Home)) {
		t.Fatal("planning changed retained files")
	}
	m.Revision = "drift"
	if _, err := planNotificationControl(context.Background(), r, p, inspect); err == nil {
		t.Fatal("drift bypassed canonical inspection")
	}
	m.Revision = ""
	r.HelperSHA256 = protocol.Hash("new-helper")
	got, err := planNotificationControl(context.Background(), r, p, inspect)
	testutil.Must(t, err)
	if got.Code != "artifact_update_required" {
		t.Fatal("matrix hid helper review", got)
	}
	r.HelperSHA256 = j.Pins.Artifacts["helper"]
	j.Complete = false
	j.Children[0].Complete = false
	j.Children[0].Receipt = ""
	testutil.Must(t, d.WriteJSON("operation.json", j, true))
	got, err = planNotificationControl(context.Background(), r, p, func(context.Context, string) (install.InstallationObservation, error) {
		t.Fatal("pending operation reached installation inspection")
		return install.InstallationObservation{}, nil
	})
	testutil.Must(t, err)
	if got.Code != "pending_operation" {
		t.Fatal("matrix hid pending operation", got)
	}
	j.Complete, j.Children[0].Complete, j.Children[0].Receipt = true, true, protocol.Hash(m)
	testutil.Must(t, d.WriteJSON("operation.json", j, true))
	testutil.Must(t, os.Remove(filepath.Join(p.Control, "declarations", j.Pins.DesiredHash+".json")))
	files = planTree(t, r.Facts.Home)
	if _, err := planNotificationControl(context.Background(), r, p, inspect); err == nil {
		t.Fatal("missing snapshot bypassed retained declaration review")
	}
	if !reflect.DeepEqual(files, planTree(t, r.Facts.Home)) {
		t.Fatal("missing snapshot was recreated")
	}
}

func TestSharedBackupPlanRequiresGatewayInspection(t *testing.T) {
	before := sharedPlanFixture(t)
	after := cloneSharedPlanPolicy(t, before)
	after.Notification.Limits.MaxMessages++
	observed := backup.SetupObservation{Stage: "complete", SharedReportingConnected: true}
	r := ControlRequest{Server: "manager", Desired: after, Facts: testFacts()}
	inspect := func(context.Context, string) (backup.SetupObservation, *config.DesiredWorkers, error) {
		return observed, &before, nil
	}
	got, err := planBackupControl(context.Background(), r, inspect)
	testutil.Must(t, err)
	if got.Code != "shared_gateway_review_required" || !strings.Contains(got.Next, "notification-worker plan manager") {
		t.Fatal(got)
	}
	after = cloneSharedPlanPolicy(t, before)
	off := false
	after.Backup.Enabled = &off
	r.Desired = after
	got, err = planBackupControl(context.Background(), r, inspect)
	testutil.Must(t, err)
	if got.Code != "shared_activity_migration_required" {
		t.Fatal(got)
	}
	after = cloneSharedPlanPolicy(t, before)
	after.Backup.Schedule = "weekly"
	r.Desired = after
	got, err = planBackupControl(context.Background(), r, inspect)
	testutil.Must(t, err)
	if got.Code != "policy_changed" {
		t.Fatal("backup-only edit acquired gateway requirement", got)
	}
}

func TestSharedConfigurationConditionalPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name, state, want string
		change            func(*config.DesiredWorkers, *config.DesiredWorkers, *install.InstallationObservation)
	}{
		{"telegram-presence-staged", "V0", "policy_changed", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			signal := config.ProviderDeclaration{AccountFile: "/private/signal", RecipientFile: "/private/recipient"}
			b.Notification.Providers["signal"], a.Notification.Providers["signal"] = signal, signal
			b.Notification.Routes["alerts"], a.Notification.Routes["alerts"] = config.NotificationRoute{Primary: "signal"}, config.NotificationRoute{Primary: "signal"}
			b.Notification.Routes["audit"], a.Notification.Routes["audit"] = config.NotificationRoute{Primary: "signal"}, config.NotificationRoute{Primary: "signal"}
			delete(a.Notification.Providers, "telegram")
			o.Manifest.FreshSignal = &install.FreshSignalReference{}
		}},
		{"telegram-presence-owned-signal", "A0", "policy_changed", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			signal := config.ProviderDeclaration{AccountFile: "/private/signal", RecipientFile: "/private/recipient"}
			b.Notification.Providers["signal"], a.Notification.Providers["signal"] = signal, signal
			b.Notification.Routes["alerts"], a.Notification.Routes["alerts"] = config.NotificationRoute{Primary: "signal"}, config.NotificationRoute{Primary: "signal"}
			b.Notification.Routes["audit"], a.Notification.Routes["audit"] = config.NotificationRoute{Primary: "signal"}, config.NotificationRoute{Primary: "signal"}
			delete(a.Notification.Providers, "telegram")
			o.Manifest.FreshSignal = &install.FreshSignalReference{}
		}},
		{"telegram-presence-unadmitted", "VU", "command_provider_migration_required", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			delete(a.Notification.Providers, "telegram")
		}},
		{"additional-producer", "A0", "producer_enrollment_required", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			a.Notification.Producers["new-producer"] = a.Notification.Producers["backup-worker"]
		}},
		{"legacy-signal", "A0", "signal_adoption_required", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			a.Notification.Limits.MaxMessages++
			o.Manifest.SignalVerification = &install.ManagedSignalBinding{}
		}},
		{"missing-staged-history", "V0", "shared_configuration_review_required", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			a.Notification.Limits.MaxMessages++
		}},
		{"private-inputs", "A0", "shared_configuration_prerequisites_required", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			p := a.Notification.Providers["telegram"]
			p.TokenInput = "/private/operator"
			a.Notification.Providers["telegram"] = p
		}},
		{"unapplied-report-route", "V0", "shared_configuration_prerequisites_required", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			a.Backup.Notifications.Route = "undeclared"
		}},
		{"command-toggle-plus-budget", "AE", "separate_configuration_changes_required", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			a.Notification.Commands.Enabled = false
			a.Notification.Limits.MaxMessages++
		}},
		{"dependency-removal", "A0", "shared_dependency_migration_required", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			a.Backup.Notifications.Worker = ""
		}},
		{"backup-only-history", "AD", "policy_changed", func(b, a *config.DesiredWorkers, o *install.InstallationObservation) {
			a.Notification.Limits.MaxMessages++
			a.Backup.Schedule = "weekly"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := sharedPlanFixture(t)
			j, o := sharedPlanState(&before, tc.state)
			if tc.name == "missing-staged-history" {
				j.Children[0].Name = "notification-bootstrap"
			}
			after := cloneSharedPlanPolicy(t, before)
			tc.change(&before, &after, &o)
			result := plannedChanges(notificationDiffPolicy(before), notificationDiffPolicy(after))
			got := sharedConfigurationPlan(result, before, after, j, o)
			if got.Code != tc.want {
				t.Fatalf("want %s, got %+v", tc.want, got)
			}
		})
	}
}
