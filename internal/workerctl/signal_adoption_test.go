package workerctl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func signalAdoptionFixture(t *testing.T, telegram ...bool) (ControlRequest, ResolvedPaths, *install.PolicyReview, signalSetupLifecycle) {
	t.Helper()
	s, op, _ := signalSetupFixture(t)
	s.display = io.Discard
	if err := s.Run(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	record, err := s.record(op)
	if err != nil {
		t.Fatal(err)
	}
	// The service adapter supplies a canonical completed service. Native account
	// reservation/link and inbox records underneath it are the real offline APIs.
	m := install.FreshSignalManifest{Spec: record.Spec, RootDevice: 1, RootInode: 1, OperationDevice: 1, OperationInode: 2, RuntimeDevice: 1, RuntimeInode: 3, UnitHash: inputDigest([]byte("fixture unit"))}
	m.ManifestHash = protocol.Hash(m)
	r, _ := bootstrapControlFixture(t)
	r.Facts = s.request.Facts
	r.HelperSHA256 = s.request.HelperSHA256
	r.Desired.Backup = nil
	r.Desired.Notification.Providers = config.ProviderDeclarations{"signal": {AccountFile: record.Spec.AccountFile, RecipientFile: record.Spec.RecipientFile, Socket: record.Spec.Socket, Data: record.Spec.AccountState, Executable: record.Spec.CLIFile, ExecutableSHA256: record.Spec.CLISHA256}}
	r.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "signal"}}
	secrets := map[string][]byte{"signal_account": s.account, "signal_recipient": s.recipient}
	if len(telegram) > 0 && telegram[0] {
		p := config.ProviderDeclaration{TokenFile: filepath.Join(t.TempDir(), "token"), ChatIDFile: filepath.Join(t.TempDir(), "chat")}
		secrets["token"], secrets["chat_id"] = []byte("123:combined_fixture"), []byte("456")
		for path, body := range map[string][]byte{p.TokenFile: secrets["token"], p.ChatIDFile: secrets["chat_id"]} {
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
		}
		r.Desired.Notification.Providers["telegram"] = p
		r.Desired.Notification.Routes["alerts"] = config.NotificationRoute{Primary: "telegram", Fallback: "signal"}
	}
	r.Action, r.DesiredScope, r.Inputs = "adopt", 1, nil
	r.DesiredHash, _ = r.Desired.NotificationFingerprint()
	paths := s.gateway
	paths.Control = filepath.Join(r.Facts.Home, "adopt-control")
	paths.Socket = filepath.Join(paths.Installation, "api.sock")
	input, err := CompileFreshSignalNotification(r.Desired, paths, r.Facts.Home, secrets, m)
	if err != nil {
		t.Fatal(err)
	}
	for name, route := range input.Worker.Routes {
		route.Revision = "manual-native-route"
		input.Worker.Routes[name] = route
	}
	raw, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	c, err := input.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	d, err := ensureConfigDirectory(paths.Installation)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	file := filepath.Join(paths.Installation, "manual-runtime.json")
	if err = publishPrivateInput(file, raw); err != nil {
		t.Fatal(err)
	}
	review := &install.PolicyReview{Input: input, Runtime: raw, Signal: &m.Spec, Native: &m, Manifest: install.ManagedManifest{Version: 1, Identity: protocol.Identity{Worker: "native-adopted-gateway", Recovery: "native-retained-recovery"}, Root: paths.Installation, UnitDirectory: paths.UnitDirectory, Mode: "verification", BinaryHash: r.HelperSHA256, RuntimeHash: inputDigest(raw), ConfigHash: protocol.Hash(c), FreshSignal: &install.FreshSignalReference{Root: m.Spec.Root, ManifestHash: m.ManifestHash}, Paths: install.ServicePaths{Config: file, State: paths.State, Socket: paths.Socket, DeniedPaths: paths.DeniedPaths}}}
	l := signalSetupLifecycle{
		inspect: func(context.Context, install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
			return m, true, nil
		},
		ensure: func(context.Context, install.FreshSignalSpec) (install.FreshSignalManifest, error) {
			t.Fatal("adoption repeated service setup")
			return m, nil
		},
		gateway: func(context.Context, string) (install.InstallationObservation, error) {
			return install.InstallationObservation{Manifest: review.Manifest, Phase: "complete", Unbound: true}, nil
		},
	}
	return r, paths, review, l
}

