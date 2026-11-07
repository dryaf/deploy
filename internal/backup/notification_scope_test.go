//go:build linux

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func verificationScopeFixture(t *testing.T, commands ...bool) (string, VerificationNotificationScopeChange, WorkerSetupJournal) {
	t.Helper()
	home, r, _ := sourceExpansionFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	d, err := root.Sub(".local/state/deploy/backup-worker")
	testutil.Must(t, err)
	defer d.Close()
	var j WorkerSetupJournal
	testutil.Must(t, d.ReadJSON("setup.json", &j))
	c, err := loadInstalledWorker(home)
	testutil.Must(t, err)
	c.Policy.Notifications = config.WorkerNotifications{Mode: "shared"}
	c.Policy, err = config.NormalizeWorker(c.Policy)
	testutil.Must(t, err)
	c.ConfigHash = ""
	c.ConfigHash = notify.Hash(c)
	r.Policy = c.Policy
	r.Declaration.Desired.Backup.Notifications = config.DesiredBackupNotifications{Worker: "manager", Route: "alerts", WorkerNotifications: c.Policy.Notifications}
	r.Declaration.Desired.Notification = &config.NotificationDeclaration{StatusLEDs: config.LEDDeclaration{Enabled: true, Socket: "/private/led.sock", Daemon: config.LEDDaemon{Mode: "existing"}, Bindings: map[string]config.LEDBindingDeclaration{"backup": {Producer: "backup-worker", LED: "green", Subjects: []string{"*"}}}}}
	if len(commands) == 1 && commands[0] {
		r.Declaration.Desired.Notification.Providers = config.ProviderDeclarations{"telegram": {TokenFile: "~/.config/token", ChatIDFile: "~/.config/chat"}}
	}
	r.Declaration.DesiredHash, err = r.Declaration.Desired.Fingerprint()
	testutil.Must(t, err)
	r.Declaration.NotificationDesiredHash, err = r.Declaration.Desired.NotificationFingerprint()
	testutil.Must(t, err)
	r.Declaration.NotificationRoot, r.Declaration.NotificationRoute = filepath.Join(home, "gateway"), "alerts"
	j.Declaration, j.RequestHash = r.Declaration, workerSetupRequestHash(r)
	j.Producer = &ProducerReservation{1, notify.Producer{ID: j.WorkerID, Epoch: testutil.ID(t, "n")}, testutil.ID(t, "g")}
	testutil.Must(t, root.WriteJSON(workerRuntimeConfig, c, true))
	raw, err := safefs.ReadFile(root, workerRuntimeConfig, 8<<20)
	testutil.Must(t, err)
	j.OwnedFiles[workerRuntimeConfig] = sharedSetupDigest(raw)
	testutil.Must(t, d.WriteJSON("setup.json", j, true))
	testutil.Must(t, d.WriteJSON("producer-reservation.json", producerReservationIntent{1, j.ID, j.RequestHash, j.WorkerID, j.ArchiveID, *j.Producer}, false))
	state, err := openInstalledBackupState(home, false)
	testutil.Must(t, err)
	a, err := openBackupArchive(c.Policy.Storage.Path)
	testutil.Must(t, err)
	testutil.Must(t, a.updateControl(state, func(control *ArchiveControl) error {
		control.ConfigHash, control.SharedRequired, control.ReportingDisabled = c.ConfigHash, true, false
		return nil
	}))
	a.Close()
	state.Close()
	raw, err = json.Marshal(*r.Declaration.Desired)
	testutil.Must(t, err)
	var desired config.DesiredWorkers
	testutil.Must(t, json.Unmarshal(raw, &desired))
	entry := desired.Notification.StatusLEDs.Bindings["backup"]
	entry.Subjects = []string{"source/database"}
	desired.Notification.StatusLEDs.Bindings["backup"] = entry
	return home, VerificationNotificationScopeChange{Operation: testutil.ID(t, "n"), BeforeScope: r.Declaration.NotificationDesiredHash, BeforeGateway: notify.Hash("retained gateway"), Desired: desired}, j
}

