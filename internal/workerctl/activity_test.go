package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestDeclaredActivityLostRepliesAndRepeatedCycles(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	ctx := context.Background()
	r, paths, o := notificationOutputFixture(t)
	o.LEDs = true
	identity := o.Manifest.Identity
	var effects []string
	receipts := map[string]install.ManagedManifest{}
	transition := func(name, expected string, change func(*install.ManagedManifest)) (install.ManagedManifest, error) {
		key := name + expected
		if m, ok := receipts[key]; ok {
			return m, nil
		}
		if expected != protocol.Hash(o.Manifest) {
			t.Fatal("stale predecessor reached effect", name)
		}
		change(&o.Manifest)
		effects = append(effects, name)
		receipts[key] = o.Manifest
		return o.Manifest, errors.New("lost domain reply")
	}
	l := notificationActivityLifecycle{
		inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil },
		stop: func(_ context.Context, _, expected string) (install.ManagedManifest, error) {
			return transition("stop", expected, func(m *install.ManagedManifest) { m.Mode = "uninstalled" })
		},
		restore: func(_ context.Context, _, expected string) (install.ManagedManifest, error) {
			return transition("restore", expected, func(m *install.ManagedManifest) { m.Mode = "verification"; m.Revision, _ = safefs.NewID("n") })
		},
		output: func(_ context.Context, _, expected string, disabled bool) (install.ManagedManifest, error) {
			name := "output-on"
			if disabled {
				name = "output-off"
			}
			return transition(name, expected, func(m *install.ManagedManifest) {
				id, _ := safefs.NewID("n")
				m.LEDOutput = &install.ManagedLEDOutput{Operation: id, Disabled: disabled}
			})
		},
	}
	for _, enabled := range []bool{false, true, false, true} {
		value := enabled
		r.Desired.Notification.Enabled = &value
		r.DesiredHash, _ = declarationFingerprint(r.Desired, r.DesiredScope)
		want := "disabled"
		if enabled {
			want = "waiting_for_producers"
		}
		failures := 0
		for {
			got, handled, err := reconcileNotificationActivity(ctx, r, paths, l)
			if !handled {
				t.Fatal("activity ignored")
			}
			if err != nil {
				failures++
				failure := controlFailure(err)
				if failures > 2 || failure.Code != "activity_pending" || !strings.Contains(failure.Next, "resume manager") {
					t.Fatal("cannot resume", failure, err)
				}
				continue
			}
			if failures != 2 || got.State != want || got.Identity == nil || *got.Identity != identity {
				t.Fatal("activity completion changed identity", failures, got)
			}
			break
		}
		count := len(effects)
		for range 2 {
			got, _, err := reconcileNotificationActivity(ctx, r, paths, l)
			if err != nil || got.State != want || len(effects) != count {
				t.Fatal("unchanged activity repeated effect", got, err)
			}
		}
		status, err := inspectNotificationControl(ctx, r, paths, l.inspect, nil)
		if err != nil || status.State != want {
			t.Fatal("activity status lost authority", status, err)
		}
		plan, err := planNotificationControl(ctx, r, paths, l.inspect)
		if err != nil || plan.State != "up_to_date" {
			t.Fatal("activity plan lost policy", plan, err)
		}
		sent := false
		_, err = testNotificationControl(ctx, r, paths, func(_ context.Context, root, receipt string) (install.DiagnosticResult, error) {
			sent = true
			if root != paths.Installation || receipt != protocol.Hash(o.Manifest) {
				t.Fatal("activity diagnostic lost receipt")
			}
			return install.DiagnosticResult{Status: "provider_accepted"}, nil
		})
		if sent != enabled || (err == nil) != enabled {
			t.Fatal("diagnostic did not respect YAML activity", enabled, sent, err)
		}
	}
	want := []string{"output-off", "stop", "restore", "output-on", "output-off", "stop", "restore", "output-on"}
	if !reflect.DeepEqual(effects, want) {
		t.Fatal("wrong activity order", effects)
	}
	d, err := safefs.Open(paths.Control)
	if err != nil {
		t.Fatal(err)
	}
	var j OperationJournal
	err = d.ReadJSON("operation.json", &j)
	d.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(paths.Control + "/activity-" + j.Children[0].Operation + ".json"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = reconcileNotificationActivity(ctx, r, paths, l); err == nil {
		t.Fatal("lost completion receipt recreated")
	}
	if _, err = inspectNotificationControl(ctx, r, paths, l.inspect, nil); err == nil {
		t.Fatal("lost completion reported healthy")
	}
}

