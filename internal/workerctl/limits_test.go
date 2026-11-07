package workerctl

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/led"
	"os"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func notificationLimitsFixture(t *testing.T) (ControlRequest, ResolvedPaths, *install.InstallationObservation, *safefs.Directory) {
	t.Helper()
	r, paths, o, d := notificationReleaseFixture(t)
	raw, err := readLimitsRuntime(paths.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		t.Fatal(err)
	}
	c, err := input.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	o.Manifest.ConfigHash, o.Manifest.RuntimeHash = protocol.Hash(c), inputDigest(raw)
	o.Manifest.Revision, _ = safefs.NewID("n")
	o.Manifest.Installation, _ = safefs.NewID("n")
	o.Manifest.HelperHash = r.HelperSHA256
	var j OperationJournal
	if err = d.ReadJSON("operation.json", &j); err != nil {
		t.Fatal(err)
	}
	j.Children[0].Receipt = protocol.Hash(o.Manifest)
	if err = d.WriteJSON("operation.json", j, true); err != nil {
		t.Fatal(err)
	}
	return r, paths, o, d
}

func TestNotificationLimitsLostRepliesAndFollowingLifecycles(t *testing.T) {
	testNotificationPolicyLostReplies(t, "")
}
func TestNotificationRoutesLostRepliesAndFollowingLifecycles(t *testing.T) {
	testNotificationPolicyLostReplies(t, "routes")
}
func testNotificationPolicyLostReplies(t *testing.T, kind string) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, withOutput := range []bool{false, true} {
		for _, boundary := range []string{"limits-intent", "limits-domain-complete", "limits-runtime-published", "limits-inputs-published", "domain-reply"} {
			t.Run(boundary+map[bool]string{false: "/plain", true: "/led"}[withOutput], func(t *testing.T) {
				r, paths, o, d := notificationLimitsFixture(t)
				ctx := context.Background()
				identity := o.Manifest.Identity
				if withOutput {
					// Use the real output domain and an empty fixture daemon for the predecessor.
					r.Desired.Notification.StatusLEDs.Enabled = false
					r.DesiredHash, _ = r.Desired.Fingerprint()
					l := notificationLEDLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil }}
					l.set = func(ctx context.Context, root, expected string, disabled bool) (install.ManagedManifest, error) {
						raw, err := readLimitsRuntime(paths.Runtime)
						if err != nil {
							return o.Manifest, err
						}
						var input install.RuntimeInput
						if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
							return o.Manifest, err
						}
						c, err := input.Resolve()
						if err != nil {
							return o.Manifest, err
						}
						op, _ := safefs.NewID("n")
						_, err = worker.ConfigureLEDOutput(ctx, paths.State, c.Worker, worker.LEDOutputRequest{Identity: identity, Operation: op, Socket: c.LEDSocket, Disabled: disabled}, limitsLEDAPI{})
						if err == nil {
							o.Manifest.LEDOutput = &install.ManagedLEDOutput{Operation: op, Disabled: disabled}
						}
						return o.Manifest, err
					}
					if _, handled, err := reconcileDeclaredLEDOutput(ctx, r, paths, l); err != nil || !handled {
						t.Fatal("output predecessor", err)
					}
				}
				if kind == "providers" {
					changeDeclaredTelegramFiles(t, &r, paths, boundary != "domain-reply")
				} else if kind == "routes" {
					r.Desired.Notification.Routes["audit"] = config.NotificationRoute{Primary: "telegram"}
				} else {
					r.Desired.Notification.Limits.MaxMessages--
					r.Desired.Notification.Limits.ReceiptAuditDays = 180
				}
				r.DesiredHash, _ = r.Desired.Fingerprint()
				calls := 0
				stop := errors.New("lost limits response")
				l := notificationLimitsLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil }}
				l.boundary = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				l.migrate = func(ctx context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
					calls++
					if expected != protocol.Hash(o.Manifest) || spec.BinarySHA256 != r.HelperSHA256 || spec.RuntimeFile == paths.Runtime {
						t.Fatal("unreviewed migration")
					}
					beforeRaw, e := readLimitsRuntime(paths.Runtime)
					if e != nil {
						t.Fatal(e)
					}
					raw, e := readLimitsRuntime(spec.RuntimeFile)
					if e != nil {
						t.Fatal(e)
					}
					var beforeInput, afterInput install.RuntimeInput
					if e = safefs.DecodeJSON(beforeRaw, &beforeInput, maxControlFrame); e != nil {
						t.Fatal(e)
					}
					if e = safefs.DecodeJSON(raw, &afterInput, maxControlFrame); e != nil {
						t.Fatal(e)
					}
					before, e := beforeInput.Resolve()
					if e != nil {
						t.Fatal(e)
					}
					after, e := afterInput.Resolve()
					if e != nil {
						t.Fatal(e)
					}
					op, _ := safefs.NewID("n")
					migrate := worker.MigrateBootstrapLimits
					if kind == "routes" || kind == "providers" {
						migrate = worker.MigrateBootstrapRoutes
					}
					if kind == "providers" && protocol.Hash(before.Worker) == protocol.Hash(after.Worker) {
						e = worker.PreflightBootstrapRoutes(ctx, paths.State, identity, before.Worker, after.Worker)
					} else {
						_, e = migrate(ctx, paths.State, identity, before.Worker, after.Worker, op)
					}
					if e != nil {
						t.Fatal(e)
					}
					o.Manifest.ConfigHash, o.Manifest.RuntimeHash = protocol.Hash(after), inputDigest(raw)
					o.Manifest.Revision, _ = safefs.NewID("n")
					o.Manifest.HelperRevision = o.Manifest.Revision
					o.Manifest.HelperHash = o.Manifest.BinaryHash
					if boundary == "domain-reply" {
						return o.Manifest, stop
					}
					return o.Manifest, nil
				}
				if _, handled, err := reconcileNotificationPolicy(ctx, r, paths, "/helper", l, kind); !handled || !errors.Is(err, stop) {
					t.Fatal("lost reply", handled, err)
				}
				l.boundary = nil
				for range 2 {
					got, handled, err := reconcileNotificationPolicy(ctx, r, paths, "/helper", l, kind)
					if err != nil || !handled || got.State != "waiting_for_producers" || got.Identity == nil || *got.Identity != identity || calls != 1 {
						t.Fatal("resume", got, handled, err, calls)
					}
				}
				if got, err := inspectNotificationControl(ctx, r, paths, l.inspect, nil); err != nil || got.State != "waiting_for_producers" {
					t.Fatal("status", got, err)
				}
				if got, err := planNotificationControl(ctx, r, paths, l.inspect); err != nil || got.State != "up_to_date" {
					t.Fatal("plan", got, err)
				}
				if err := verifyLEDOutputInputs(r, paths, d, r.Desired); err != nil {
					t.Fatal("new input pins unavailable", err)
				}
				var completed OperationJournal
				if err := d.ReadJSON("operation.json", &completed); err != nil {
					t.Fatal(err)
				}
				intentPath := paths.Control + "/limits-" + completed.Children[0].Operation + ".json"
				if err := os.Rename(intentPath, intentPath+".lost"); err != nil {
					t.Fatal(err)
				}
				if _, _, err := reconcileNotificationPolicy(ctx, r, paths, "/helper", l, kind); err == nil {
					t.Fatal("missing completion recreated")
				}
				if _, err := inspectNotificationControl(ctx, r, paths, l.inspect, nil); err == nil {
					t.Fatal("missing completion reported healthy")
				}
				if calls != 1 {
					t.Fatal("lost evidence repeated domain mutation")
				}
				if err := os.Rename(intentPath+".lost", intentPath); err != nil {
					t.Fatal(err)
				}
				if _, err := testNotificationControl(ctx, r, paths, func(context.Context, string, string) (install.DiagnosticResult, error) {
					return install.DiagnosticResult{Status: "provider_accepted"}, nil
				}); err != nil {
					t.Fatal("explicit diagnostic", err)
				}
				// Scope conversion reuses the revised runtime and does not reapply limits.
				r.DesiredScope = 1
				r.DesiredHash, _ = r.Desired.NotificationFingerprint()
				if _, handled, err := reconcileNotificationRelease(ctx, r, paths, "/helper", notificationReleaseLifecycle{inspect: l.inspect, update: func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error) {
					return o.Manifest, nil
				}}); err != nil || !handled {
					t.Fatal("scope after limits", handled, err)
				}
			})
		}
	}

}