func TestVerificationNotificationScopeRetainsReservationAcrossLostReply(t *testing.T) {
	home, request, before := verificationScopeFixture(t)
	stop := errors.New("gateway committed but reply lost")
	calls := 0
	callback := func(p ProducerReservation) (string, error) {
		calls++
		if p != *before.Producer {
			t.Fatal("producer replaced")
		}
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var pending WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &pending))
		if pending.NotificationScope == nil || pending.RequestHash != before.RequestHash {
			t.Fatal("gateway changed without retained backup predecessor")
		}
		if lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false); err == nil {
			lease.Close()
			t.Fatal("scope transition lost setup lease")
		}
		if calls == 1 {
			return "", stop
		}
		return notify.Hash("completed gateway"), nil
	}
	_, err := ReconcileVerificationNotificationScope(context.Background(), home, request, callback)
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	observed, err := InspectSetup(context.Background(), home)
	if err != nil || observed.Stage != "notification_policy_pending" {
		t.Fatal(observed, err)
	}
	if _, _, err = InspectReservedProducer(home, request.BeforeScope, before.Declaration.NotificationRoot, before.Declaration.NotificationRoute); err == nil {
		t.Fatal("pending scope admitted producer")
	}
	got, err := ReconcileVerificationNotificationScope(context.Background(), home, request, callback)
	testutil.Must(t, err)
	if got != notify.Hash("completed gateway") {
		t.Fatal(got)
	}
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var after WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
	if after.NotificationScope != nil || after.NotificationScopeHistory == nil || after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.Producer, before.Producer) || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || !reflect.DeepEqual(after.OwnedFiles, before.OwnedFiles) {
		t.Fatal("scope update changed runtime ownership")
	}
	tree := policyReviewTree(t, home)
	again, err := ReconcileVerificationNotificationScope(context.Background(), home, request, callback)
	if err != nil || again != got || calls != 2 || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("completed retry changed state or replayed gateway", err, calls)
	}
	p, present, err := InspectReservedProducer(home, after.Declaration.NotificationDesiredHash, after.Declaration.NotificationRoot, after.Declaration.NotificationRoute)
	if err != nil || !present || p != *before.Producer {
		t.Fatal("new scope lost reservation", err)
	}
}

func TestVerificationNotificationScopeRejectsUnrelatedChanges(t *testing.T) {
	for _, damage := range []string{"backup", "provider", "socket", "enablement", "old-scope", "active", "cutover", "key", "runtime"} {
		t.Run(damage, func(t *testing.T) {
			home, r, j := verificationScopeFixture(t)
			switch damage {
			case "backup":
				r.Desired.Backup.Schedule = "weekly"
			case "provider":
				r.Desired.Notification.Providers = config.ProviderDeclarations{"telegram": {TokenFile: "/changed"}}
			case "socket":
				r.Desired.Notification.StatusLEDs.Socket = "/other.sock"
			case "enablement":
				r.Desired.Notification.StatusLEDs.Enabled = false
			case "old-scope":
				r.BeforeScope = notify.Hash("wrong scope")
			case "active", "cutover":
				state, err := openInstalledBackupState(home, false)
				testutil.Must(t, err)
				if damage == "active" {
					testutil.Must(t, state.put("installation", "active"))
				} else {
					testutil.Must(t, state.put("notification_owner", map[string]string{"Mode": "shared"}))
				}
				state.Close()
			case "key":
				for alias := range j.SourceKeys {
					testutil.Must(t, os.Remove(filepath.Join(home, workerTrustDirectory, alias+".key")))
					break
				}
			case "runtime":
				testutil.Must(t, os.WriteFile(filepath.Join(home, workerRuntimeConfig), []byte("{}"), 0600))
			}
			before := policyReviewTree(t, home)
			called := false
			_, err := ReconcileVerificationNotificationScope(context.Background(), home, r, func(ProducerReservation) (string, error) { called = true; return notify.Hash("unexpected"), nil })
			if err == nil || called || !reflect.DeepEqual(before, policyReviewTree(t, home)) {
				t.Fatal("invalid scope change had effects", err, called)
			}
		})
	}
}