func TestSignalCoordinatorAdoptionRetainsNativeReceipts(t *testing.T) {
	for _, at := range []string{"adoption-staged", "adoption-ready", "adoption-published", "adoption-runtime", "complete"} {
		t.Run(at, func(t *testing.T) {
			r, paths, review, native := signalAdoptionFixture(t)
			accountBefore, inboxBefore := planTree(t, review.Native.Spec.AccountState), planTree(t, review.Native.Spec.Inbox.State)
			stop := errors.New("lost native adoption reply")
			l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }, boundary: func(boundary string) error {
				if boundary == at {
					return stop
				}
				return nil
			}}
			if _, handled, err := reconcileNotificationAdoption(context.Background(), r, paths, l); !handled || !errors.Is(err, stop) {
				t.Fatal(handled, err)
			}
			if at == "adoption-staged" || at == "adoption-ready" {
				if _, err := os.Stat(paths.Control); !os.IsNotExist(err) {
					t.Fatal("published partial native coordinator", err)
				}
			}
			l.boundary = nil
			r.Action = "resume"
			for range 2 {
				if got, handled, err := reconcileNotificationAdoption(context.Background(), r, paths, l); err != nil || !handled || got.Code != "ok" {
					t.Fatal(got, handled, err)
				}
			}
			r.SignalSHA256 = review.Native.Spec.CLISHA256
			for range 2 {
				m, _, _, err := prepareDeclaredSignal(context.Background(), r, paths, "/unused-helper", "", nil, native)
				if err != nil || protocol.Hash(m) != protocol.Hash(review.Native) {
					t.Fatal("adopted child reuse", err)
				}
			}
			d, err := safefs.Open(paths.Control)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err = verifyLEDOutputInputs(r, paths, d, r.Desired); err != nil {
				t.Fatal("adopted native input pins", err)
			}
			inspect := native.gateway
			if got, err := inspectNotificationControl(context.Background(), r, paths, inspect, nil); err != nil || got.State != "waiting_for_producers" {
				t.Fatal(got, err)
			}
			if got, err := planNotificationControl(context.Background(), r, paths, inspect); err != nil || got.State != "up_to_date" {
				t.Fatal(got, err)
			}
			if !reflect.DeepEqual(accountBefore, planTree(t, review.Native.Spec.AccountState)) || !reflect.DeepEqual(inboxBefore, planTree(t, review.Native.Spec.Inbox.State)) {
				t.Fatal("adoption changed account or inbox history")
			}
			// The gateway helper can later evolve independently. The imported
			// native child keeps its original executable and service receipt.
			if at == "complete" {
				r.Action = "apply"
				r.HelperSHA256 = inputDigest([]byte("new gateway helper"))
				release := notificationReleaseLifecycle{inspect: native.gateway, update: func(_ context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
					if protocol.Hash(review.Manifest) != expected {
						t.Fatal("changed gateway predecessor")
					}
					review.Manifest.BinaryHash = spec.BinarySHA256
					review.Manifest.Revision, _ = safefs.NewID("n")
					return review.Manifest, nil
				}}
				if _, handled, err := reconcileNotificationRelease(context.Background(), r, paths, "/new-helper", release); err != nil || !handled {
					t.Fatal("adopted gateway helper update", handled, err)
				}
				if m, _, _, err := prepareDeclaredSignal(context.Background(), r, paths, "/new-helper", "", nil, native); err != nil || protocol.Hash(m) != protocol.Hash(review.Native) {
					t.Fatal("helper update changed adopted native service", err)
				}
				r.Action = "resume"
				if m, _, _, err := prepareDeclaredSignal(context.Background(), r, paths, "/new-helper", "", nil, native); err != nil || protocol.Hash(m) != protocol.Hash(review.Native) {
					t.Fatal("native reuse after gateway update", err)
				}
			}
			count, err := os.ReadFile(filepath.Join(review.Native.Spec.AccountState, "fixture-link-count"))
			if err != nil || string(count) != "1" {
				t.Fatal("adoption relinked native account", err)
			}
			var original OperationJournal
			if err = d.ReadJSON("operation.json", &original); err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(filepath.Join(paths.Control, "signal", "service.json")); err != nil {
				t.Fatal(err)
			}
			if _, _, err = reconcileNotificationAdoption(context.Background(), r, paths, l); err == nil && at != "complete" {
				t.Fatal("missing native receipt recreated")
			}
			if _, err = inspectNotificationControl(context.Background(), r, paths, native.gateway, nil); err == nil {
				t.Fatal("missing native child reported healthy")
			}
			if _, err = testNotificationControl(context.Background(), r, paths, func(context.Context, string, string) (install.DiagnosticResult, error) {
				t.Fatal("missing native child reached provider send")
				return install.DiagnosticResult{}, nil
			}); err == nil {
				t.Fatal("missing native child permitted diagnostics")
			}
			if _, _, _, err = prepareDeclaredSignal(context.Background(), r, paths, "/unused", "", nil, native); err == nil {
				t.Fatal("missing native receipt repaired through setup")
			}
		})
	}
}

func TestSignalCoordinatorAdoptionRetainsExplicitServiceResume(t *testing.T) {
	r, paths, review, native := signalAdoptionFixture(t)
	l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }}
	if _, _, err := reconcileNotificationAdoption(context.Background(), r, paths, l); err != nil {
		t.Fatal(err)
	}
	r.Action = "resume"
	r.SignalSHA256 = review.Native.Spec.CLISHA256
	stopped, starts := true, 0
	native.inspect = func(context.Context, install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
		if stopped {
			return *review.Native, false, install.FreshSignalResumeRequired{Reference: *review.Manifest.FreshSignal}
		}
		return *review.Native, true, nil
	}
	native.resume = func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
		if ref != *review.Manifest.FreshSignal {
			t.Fatal("different native owner")
		}
		stopped = false
		starts++
		return *review.Native, nil
	}
	for range 2 {
		if err := resumeCompletedSignalChild(context.Background(), r, paths, "/unused", "", native); err != nil {
			t.Fatal(err)
		}
	}
	if starts != 1 {
		t.Fatal("native resume repeated setup", starts)
	}
	count, err := os.ReadFile(filepath.Join(review.Native.Spec.AccountState, "fixture-link-count"))
	if err != nil || string(count) != "1" {
		t.Fatal("resume relinked account", err)
	}
}

