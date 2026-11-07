package workerctl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func adoptionFixture(t *testing.T) (ControlRequest, ResolvedPaths, *install.PolicyReview) {
	t.Helper()
	r, paths := bootstrapControlFixture(t)
	p := r.Desired.Notification.Providers["telegram"]
	p.TokenFile, p.ChatIDFile = filepath.Join(r.Facts.Home, "token"), filepath.Join(r.Facts.Home, "chat")
	p.TokenInput, p.ChatIDInput = "", ""
	r.Desired.Notification.Providers["telegram"] = p
	if err := os.WriteFile(p.TokenFile, r.Inputs["token"], 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ChatIDFile, r.Inputs["chat_id"], 0600); err != nil {
		t.Fatal(err)
	}
	input, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	for name, route := range input.Worker.Routes {
		route.Revision = "legacy-manual-revision"
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
	root, err := ensureConfigDirectory(paths.Installation)
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	installed := filepath.Join(paths.Installation, "manual-runtime.json")
	if err = publishPrivateInput(installed, raw); err != nil {
		t.Fatal(err)
	}
	r.Inputs = nil
	r.Action = "adopt"
	r.DesiredScope = 1
	r.DesiredHash, _ = r.Desired.NotificationFingerprint()
	return r, paths, &install.PolicyReview{Input: input, Runtime: raw, Manifest: install.ManagedManifest{Version: 1, Identity: protocol.Identity{Worker: "retained-gateway", Recovery: "retained-recovery"}, Root: paths.Installation, UnitDirectory: paths.UnitDirectory, Mode: "verification", BinaryHash: r.HelperSHA256, RuntimeHash: inputDigest(raw), ConfigHash: protocol.Hash(c), Paths: install.ServicePaths{Config: installed, State: paths.State, Socket: paths.Socket, DeniedPaths: paths.DeniedPaths}}}
}

func TestNotificationAdoptionLostRepliesAndNoop(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, boundary := range []string{"adoption-staged", "adoption-ready", "adoption-published", "adoption-runtime", "ran/notification-adopt", "recorded/notification-adopt", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			r, paths, review := adoptionFixture(t)
			stop := errors.New("lost adoption reply")
			l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }, boundary: func(at string) error {
				if at == boundary {
					return stop
				}
				return nil
			}}
			if _, handled, err := reconcileNotificationAdoption(context.Background(), r, paths, l); !handled || !errors.Is(err, stop) {
				t.Fatal("boundary", handled, err)
			}
			if boundary == "adoption-staged" || boundary == "adoption-ready" {
				if _, err := os.Stat(paths.Control); !os.IsNotExist(err) {
					t.Fatal("published partial coordinator", err)
				}
				if _, err := os.Stat(paths.Runtime); !os.IsNotExist(err) {
					t.Fatal("published runtime before intent", err)
				}
			}
			r.Action = "resume"
			l.boundary = nil
			for range 2 {
				got, handled, err := reconcileNotificationAdoption(context.Background(), r, paths, l)
				if err != nil || !handled || got.Code != "ok" || got.Identity == nil || *got.Identity != review.Manifest.Identity {
					t.Fatal("resume", got, handled, err)
				}
			}
			raw, err := os.ReadFile(paths.Runtime)
			if err != nil || string(raw) != string(review.Runtime) {
				t.Fatal("rewrote legacy bytes", err)
			}
			original, err := os.ReadFile(review.Manifest.Paths.Config)
			if err != nil || string(original) != string(raw) {
				t.Fatal("changed installed input", err)
			}
			inspect := func(context.Context, string) (install.InstallationObservation, error) {
				return install.InstallationObservation{Manifest: review.Manifest, Phase: "complete", Unbound: true}, nil
			}
			if got, err := inspectNotificationControl(context.Background(), r, paths, inspect, nil); err != nil || got.State != "waiting_for_producers" {
				t.Fatal("status", got, err)
			}
			if got, err := planNotificationControl(context.Background(), r, paths, inspect); err != nil || got.State != "up_to_date" {
				t.Fatal("plan", got, err)
			}
			secrets := map[string][]byte{}
			if err = readDeclaredTelegramInputs(r, paths, secrets); err != nil {
				t.Fatal(err)
			}
			input, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, secrets)
			if err != nil {
				t.Fatal(err)
			}
			step := notificationBootstrapStep{paths: paths, input: input, secrets: secrets}
			if err = step.verifyFiles(); err != nil {
				t.Fatal("following enrollment input", err)
			}
			r.Desired.Notification.Limits.MaxMessages--
			after, err := compileLimitsRuntime(r, paths, review.Input)
			if err != nil {
				t.Fatal(err)
			}
			var next install.RuntimeInput
			if err = safefs.DecodeJSON(after, &next, maxControlFrame); err != nil {
				t.Fatal(err)
			}
			for name, route := range review.Input.Worker.Routes {
				if next.Worker.Routes[name].Revision != route.Revision {
					t.Fatal("lost imported route label")
				}
			}
		})
	}
}