func TestVerificationNotificationScopeSuccessiveRevisionsAndHistory(t *testing.T) {
	home, request, before := verificationScopeFixture(t)
	ctx := context.Background()
	for revision := range 3 {
		receipt := notify.Hash(struct{ Revision int }{revision})
		got, err := ReconcileVerificationNotificationScope(ctx, home, request, func(p ProducerReservation) (string, error) {
			if p != *before.Producer {
				t.Fatal("producer changed")
			}
			return receipt, nil
		})
		testutil.Must(t, err)
		scope, err := request.Desired.NotificationFingerprint()
		testutil.Must(t, err)
		observed, complete, err := InspectVerificationNotificationScope(home, request.Operation, request.BeforeGateway, scope)
		if err != nil || !complete || observed != got {
			t.Fatal("completion inspection", complete, err)
		}
		if revision < 2 {
			request.BeforeScope, request.BeforeGateway, request.Operation = scope, receipt, testutil.ID(t, "n")
			if revision == 0 {
				entry := request.Desired.Notification.StatusLEDs.Bindings["backup"]
				entry.Subjects = []string{"source/other"}
				request.Desired.Notification.StatusLEDs.Bindings["backup"] = entry
			} else {
				request.Desired.Notification.StatusLEDs.Bindings = nil
			}
		} else {
			path := filepath.Join(home, ".local/state/deploy/backup-worker/notification-scope-"+request.Operation+".json")
			testutil.Must(t, os.Remove(path))
			if _, _, err = InspectVerificationNotificationScope(home, request.Operation, request.BeforeGateway, scope); err == nil {
				t.Fatal("lost history admitted mapping")
			}
			if _, _, err = InspectReservedProducer(home, scope, before.Declaration.NotificationRoot, before.Declaration.NotificationRoute); err == nil {
				t.Fatal("lost history admitted producer")
			}
		}
	}
}

func TestVerificationNotificationScopePendingRetargetRefused(t *testing.T) {
	home, request, _ := verificationScopeFixture(t)
	stop := errors.New("paused gateway")
	_, err := ReconcileVerificationNotificationScope(context.Background(), home, request, func(ProducerReservation) (string, error) { return "", stop })
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	for _, change := range []string{"operation", "before", "desired"} {
		t.Run(change, func(t *testing.T) {
			copy := request
			switch change {
			case "operation":
				copy.Operation = testutil.ID(t, "n")
			case "before":
				copy.BeforeGateway = notify.Hash("other")
			case "desired":
				copy.Desired.Notification = &config.NotificationDeclaration{}
			}
			tree := policyReviewTree(t, home)
			called := false
			_, err := ReconcileVerificationNotificationScope(context.Background(), home, copy, func(ProducerReservation) (string, error) { called = true; return notify.Hash("wrong"), nil })
			if err == nil || called || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
				t.Fatal("pending scope retargeted", err, called)
			}
		})
	}
}

func TestVerificationNotificationScopeProcessDeath(t *testing.T) {
	if home := os.Getenv("DEPLOY_SCOPE_CRASH_FIXTURE"); home != "" {
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var request VerificationNotificationScopeChange
		testutil.Must(t, root.ReadJSON("scope-request.json", &request))
		_, err = ReconcileVerificationNotificationScope(context.Background(), home, request, func(ProducerReservation) (string, error) {
			if os.Getenv("DEPLOY_SCOPE_CRASH_BOUNDARY") == "receipt" {
				var j WorkerSetupJournal
				testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
				receipt := setupNotificationScopeReceipt{1, *j.NotificationScope, notify.Hash("committed gateway")}
				testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/notification-scope-"+request.Operation+".json", receipt, false))
			}
			testutil.Must(t, syscall.Kill(os.Getpid(), syscall.SIGKILL))
			return "", fmt.Errorf("kill returned")
		})
		t.Fatal("crash boundary missed", err)
	}
	for _, boundary := range []string{"intent", "receipt"} {
		t.Run(boundary, func(t *testing.T) {
			home, request, before := verificationScopeFixture(t)
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			testutil.Must(t, root.WriteJSON("scope-request.json", request, false))
			cmd := exec.Command(os.Args[0], "-test.run=^TestVerificationNotificationScopeProcessDeath$")
			cmd.Env = append(os.Environ(), "DEPLOY_SCOPE_CRASH_FIXTURE="+home, "DEPLOY_SCOPE_CRASH_BOUNDARY="+boundary)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("crash boundary missed: %v %s", err, out)
			}
			var pending WorkerSetupJournal
			testutil.Must(t, root.ReadJSON(backupSetupFile, &pending))
			if pending.NotificationScope == nil {
				t.Fatal("lost intent")
			}
			testutil.Must(t, checkSetupNotificationScope(pending))
			_, err = ReconcileVerificationNotificationScope(context.Background(), home, request, func(p ProducerReservation) (string, error) {
				if p != *before.Producer {
					t.Fatal("lost reservation")
				}
				return notify.Hash("committed gateway"), nil
			})
			testutil.Must(t, err)
			scope, err := request.Desired.NotificationFingerprint()
			testutil.Must(t, err)
			_, done, err := InspectVerificationNotificationScope(home, request.Operation, request.BeforeGateway, scope)
			if err != nil || !done {
				t.Fatal("crash did not resume", err)
			}
		})
	}
}