func TestNotificationLimitsRefusesDriftAndMissingCompletion(t *testing.T) {
	testNotificationPolicyRefusesDrift(t, "")
}
func TestNotificationRoutesRefusesDriftAndMissingCompletion(t *testing.T) {
	testNotificationPolicyRefusesDrift(t, "routes")
}
func testNotificationPolicyRefusesDrift(t *testing.T, policyKind string) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, kind := range []string{"bound", "other-policy", "credentials", "snapshot", "pending", "led-history"} {
		t.Run(kind, func(t *testing.T) {
			r, paths, o, d := notificationLimitsFixture(t)
			switch kind {
			case "bound":
				o.Unbound = false
			case "other-policy":
				r.Desired.Notification.Limits.MaxMessages--
				r.Desired.Notification.Commands.Enabled = true
			case "credentials":
				r.Inputs["token"] = []byte("123:changed")
			case "snapshot":
				if err := os.Remove(paths.Control + "/declarations/" + r.DesiredHash + ".json"); err != nil {
					t.Fatal(err)
				}
			case "pending":
				var j OperationJournal
				if err := d.ReadJSON("operation.json", &j); err != nil {
					t.Fatal(err)
				}
				j.Complete = false
				if err := d.WriteJSON("operation.json", j, true); err != nil {
					t.Fatal(err)
				}
			case "led-history":
				o.Manifest.LEDOutput = &install.ManagedLEDOutput{Disabled: true}
			}
			if policyKind == "routes" {
				r.Desired.Notification.Routes["audit"] = config.NotificationRoute{Primary: "telegram"}
			} else {
				r.Desired.Notification.Limits.MaxMessages--
			}
			r.DesiredHash, _ = r.Desired.Fingerprint()
			old, _ := os.ReadFile(paths.Control + "/operation.json")
			l := notificationLimitsLifecycle{inspect: func(context.Context, string) (install.InstallationObservation, error) { return *o, nil }, migrate: func(context.Context, install.ManagedSpec, string) (install.ManagedManifest, error) {
				t.Fatal("unsafe domain migration")
				return install.ManagedManifest{}, nil
			}}
			if _, handled, err := reconcileNotificationPolicy(context.Background(), r, paths, "/helper", l, policyKind); !handled || err == nil {
				t.Fatal("unsafe limits accepted", handled, err)
			}
			got, _ := os.ReadFile(paths.Control + "/operation.json")
			if string(got) != string(old) {
				t.Fatal("preflight rejection changed coordinator")
			}
		})
	}
}

// The notification fixture has no emitted output; no clear or set is needed.
type limitsLEDAPI struct{}

func (limitsLEDAPI) Status(context.Context) (led.DaemonStatus, error) {
	return led.DaemonStatus{Version: "0.1.0", LEDs: map[string]json.RawMessage{"green": json.RawMessage(`{"enabled":true,"available":true,"device":"ACT","default_pattern":"off"}`)}}, nil
}
func (limitsLEDAPI) List(context.Context) ([]led.Entry, error) { return nil, nil }
func (limitsLEDAPI) Clear(context.Context, string) error       { return errors.New("unexpected LED clear") }
func (limitsLEDAPI) Set(context.Context, led.State) error      { return errors.New("unexpected LED set") }