func TestNotificationAdoptionConcurrentPublicationAndStagedDrift(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	r, paths, review := adoptionFixture(t)
	ready, release := make(chan struct{}), make(chan struct{})
	l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }, boundary: func(at string) error {
		if at == "adoption-ready" {
			close(ready)
			<-release
		}
		return nil
	}}
	done := make(chan error, 1)
	go func() { _, _, err := reconcileNotificationAdoption(context.Background(), r, paths, l); done <- err }()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("adoption did not stage")
	}
	l.boundary = nil
	_, _, err := reconcileNotificationAdoption(context.Background(), r, paths, l)
	close(release)
	if err == nil {
		t.Fatal("concurrent adoption bypassed staging lease")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if got, _, err := reconcileNotificationAdoption(context.Background(), r, paths, l); err != nil || got.Code != "ok" {
		t.Fatal(got, err)
	}

	r, paths, review = adoptionFixture(t)
	l.boundary = func(at string) error {
		if at == "adoption-ready" {
			return errors.New("stop")
		}
		return nil
	}
	if _, _, err = reconcileNotificationAdoption(context.Background(), r, paths, l); err == nil {
		t.Fatal("boundary missing")
	}
	l.boundary = nil
	staged := filepath.Join(filepath.Dir(paths.Control), notificationAdoptionStage)
	if err = os.Remove(filepath.Join(staged, "adoption.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = reconcileNotificationAdoption(context.Background(), r, paths, l); err == nil {
		t.Fatal("recreated missing staged intent")
	}
	if _, err = os.Stat(filepath.Join(staged, "adoption.json")); !os.IsNotExist(err) {
		t.Fatal("lost authority was rewritten")
	}
}

func TestNotificationAdoptionRefusesDriftAndLostAuthority(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, damage := range []string{"policy", "helper", "credentials", "journal", "input-pins", "intent", "runtime", "installed", "pending-policy", "managed-led", "bound"} {
		t.Run(damage, func(t *testing.T) {
			r, paths, review := adoptionFixture(t)
			l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }}
			if damage == "bound" {
				review.Input.Bootstrap = false
			} else if damage == "managed-led" {
				r.Desired.Notification.StatusLEDs.Enabled = true
			} else {
				if damage == "pending-policy" {
					l.boundary = func(at string) error {
						if at == "adoption-published" {
							return errors.New("stop")
						}
						return nil
					}
				}
				_, _, err := reconcileNotificationAdoption(context.Background(), r, paths, l)
				if damage != "pending-policy" && err != nil {
					t.Fatal(err)
				}
				l.boundary = nil
			}
			switch damage {
			case "policy", "pending-policy":
				r.Desired.Notification.Limits.MaxMessages--
				r.DesiredHash, _ = r.Desired.NotificationFingerprint()
			case "helper":
				r.HelperSHA256 = strings.Repeat("b", 64)
			case "credentials":
				if err := os.WriteFile(r.Desired.Notification.Providers["telegram"].TokenFile, []byte("123:rotated"), 0600); err != nil {
					t.Fatal(err)
				}
			case "journal", "input-pins", "intent", "runtime":
				file := map[string]string{"journal": paths.Control + "/operation.json", "input-pins": paths.Control + "/notification-inputs.json", "intent": paths.Control + "/adoption.json", "runtime": paths.Runtime}[damage]
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			case "installed":
				review.Manifest.Revision = "changed"
			}
			if _, _, err := reconcileNotificationAdoption(context.Background(), r, paths, l); err == nil {
				t.Fatal("accepted damaged adoption")
			}
			if damage == "journal" {
				if _, err := os.Stat(paths.Control + "/operation.json"); !os.IsNotExist(err) {
					t.Fatal("recreated journal")
				}
			}
		})
	}
}