func TestVerificationNotificationScopeLEDOutputCycles(t *testing.T) {
	home, r, j := verificationScopeFixture(t)
	r.Desired.Notification.StatusLEDs.Bindings = j.Declaration.Desired.Notification.StatusLEDs.Bindings
	for _, enabled := range []bool{false, true, false, true} {
		r.Desired.Notification.StatusLEDs.Enabled = enabled
		receipt := notify.Hash(r.Operation)
		got, err := ReconcileVerificationNotificationScope(context.Background(), home, r, func(p ProducerReservation) (string, error) {
			if p != *j.Producer {
				t.Fatal("output toggle replaced producer")
			}
			return receipt, nil
		})
		testutil.Must(t, err)
		scope, err := r.Desired.NotificationFingerprint()
		testutil.Must(t, err)
		checked, done, err := InspectVerificationNotificationScope(home, r.Operation, r.BeforeGateway, scope)
		if err != nil || !done || checked != got {
			t.Fatal("output completion", done, err)
		}
		r.BeforeScope, r.BeforeGateway, r.Operation = scope, receipt, testutil.ID(t, "n")
	}
}

func TestVerificationNotificationScopeRechecksPendingSourceKeys(t *testing.T) {
	home, r, j := verificationScopeFixture(t)
	stop := errors.New("pending")
	_, err := ReconcileVerificationNotificationScope(context.Background(), home, r, func(ProducerReservation) (string, error) { return "", stop })
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	for alias := range j.SourceKeys {
		testutil.Must(t, os.Remove(filepath.Join(home, workerTrustDirectory, alias+".key")))
		break
	}
	called := false
	_, err = ReconcileVerificationNotificationScope(context.Background(), home, r, func(ProducerReservation) (string, error) { called = true; return notify.Hash("wrong"), nil })
	if err == nil || called {
		t.Fatal("pending mapping ignored source drift", err)
	}
}

func TestVerificationNotificationScopeCompletionFencesLaterPendingBackup(t *testing.T) {
	home, r, _ := verificationScopeFixture(t)
	_, err := ReconcileVerificationNotificationScope(context.Background(), home, r, func(ProducerReservation) (string, error) { return notify.Hash("completed gateway"), nil })
	testutil.Must(t, err)
	scope, err := r.Desired.NotificationFingerprint()
	testutil.Must(t, err)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var before WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &before))
	for _, kind := range []string{"update", "recovery", "retirement", "expansion", "activity", "removed"} {
		changed := before
		switch kind {
		case "update":
			changed.Updating = true
		case "recovery":
			changed.RecoveryNeeded = true
		case "retirement":
			changed.SourceRetirement = &setupSourceRetirement{}
		case "expansion":
			changed.SourceExpansion = &setupSourceExpansion{}
		case "activity":
			changed.Activity = &declaredBackupActivity{}
		case "removed":
			changed.Stage = "uninstalled"
		}
		testutil.Must(t, root.WriteJSON(backupSetupFile, changed, true))
		if _, _, err := InspectVerificationNotificationScope(home, r.Operation, r.BeforeGateway, scope); err == nil {
			t.Fatal("pending backup admitted notification", kind)
		}
	}
	testutil.Must(t, root.WriteJSON(backupSetupFile, before, true))
}