func TestDeclaredActivityRefusesBoundOrPolicyChangesBeforeIntent(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, kind := range []string{"bound", "route", "helper", "uninstall"} {
		t.Run(kind, func(t *testing.T) {
			r, paths, o := notificationOutputFixture(t)
			before, err := os.ReadFile(paths.Control + "/operation.json")
			if err != nil {
				t.Fatal(err)
			}
			o.Unbound = kind != "bound"
			if kind == "route" {
				r.Desired.Notification.Limits.MaxMessages++
			}
			if kind == "helper" {
				r.HelperSHA256 = strings.Repeat("b", 64)
			}
			if kind == "uninstall" {
				d, _ := safefs.Open(paths.Control)
				var j OperationJournal
				if err = d.ReadJSON("operation.json", &j); err != nil {
					t.Fatal(err)
				}
				j.Pins.Action = "uninstall"
				if err = d.WriteJSON("operation.json", j, true); err != nil {
					t.Fatal(err)
				}
				d.Close()
				before, _ = os.ReadFile(paths.Control + "/operation.json")
			}
			no := false
			r.Desired.Notification.Enabled = &no
			r.DesiredHash, _ = r.Desired.Fingerprint()
			l := notificationActivityLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil }}
			if _, handled, err := reconcileNotificationActivity(context.Background(), r, paths, l); !handled || err == nil {
				t.Fatal("unsafe activity accepted", handled, err)
			}
			after, _ := os.ReadFile(paths.Control + "/operation.json")
			if string(before) != string(after) {
				t.Fatal("rejected activity changed retained intent")
			}
		})
	}
}

func TestDisabledAbsentWorkerDoesNotCreateStateOrReadCredentials(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	no := false
	r.Desired.Notification.Enabled = &no
	r.DesiredHash, _ = r.Desired.Fingerprint()
	if err := prepareOperatorNotificationInputs(&r, ResolvedArtifacts{}, r.Facts.Arch); err != nil {
		t.Fatal(err)
	}
	got, handled, err := reconcileNotificationActivity(context.Background(), r, paths, notificationActivityLifecycle{})
	if err != nil || !handled || got.State != "not_installed" {
		t.Fatal(got, handled, err)
	}
	if _, err = os.Stat(paths.Control); !os.IsNotExist(err) {
		t.Fatal("disabled declaration initialized state")
	}
	d, err := ensureConfigDirectory(paths.State)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	if _, _, err = reconcileNotificationActivity(context.Background(), r, paths, notificationActivityLifecycle{}); err == nil {
		t.Fatal("unowned state treated as absent")
	}
}

func TestExplicitUninstallAfterYAMLDisable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	r, paths, o := notificationOutputFixture(t)
	no := false
	r.Desired.Notification.Enabled = &no
	r.DesiredHash, _ = r.Desired.Fingerprint()
	stops := 0
	l := notificationActivityLifecycle{
		inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil },
		stop: func(_ context.Context, _, expected string) (install.ManagedManifest, error) {
			if protocol.Hash(o.Manifest) != expected {
				t.Fatal("disable used wrong receipt")
			}
			stops++
			o.Manifest.Mode = "uninstalled"
			return o.Manifest, nil
		},
	}
	ctx := context.Background()
	if _, _, err := reconcileNotificationActivity(ctx, r, paths, l); err != nil {
		t.Fatal(err)
	}
	r.Action = "uninstall"
	for range 2 {
		result, err := uninstallNotificationWithLED(ctx, r, paths, l.inspect, l.stop, nil)
		if err != nil || result.State != "uninstalled" || stops != 1 {
			t.Fatal("disabled uninstall repeated stop", result, err)
		}
	}
	status, err := inspectNotificationControl(ctx, r, paths, l.inspect, nil)
	if err != nil || status.State != "uninstalled" {
		t.Fatal("explicit removal was still disabled", status, err)
	}
	r.Action = "apply"
	if _, handled, err := reconcileNotificationActivity(ctx, r, paths, l); !handled || err == nil {
		t.Fatal("explicit removal was adopted as activity intent")
	}
}

func TestShortDisabledNotificationDoesNotRequestCredentialsOrArtifacts(t *testing.T) {
	dir := t.TempDir()
	helper := elfFixture(t, "arm64")
	raw, err := os.ReadFile(helper.File)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "deploy"), raw, 0700); err != nil {
		t.Fatal(err)
	}
	helper.File = "deploy"
	raw, err = json.Marshal(Catalog{Version: 1, Release: "fixture", Linux: map[string]ArchitectureArtifacts{"arm64": {Helper: helper}}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := filepath.Join(dir, "catalog.json")
	if err = os.WriteFile(catalog, raw, 0600); err != nil {
		t.Fatal(err)
	}
	declaration := `version: 3
servers:
  manager:
    host: raspberrypi.local
    user: deploy
    notification_worker:
      enabled: false
      providers:
        telegram:
          token_input: /missing/private-token
          chat_id_input: /missing/private-chat
        signal: {}
      status_leds:
        enabled: true
`
	inventory := filepath.Join(dir, "server.yaml")
	if err = os.WriteFile(inventory, []byte(declaration), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	var out bytes.Buffer
	c := CLI{Inspector: &countingInspector{}, Out: &out, Controller: qualificationController(func(r ControlRequest) (ControlResult, error) {
		calls++
		if config.WorkerEnabled(r.Desired.Notification.Enabled) || len(r.Inputs) != 0 || r.LocalSignal != nil || r.LocalLED != nil || r.InteractiveLink {
			t.Fatal("disabled apply requested new inputs", r.Action)
		}
		return ControlResult{Version: 1, State: "disabled", Code: "ok"}, nil
	})}
	for _, args := range [][]string{{"manager"}, {"resume", "manager"}} {
		out.Reset()
		if err = c.Run(context.Background(), "notification-worker", args, Options{Inventory: inventory, Catalog: catalog, JSON: true, NonInteractive: true}); err != nil {
			t.Fatal(err, out.String())
		}
	}
	if calls != 2 {
		t.Fatal("disable did not reach target", calls)
	}
}