func TestSignalCoordinatorAdoptionProcessDeath(t *testing.T) {
	type fixture struct {
		Request ControlRequest
		Paths   ResolvedPaths
		Review  install.PolicyReview
	}
	if file := os.Getenv("DEPLOY_NATIVE_ADOPTION_CRASH_INPUT"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var f fixture
		if err = json.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return f.Review, nil }, boundary: func(at string) error {
			if at == os.Getenv("DEPLOY_NATIVE_ADOPTION_CRASH_AT") {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
			return nil
		}}
		_, _, err = reconcileNotificationAdoption(context.Background(), f.Request, f.Paths, l)
		t.Fatal("crash not reached", err)
	}
	for _, at := range []string{"adoption-staged", "adoption-ready", "adoption-published", "adoption-runtime", "recorded/notification-adopt"} {
		t.Run(at, func(t *testing.T) {
			r, paths, review, _ := signalAdoptionFixture(t)
			file := filepath.Join(t.TempDir(), "fixture.json")
			raw, _ := json.Marshal(fixture{r, paths, *review})
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSignalCoordinatorAdoptionProcessDeath$")
			cmd.Env = append(os.Environ(), "DEPLOY_NATIVE_ADOPTION_CRASH_INPUT="+file, "DEPLOY_NATIVE_ADOPTION_CRASH_AT="+at)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not killed: %v %s", err, out)
			}
			l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }}
			r.Action = "resume"
			for range 2 {
				if got, handled, err := reconcileNotificationAdoption(ctx, r, paths, l); err != nil || !handled || got.Code != "ok" {
					t.Fatal(got, handled, err)
				}
			}
			d, err := safefs.Open(paths.Control)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var j OperationJournal
			if err = d.ReadJSON("operation.json", &j); err != nil {
				t.Fatal(err)
			}
			if err = verifyAdoptionReceipt(d, j, review.Manifest); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSignalCoordinatorAdoptionRefusesChangedOrUnownedInputs(t *testing.T) {
	for _, damage := range []string{"native-receipt", "account-marker", "native-helper", "native-path", "recipient", "local-input", "owned-layout"} {
		t.Run(damage, func(t *testing.T) {
			r, paths, review, _ := signalAdoptionFixture(t)
			switch damage {
			case "native-receipt":
				review.Native = nil
			case "account-marker":
				if err := os.Remove(filepath.Join(review.Native.Spec.AccountState, "deploy-native-bootstrap.json")); err != nil {
					t.Fatal(err)
				}
			case "native-helper":
				review.Native.Spec.BinarySHA256 = inputDigest([]byte("other helper"))
			case "native-path":
				p := r.Desired.Notification.Providers["signal"]
				p.Data = filepath.Join(r.Facts.Home, "other")
				r.Desired.Notification.Providers["signal"] = p
			case "recipient":
				if err := os.WriteFile(review.Native.Spec.RecipientFile, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "local-input":
				p := r.Desired.Notification.Providers["signal"]
				p.AccountInput, p.AccountFile = "/operator/account", ""
				r.Desired.Notification.Providers["signal"] = p
			case "owned-layout":
				if err := os.Mkdir(paths.Control, 0700); err != nil {
					t.Fatal(err)
				}
			}
			r.DesiredHash, _ = r.Desired.NotificationFingerprint()
			l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }}
			if _, handled, err := reconcileNotificationAdoption(context.Background(), r, paths, l); !handled || err == nil {
				t.Fatal("invalid native takeover", handled, err)
			}
			if _, err := os.Stat(filepath.Join(paths.Control, "operation.json")); !os.IsNotExist(err) {
				t.Fatal("invalid native takeover published", err)
			}
		})
	}
}

func TestAdoptionDoesNotInterceptPendingFreshSignalSetup(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	d, err := ensureConfigDirectory(filepath.Join(paths.Control, "signal"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.WriteJSON("operation.json", map[string]string{"fixture": "only normal setup validates this"}, false); err != nil {
		t.Fatal(err)
	}
	r.Action = "resume"
	l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) {
		t.Fatal("fresh setup was treated as adoption")
		return install.PolicyReview{}, nil
	}}
	if _, handled, err := reconcileNotificationAdoption(context.Background(), r, paths, l); handled || err != nil {
		t.Fatal(handled, err)
	}
	if err = os.MkdirAll(paths.State, 0700); err != nil {
		t.Fatal(err)
	}
	if _, handled, err := reconcileNotificationAdoption(context.Background(), r, paths, l); !handled || err == nil {
		t.Fatal("existing gateway lost authority", handled, err)
	}
}