func TestVerificationLimitsScopeRetainsReservationAcrossLostReply(t *testing.T) {
	home, request, before := verificationScopeFixture(t)
	request.Desired.Notification.StatusLEDs = before.Declaration.Desired.Notification.StatusLEDs
	request.Desired.Notification.Limits.MaxMessages++
	stop := errors.New("gateway committed but reply lost")
	calls := 0
	callback := func(p ProducerReservation) (string, error) {
		calls++
		if p != *before.Producer {
			t.Fatal("producer replaced")
		}
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var pending WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &pending))
		if pending.NotificationScope == nil || pending.RequestHash != before.RequestHash {
			t.Fatal("gateway changed without retained backup predecessor")
		}
		if lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false); err == nil {
			lease.Close()
			t.Fatal("scope transition lost setup lease")
		}
		if calls == 1 {
			return "", stop
		}
		return notify.Hash("completed gateway"), nil
	}
	_, err := ReconcileVerificationLimitsScope(context.Background(), home, request, callback)
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	observed, err := InspectSetup(context.Background(), home)
	if err != nil || observed.Stage != "notification_policy_pending" {
		t.Fatal(observed, err)
	}
	if _, _, err = InspectReservedProducer(home, request.BeforeScope, before.Declaration.NotificationRoot, before.Declaration.NotificationRoute); err == nil {
		t.Fatal("pending scope admitted producer")
	}
	got, err := ReconcileVerificationLimitsScope(context.Background(), home, request, callback)
	testutil.Must(t, err)
	if got != notify.Hash("completed gateway") {
		t.Fatal(got)
	}
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var after WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
	if after.NotificationScope != nil || after.NotificationScopeHistory == nil || after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.Producer, before.Producer) || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || !reflect.DeepEqual(after.OwnedFiles, before.OwnedFiles) {
		t.Fatal("scope update changed runtime ownership")
	}
	tree := policyReviewTree(t, home)
	again, err := ReconcileVerificationLimitsScope(context.Background(), home, request, callback)
	if err != nil || again != got || calls != 2 || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("completed retry changed state or replayed gateway", err, calls)
	}
	p, present, err := InspectReservedProducer(home, after.Declaration.NotificationDesiredHash, after.Declaration.NotificationRoot, after.Declaration.NotificationRoute)
	if err != nil || !present || p != *before.Producer {
		t.Fatal("new scope lost reservation", err)
	}
}

func TestVerificationFreshnessScopeRetainsReservationAcrossLostReply(t *testing.T) {
	home, request, before := verificationScopeFixture(t)
	request.Desired.Notification.StatusLEDs = before.Declaration.Desired.Notification.StatusLEDs
	request.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {MaxSilenceSeconds: 300, MaxEvidenceSeconds: 172800}}
	stop := errors.New("gateway committed but reply lost")
	calls := 0
	callback := func(p ProducerReservation) (string, error) {
		calls++
		if p != *before.Producer {
			t.Fatal("producer replaced")
		}
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var pending WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &pending))
		if pending.NotificationScope == nil || pending.RequestHash != before.RequestHash {
			t.Fatal("gateway changed without retained backup predecessor")
		}
		if lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false); err == nil {
			lease.Close()
			t.Fatal("scope transition lost setup lease")
		}
		if calls == 1 {
			return "", stop
		}
		return notify.Hash("completed gateway"), nil
	}
	_, err := ReconcileVerificationFreshnessScope(context.Background(), home, request, callback)
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	observed, err := InspectSetup(context.Background(), home)
	if err != nil || observed.Stage != "notification_policy_pending" {
		t.Fatal(observed, err)
	}
	if _, _, err = InspectReservedProducer(home, request.BeforeScope, before.Declaration.NotificationRoot, before.Declaration.NotificationRoute); err == nil {
		t.Fatal("pending scope admitted producer")
	}
	got, err := ReconcileVerificationFreshnessScope(context.Background(), home, request, callback)
	testutil.Must(t, err)
	if got != notify.Hash("completed gateway") {
		t.Fatal(got)
	}
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var after WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
	if after.NotificationScope != nil || after.NotificationScopeHistory == nil || after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.Producer, before.Producer) || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || !reflect.DeepEqual(after.OwnedFiles, before.OwnedFiles) {
		t.Fatal("scope update changed runtime ownership")
	}
	tree := policyReviewTree(t, home)
	again, err := ReconcileVerificationFreshnessScope(context.Background(), home, request, callback)
	if err != nil || again != got || calls != 2 || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("completed retry changed state or replayed gateway", err, calls)
	}
	p, present, err := InspectReservedProducer(home, after.Declaration.NotificationDesiredHash, after.Declaration.NotificationRoot, after.Declaration.NotificationRoute)
	if err != nil || !present || p != *before.Producer {
		t.Fatal("new scope lost reservation", err)
	}
}