func TestNotificationAdoptionProcessDeath(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	if file := os.Getenv("DEPLOY_ADOPTION_CRASH_INPUT"); file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var fixture struct {
			Request ControlRequest
			Review  install.PolicyReview
		}
		if err = json.Unmarshal(raw, &fixture); err != nil {
			t.Fatal(err)
		}
		paths, err := ResolvePaths(fixture.Request.Worker, fixture.Request.Desired.Notification.Paths, fixture.Request.Facts)
		if err != nil {
			t.Fatal(err)
		}
		l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return fixture.Review, nil }, boundary: func(at string) error {
			if at == os.Getenv("DEPLOY_ADOPTION_CRASH_AT") {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
			return nil
		}}
		_, _, err = reconcileNotificationAdoption(context.Background(), fixture.Request, paths, l)
		t.Fatal("crash boundary not reached", err)
	}
	for _, at := range []string{"adoption-staged", "adoption-ready", "adoption-published", "adoption-runtime", "recorded/notification-adopt", "complete"} {
		t.Run(at, func(t *testing.T) {
			r, paths, review := adoptionFixture(t)
			raw, _ := json.Marshal(struct {
				Request ControlRequest
				Review  install.PolicyReview
			}{r, *review})
			file := filepath.Join(r.Facts.Home, "crash.json")
			if err := os.WriteFile(file, raw, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNotificationAdoptionProcessDeath$")
			cmd.Env = append(os.Environ(), "DEPLOY_ADOPTION_CRASH_INPUT="+file, "DEPLOY_ADOPTION_CRASH_AT="+at)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not killed at boundary: %v %s", err, out)
			}
			l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }}
			for range 2 {
				if got, handled, err := reconcileNotificationAdoption(context.Background(), r, paths, l); err != nil || !handled || got.Code != "ok" {
					t.Fatal(got, handled, err)
				}
			}
		})
	}
}

func TestRouteLabelsPreserveOnlyUnchangedSelections(t *testing.T) {
	r, paths, review := adoptionFixture(t)
	r.Desired.Notification.Routes["audit"] = config.NotificationRoute{Primary: "telegram"}
	raw, err := compileLimitsRuntime(r, paths, review.Input)
	if err != nil {
		t.Fatal(err)
	}
	var input install.RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, maxControlFrame); err != nil {
		t.Fatal(err)
	}
	if input.Worker.Routes["audit"].Revision != "declaration-v3" {
		t.Fatal("new route inherited legacy revision")
	}
	for name, route := range input.Worker.Routes {
		if name == "audit" {
			continue
		}
		route.Primary = "signal"
		route.Revision = "new-reviewed-label"
		input.Worker.Routes[name] = route
	}
	preserveRuntimeRouteLabels(review.Input, &input)
	// A changed selection keeps the caller's new label; preserving history must
	// never copy provider routing or other desired fields from the old input.
	for name, route := range input.Worker.Routes {
		if name != "audit" && (route.Primary != "signal" || route.Revision != "new-reviewed-label") {
			t.Fatal("retained policy overrode desired")
		}
	}
}

func TestNotificationAdoptionRetainsExistingLEDDisposition(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "disabled"}[disabled], func(t *testing.T) {
			r, paths, review := adoptionFixture(t)
			r.Desired.Notification.StatusLEDs = config.LEDDeclaration{Enabled: !disabled, Socket: "/run/pi-status-led.sock", Daemon: config.LEDDaemon{Mode: "existing"}}
			r.DesiredHash, _ = r.Desired.NotificationFingerprint()
			review.Input.LEDSocket = "/run/pi-status-led.sock"
			review.Input.LEDEndpoint = &led.Endpoint{Version: 1, Socket: review.Input.LEDSocket, OwnerUID: 0, PeerUID: 995}
			op, _ := safefs.NewID("n")
			review.Manifest.LEDOutput = &install.ManagedLEDOutput{Operation: op, Disabled: disabled}
			review.Runtime, _ = review.Input.Bytes()
			c, err := review.Input.Resolve()
			if err != nil {
				t.Fatal(err)
			}
			review.Manifest.RuntimeHash, review.Manifest.ConfigHash = inputDigest(review.Runtime), protocol.Hash(c)
			l := notificationAdoptionLifecycle{review: func(context.Context, string) (install.PolicyReview, error) { return *review, nil }}
			got, _, err := reconcileNotificationAdoption(context.Background(), r, paths, l)
			if err != nil || got.LEDOutput == nil || *got.LEDOutput != *review.Manifest.LEDOutput {
				t.Fatal("changed output disposition", got, err)
			}
			if err = os.Remove(filepath.Join(paths.Control, "adoption.json")); err != nil {
				t.Fatal(err)
			}
			calls := 0
			if _, err = testNotificationControl(context.Background(), r, paths, func(context.Context, string, string) (install.DiagnosticResult, error) {
				calls++
				return install.DiagnosticResult{}, nil
			}); err == nil || calls != 0 {
				t.Fatal("lost adoption authority allowed send", err)
			}
		})
	}
}