func TestVerificationRoutesScopeRetainsReservationAcrossLostReply(t *testing.T) {
	home, request, before := verificationScopeFixture(t)
	request.Desired.Notification.StatusLEDs = before.Declaration.Desired.Notification.StatusLEDs
	request.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "telegram"}, "audit": {Primary: "telegram"}}
	stop := errors.New("gateway committed but reply lost")
	calls := 0
	callback := func(p ProducerReservation) (string, error) {
		calls++
		if p != *before.Producer {
			t.Fatal("producer replaced")
		}
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var pending WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &pending))
		if pending.NotificationScope == nil || pending.RequestHash != before.RequestHash {
			t.Fatal("gateway changed without retained backup predecessor")
		}
		if lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false); err == nil {
			lease.Close()
			t.Fatal("scope transition lost setup lease")
		}
		if calls == 1 {
			return "", stop
		}
		return notify.Hash("completed gateway"), nil
	}
	_, err := ReconcileVerificationRoutesScope(context.Background(), home, request, callback)
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	observed, err := InspectSetup(context.Background(), home)
	if err != nil || observed.Stage != "notification_policy_pending" {
		t.Fatal(observed, err)
	}
	if _, _, err = InspectReservedProducer(home, request.BeforeScope, before.Declaration.NotificationRoot, before.Declaration.NotificationRoute); err == nil {
		t.Fatal("pending scope admitted producer")
	}
	got, err := ReconcileVerificationRoutesScope(context.Background(), home, request, callback)
	testutil.Must(t, err)
	if got != notify.Hash("completed gateway") {
		t.Fatal(got)
	}
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var after WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
	if after.NotificationScope != nil || after.NotificationScopeHistory == nil || after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.Producer, before.Producer) || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || !reflect.DeepEqual(after.OwnedFiles, before.OwnedFiles) {
		t.Fatal("scope update changed runtime ownership")
	}
	tree := policyReviewTree(t, home)
	again, err := ReconcileVerificationRoutesScope(context.Background(), home, request, callback)
	if err != nil || again != got || calls != 2 || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("completed retry changed state or replayed gateway", err, calls)
	}
	p, present, err := InspectReservedProducer(home, after.Declaration.NotificationDesiredHash, after.Declaration.NotificationRoot, after.Declaration.NotificationRoute)
	if err != nil || !present || p != *before.Producer {
		t.Fatal("new scope lost reservation", err)
	}
}

func TestVerificationProducerPolicyScopeRetainsReservationAcrossLostReply(t *testing.T) {
	home, request, before := verificationScopeFixture(t)
	request.Desired.Notification.StatusLEDs = before.Declaration.Desired.Notification.StatusLEDs
	request.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {MaxMessages: 700, InteractiveContext: true, ContextKinds: []string{"source"}}}
	stop := errors.New("gateway committed but reply lost")
	calls := 0
	callback := func(p ProducerReservation) (string, error) {
		calls++
		if p != *before.Producer {
			t.Fatal("producer replaced")
		}
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var pending WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &pending))
		if pending.NotificationScope == nil || pending.RequestHash != before.RequestHash {
			t.Fatal("gateway changed without retained backup predecessor")
		}
		if lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false); err == nil {
			lease.Close()
			t.Fatal("scope transition lost setup lease")
		}
		if calls == 1 {
			return "", stop
		}
		return notify.Hash("completed gateway"), nil
	}
	_, err := ReconcileVerificationProducerPolicyScope(context.Background(), home, request, callback)
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	observed, err := InspectSetup(context.Background(), home)
	if err != nil || observed.Stage != "notification_policy_pending" {
		t.Fatal(observed, err)
	}
	if _, _, err = InspectReservedProducer(home, request.BeforeScope, before.Declaration.NotificationRoot, before.Declaration.NotificationRoute); err == nil {
		t.Fatal("pending scope admitted producer")
	}
	got, err := ReconcileVerificationProducerPolicyScope(context.Background(), home, request, callback)
	testutil.Must(t, err)
	if got != notify.Hash("completed gateway") {
		t.Fatal(got)
	}
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var after WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
	if after.NotificationScope != nil || after.NotificationScopeHistory == nil || after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.Producer, before.Producer) || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || !reflect.DeepEqual(after.OwnedFiles, before.OwnedFiles) {
		t.Fatal("scope update changed runtime ownership")
	}
	tree := policyReviewTree(t, home)
	again, err := ReconcileVerificationProducerPolicyScope(context.Background(), home, request, callback)
	if err != nil || again != got || calls != 2 || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("completed retry changed state or replayed gateway", err, calls)
	}
	p, present, err := InspectReservedProducer(home, after.Declaration.NotificationDesiredHash, after.Declaration.NotificationRoot, after.Declaration.NotificationRoute)
	if err != nil || !present || p != *before.Producer {
		t.Fatal("new scope lost reservation", err)
	}
}

func TestVerificationProvidersScopeRetainsReservationAcrossLostReply(t *testing.T) {
	home, request, before := verificationScopeFixture(t)
	request.Desired.Notification.StatusLEDs = before.Declaration.Desired.Notification.StatusLEDs
	providers := config.ProviderDeclarations{}
	for name, p := range request.Desired.Notification.Providers {
		providers[name] = p
	}
	selected := providers["telegram"]
	selected.TokenFile = "~/.config/rotated-token"
	providers["telegram"] = selected
	request.Desired.Notification.Providers = providers
	stop := errors.New("gateway committed but reply lost")
	calls := 0
	callback := func(p ProducerReservation) (string, error) {
		calls++
		if p != *before.Producer {
			t.Fatal("producer replaced")
		}
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var pending WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &pending))
		if pending.NotificationScope == nil || pending.RequestHash != before.RequestHash {
			t.Fatal("gateway changed without retained backup predecessor")
		}
		if lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false); err == nil {
			lease.Close()
			t.Fatal("scope transition lost setup lease")
		}
		if calls == 1 {
			return "", stop
		}
		return notify.Hash("completed gateway"), nil
	}
	_, err := ReconcileVerificationProvidersScope(context.Background(), home, request, callback)
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	observed, err := InspectSetup(context.Background(), home)
	if err != nil || observed.Stage != "notification_policy_pending" {
		t.Fatal(observed, err)
	}
	if _, _, err = InspectReservedProducer(home, request.BeforeScope, before.Declaration.NotificationRoot, before.Declaration.NotificationRoute); err == nil {
		t.Fatal("pending scope admitted producer")
	}
	got, err := ReconcileVerificationProvidersScope(context.Background(), home, request, callback)
	testutil.Must(t, err)
	if got != notify.Hash("completed gateway") {
		t.Fatal(got)
	}
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var after WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
	if after.NotificationScope != nil || after.NotificationScopeHistory == nil || after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.Producer, before.Producer) || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || !reflect.DeepEqual(after.OwnedFiles, before.OwnedFiles) {
		t.Fatal("scope update changed runtime ownership")
	}
	tree := policyReviewTree(t, home)
	again, err := ReconcileVerificationProvidersScope(context.Background(), home, request, callback)
	if err != nil || again != got || calls != 2 || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("completed retry changed state or replayed gateway", err, calls)
	}
	p, present, err := InspectReservedProducer(home, after.Declaration.NotificationDesiredHash, after.Declaration.NotificationRoot, after.Declaration.NotificationRoute)
	if err != nil || !present || p != *before.Producer {
		t.Fatal("new scope lost reservation", err)
	}
}

func TestVerificationCommandsScopeRetainsReservationAcrossLostReply(t *testing.T) {
	testVerificationCommandsScope(t, false)
}
func TestVerificationCommandPolicyScopeRetainsReservationAcrossLostReply(t *testing.T) {
	testVerificationCommandsScope(t, true)
}
func testVerificationCommandsScope(t *testing.T, update bool) {
	home, request, before := verificationScopeFixture(t, true)
	request.Desired.Notification.StatusLEDs = before.Declaration.Desired.Notification.StatusLEDs
	selected := request.Desired.Notification.Providers["telegram"]
	selected.Account, selected.BotName = "123", "fixture_bot"
	request.Desired.Notification.Providers = config.ProviderDeclarations{"telegram": selected}
	request.Desired.Notification.Commands = config.CommandDeclaration{Enabled: true, Mode: "read", Workers: map[string]string{"backup": "backup-worker"}, Conversations: map[string]config.ConversationDeclaration{"ops": {Provider: "telegram", Account: "123", NativeID: "456"}}, Permissions: []config.PermissionDeclaration{{Actor: "789", Conversation: "ops", Worker: "backup", Permissions: []string{"read"}, Scope: config.PermissionScope{WholeWorker: true}}}}
	reconcile := ReconcileVerificationCommandsScope
	if update {
		receipt, err := reconcile(context.Background(), home, request, func(ProducerReservation) (string, error) { return notify.Hash("initial commands"), nil })
		testutil.Must(t, err)
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		testutil.Must(t, root.ReadJSON(backupSetupFile, &before))
		testutil.Must(t, root.Close())
		request.Operation = testutil.ID(t, "n")
		request.BeforeScope = before.Declaration.NotificationDesiredHash
		request.BeforeGateway = receipt
		request.Desired.Notification.Commands.Mode = "mutation"
		request.Desired.Notification.Commands.Permissions[0].Permissions = []string{"read", "incident.manage"}
		reconcile = ReconcileVerificationCommandPolicyScope
	}
	stop := errors.New("gateway committed but reply lost")
	calls := 0
	callback := func(p ProducerReservation) (string, error) {
		calls++
		if p != *before.Producer {
			t.Fatal("producer replaced")
		}
		root, err := safefs.Open(home)
		testutil.Must(t, err)
		defer root.Close()
		var pending WorkerSetupJournal
		testutil.Must(t, root.ReadJSON(backupSetupFile, &pending))
		if pending.NotificationScope == nil || pending.RequestHash != before.RequestHash {
			t.Fatal("gateway changed without retained backup predecessor")
		}
		if lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false); err == nil {
			lease.Close()
			t.Fatal("scope transition lost setup lease")
		}
		if calls == 1 {
			return "", stop
		}
		return notify.Hash("completed gateway"), nil
	}
	_, err := reconcile(context.Background(), home, request, callback)
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	observed, err := InspectSetup(context.Background(), home)
	if err != nil || observed.Stage != "notification_policy_pending" {
		t.Fatal(observed, err)
	}
	if _, _, err = InspectReservedProducer(home, request.BeforeScope, before.Declaration.NotificationRoot, before.Declaration.NotificationRoute); err == nil {
		t.Fatal("pending scope admitted producer")
	}
	got, err := reconcile(context.Background(), home, request, callback)
	testutil.Must(t, err)
	if got != notify.Hash("completed gateway") {
		t.Fatal(got)
	}
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var after WorkerSetupJournal
	testutil.Must(t, root.ReadJSON(backupSetupFile, &after))
	if after.NotificationScope != nil || after.NotificationScopeHistory == nil || after.WorkerID != before.WorkerID || after.ArchiveID != before.ArchiveID || !reflect.DeepEqual(after.Producer, before.Producer) || !reflect.DeepEqual(after.SourceKeys, before.SourceKeys) || !reflect.DeepEqual(after.OwnedFiles, before.OwnedFiles) {
		t.Fatal("scope update changed runtime ownership")
	}
	tree := policyReviewTree(t, home)
	again, err := reconcile(context.Background(), home, request, callback)
	if err != nil || again != got || calls != 2 || !reflect.DeepEqual(tree, policyReviewTree(t, home)) {
		t.Fatal("completed retry changed state or replayed gateway", err, calls)
	}
	p, present, err := InspectReservedProducer(home, after.Declaration.NotificationDesiredHash, after.Declaration.NotificationRoot, after.Declaration.NotificationRoute)
	if err != nil || !present || p != *before.Producer {
		t.Fatal("new scope lost reservation", err)
	}
}
