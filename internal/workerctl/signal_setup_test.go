package workerctl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func signalSetupFixture(t *testing.T) (*signalSetupStep, string, *int) {
	t.Helper()
	base, err := os.MkdirTemp("", "sgctl-")
	testutil.Must(t, err)
	t.Cleanup(func() { os.RemoveAll(base) })
	source := filepath.Join(base, "main.go")
	testutil.Must(t, os.WriteFile(source, []byte(`package main
import("fmt";"os";"slices";"path/filepath")
func main(){
 if slices.Contains(os.Args,"--version"){fmt.Println("signal-cli 0.14.9");return}
 if slices.Contains(os.Args,"listAccounts"){fmt.Print(`+"`"+`[{"number":"+10000000000","aci":"11111111-1111-4111-8111-111111111111"}]`+"`"+`);return}
 if !slices.Contains(os.Args,"link"){os.Exit(7)}
 f,e:=os.OpenFile(filepath.Join(os.Getenv("HOME"),"fixture-link-count"),os.O_CREATE|os.O_WRONLY|os.O_APPEND,0600);if e!=nil{os.Exit(8)};f.Write([]byte("1"));f.Close();fmt.Println("private-synthetic-link-uri")
 if _,e:=os.Stat(filepath.Join(os.Getenv("HOME"),"fixture-fail"));e==nil{os.Exit(9)}
}
`), 0600))
	binary := filepath.Join(base, "input-cli")
	cmd := exec.Command("go", "build", "-o", binary, source)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(string(out), err)
	}
	testutil.Must(t, os.Chmod(binary, 0700))
	raw, err := os.ReadFile(binary)
	testutil.Must(t, err)
	paths := ResolvedPaths{Config: filepath.Join(base, "config"), Data: filepath.Join(base, "data"), State: filepath.Join(base, "state", "gateway"), Installation: filepath.Join(base, "data", "gateway"), Control: filepath.Join(base, "control"), Runtime: filepath.Join(base, "config", "runtime.json"), Credentials: filepath.Join(base, "credentials"), UnitDirectory: filepath.Join(base, "units")}
	provider := config.ProviderDeclaration{AccountInput: "/operator/account", RecipientInput: "/operator/recipient"}
	resolved, err := ResolveSignalPaths(provider, paths, base, inputDigest(raw))
	testutil.Must(t, err)
	d, err := ensureConfigDirectory(resolved.Control)
	testutil.Must(t, err)
	t.Cleanup(func() { d.Close() })
	count := 0
	var installed *install.FreshSignalManifest
	lifecycle := signalSetupLifecycle{ensure: func(_ context.Context, s install.FreshSignalSpec) (install.FreshSignalManifest, error) {
		count++
		m := install.FreshSignalManifest{Spec: s, ManifestHash: inputDigest([]byte("synthetic manifest"))}
		installed = &m
		return m, nil
	}, inspect: func(_ context.Context, s install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
		if installed == nil {
			return install.FreshSignalManifest{}, false, nil
		}
		return *installed, true, nil
	}}
	r := ControlRequest{Version: 1, Worker: "notification-worker", Server: "manager", DesiredHash: inputDigest([]byte("synthetic desired")), HelperSHA256: inputDigest(raw), SignalSHA256: inputDigest(raw), Facts: TargetFacts{OS: "linux", Arch: runtime.GOARCH, UID: os.Getuid(), User: "fixture", Home: base}}
	return &signalSetupStep{request: r, gateway: paths, paths: resolved, directory: d, provider: provider, helperSource: binary, nativeSource: binary, account: []byte("+10000000000"), recipient: []byte("22222222-2222-4222-8222-222222222222"), lifecycle: lifecycle}, testutil.ID(t, "n"), &count
}
func TestSignalSetupNeedsPrivateDisplayAndResumesSameLink(t *testing.T) {
	s, op, starts := signalSetupFixture(t)
	ctx := context.Background()
	if _, done, err := s.Inspect(ctx, op); err != nil || done {
		t.Fatal(done, err)
	}
	if err := s.Run(ctx, op); !errors.Is(err, errSignalLinkRequired) {
		t.Fatal("missing private display was not deferred", err)
	}
	status, err := signalaccount.Inspect(s.paths.Account)
	testutil.Must(t, err)
	if status.State != "none" || *starts != 0 {
		t.Fatal("unattended bootstrap launched linking")
	}
	var display bytes.Buffer
	s.display = &display
	testutil.Must(t, s.Run(ctx, op))
	_, done, err := s.Inspect(ctx, op)
	testutil.Must(t, err)
	if !done || display.String() != "private-synthetic-link-uri\n" || *starts != 1 {
		t.Fatal("guided setup incomplete", done, display.Len(), *starts)
	}
	raw, err := os.ReadFile(filepath.Join(s.paths.Account, "fixture-link-count"))
	testutil.Must(t, err)
	if string(raw) != "1" {
		t.Fatal("link count differs")
	}
	retained, err := os.ReadFile(filepath.Join(s.paths.Control, "service.json"))
	testutil.Must(t, err)
	if bytes.Contains(retained, bytes.TrimSpace(display.Bytes())) {
		t.Fatal("linking URI persisted in setup")
	}
	s.display = nil
	_, done, err = s.Inspect(ctx, op)
	testutil.Must(t, err)
	if !done || *starts != 1 {
		t.Fatal("inspection restarted service")
	}
	s.account = []byte("changed")
	if _, _, err = s.Inspect(ctx, op); err == nil {
		t.Fatal("credential change adopted")
	}
}
func TestSignalSetupUnknownLinkDoesNotReplay(t *testing.T) {
	s, op, starts := signalSetupFixture(t)
	ctx := context.Background()
	if err := s.Run(ctx, op); !errors.Is(err, errSignalLinkRequired) {
		t.Fatal(err)
	}
	testutil.Must(t, os.WriteFile(filepath.Join(s.paths.Account, "fixture-fail"), nil, 0600))
	s.display = io.Discard
	if err := s.Run(ctx, op); err == nil {
		t.Fatal("uncertain link succeeded")
	}
	if err := s.Run(ctx, op); err == nil {
		t.Fatal("uncertain link replayed")
	}
	raw, err := os.ReadFile(filepath.Join(s.paths.Account, "fixture-link-count"))
	testutil.Must(t, err)
	if string(raw) != "1" {
		t.Fatal("unknown outcome linked twice")
	}
	s.request.SignalReview = testutil.ID(t, "n")
	if err := s.Run(ctx, op); err == nil {
		t.Fatal("review retargeted another operation")
	}
	s.request.SignalReview = op
	s.display = nil
	testutil.Must(t, s.Run(ctx, op))
	if *starts != 1 {
		t.Fatal("reviewed retained account not inspected/installed")
	}
	raw, err = os.ReadFile(filepath.Join(s.paths.Account, "fixture-link-count"))
	testutil.Must(t, err)
	if string(raw) != "1" {
		t.Fatal("review launched another link")
	}
	// Lost completion responses preserve the review and service receipt.
	testutil.Must(t, s.Run(ctx, op))
	status, err := signalaccount.Inspect(s.paths.Account)
	testutil.Must(t, err)
	if status.State != "reviewed" {
		t.Fatal("retained review disappeared", status.State)
	}
}
func TestSignalSetupLostInputPinsDoNotReinitialize(t *testing.T) {
	s, op, _ := signalSetupFixture(t)
	if err := s.Run(context.Background(), op); !errors.Is(err, errSignalLinkRequired) {
		t.Fatal(err)
	}
	testutil.Must(t, os.Remove(filepath.Join(s.paths.Control, "inputs.json")))
	if err := s.Run(context.Background(), op); err == nil {
		t.Fatal("lost input authority recreated")
	}
	// The original canonical account reservation remains intact.
	var files []string
	err := filepath.Walk(s.paths.Account, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files = append(files, path)
		}
		return err
	})
	testutil.Must(t, err)
	if len(files) == 0 {
		t.Fatal("reserved account disappeared")
	}
}

func TestDeclaredSignalCoordinatorAndGatewayRetainReceipts(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	step, _, starts := signalSetupFixture(t)
	// Exercise a genuinely empty target, including absent parent control paths.
	// The lower-level fixture normally creates its journal directory for Run tests.
	testutil.Must(t, step.directory.Close())
	testutil.Must(t, os.Remove(step.paths.Control))
	testutil.Must(t, os.Remove(step.gateway.Control))
	r, _ := bootstrapControlFixture(t)
	r.Desired.Backup = nil
	r.Desired.Notification.Providers = config.ProviderDeclarations{"signal": step.provider}
	r.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "signal"}}
	r.DesiredHash, _ = r.Desired.Fingerprint()
	r.Facts = step.request.Facts
	r.HelperSHA256 = step.request.HelperSHA256
	r.SignalSHA256 = step.request.SignalSHA256
	r.Inputs = map[string][]byte{"signal_account": step.account, "signal_recipient": step.recipient}
	ctx := context.Background()
	_, _, _, err := prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle)
	if !errors.Is(err, errSignalLinkRequired) {
		t.Fatal("missing display did not defer linking", err)
	}
	status, found, err := inspectDeclaredSignalProgress(ctx, r, step.gateway)
	testutil.Must(t, err)
	if !found || status.Code != "signal_link_required" {
		t.Fatal("pending Signal setup hidden", status, found)
	}
	if _, err = os.Stat(filepath.Join(step.gateway.Control, "operation.json")); !os.IsNotExist(err) {
		t.Fatal("status created parent gateway authority", err)
	}
	r.InteractiveLink = true
	var display bytes.Buffer
	service, _, secrets, err := prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, &display, step.lifecycle)
	testutil.Must(t, err)
	if *starts != 1 {
		t.Fatal("service not installed once")
	}
	r.InteractiveLink = false
	repeated, _, _, err := prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle)
	testutil.Must(t, err)
	if repeated.ManifestHash != service.ManifestHash || *starts != 1 {
		t.Fatal("completed retry replayed service")
	}
	var gateway *install.ManagedManifest
	gatewayStarts := 0
	lifecycle := notificationLifecycle{ensure: func(_ context.Context, s install.ManagedSpec) (install.ManagedManifest, error) {
		gatewayStarts++
		d, err := safefs.Open(s.State)
		testutil.Must(t, err)
		defer d.Close()
		var reservation worker.GatewayBootstrap
		testutil.Must(t, d.ReadJSON("bootstrap.json", &reservation))
		testutil.Must(t, os.Mkdir(s.Root, 0700))
		m := install.ManagedManifest{Root: s.Root, Identity: reservation.Identity, Mode: "verification", FreshSignal: s.FreshSignal}
		gateway = &m
		return m, nil
	}, inspect: func(_ context.Context, s install.ManagedSpec) (install.ManagedManifest, bool, error) {
		if gateway == nil {
			return install.ManagedManifest{}, false, nil
		}
		return *gateway, true, nil
	}}
	result, err := applyNotificationBootstrapWithSignal(ctx, r, step.gateway, step.helperSource, lifecycle, &service, secrets)
	testutil.Must(t, err)
	if result.State != "waiting_for_producers" || gateway.FreshSignal == nil || gateway.FreshSignal.ManifestHash != service.ManifestHash {
		t.Fatal("gateway lost its Signal service")
	}
	_, err = applyNotificationBootstrapWithSignal(ctx, r, step.gateway, step.helperSource, lifecycle, &service, secrets)
	testutil.Must(t, err)
	if gatewayStarts != 1 {
		t.Fatal("unchanged gateway restarted")
	}
	step.lifecycle.gateway = func(context.Context, string) (install.InstallationObservation, error) {
		return install.InstallationObservation{Manifest: *gateway, Phase: "complete", Unbound: true}, nil
	}
	updates := 0
	release := notificationReleaseLifecycle{inspect: step.lifecycle.gateway, update: func(_ context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
		if expected != protocol.Hash(*gateway) {
			t.Fatal("release retargeted gateway")
		}
		updates++
		gateway.BinaryHash = spec.BinarySHA256
		return *gateway, nil
	}}
	childJournal, err := os.ReadFile(filepath.Join(step.paths.Control, "operation.json"))
	testutil.Must(t, err)
	childInputs, err := os.ReadFile(filepath.Join(step.paths.Control, "inputs.json"))
	testutil.Must(t, err)
	for _, change := range []string{"scope", "helper"} {
		t.Run(change, func(t *testing.T) {
			if change == "scope" {
				r.DesiredScope = 1
			} else {
				r.HelperSHA256 = inputDigest([]byte("new gateway helper"))
			}
			r.DesiredHash, err = declarationFingerprint(r.Desired, r.DesiredScope)
			testutil.Must(t, err)
			_, handled, err := reconcileNotificationRelease(ctx, r, step.gateway, step.helperSource, release)
			testutil.Must(t, err)
			if !handled {
				t.Fatal("release not reconciled")
			}
			originalInspect, originalResume := step.lifecycle.inspect, step.lifecycle.resume
			stopped, resumes := true, 0
			step.lifecycle.inspect = func(ctx context.Context, s install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
				m, complete, e := originalInspect(ctx, s)
				if e == nil && complete && stopped {
					return m, false, install.FreshSignalResumeRequired{Reference: install.FreshSignalReference{Root: m.Spec.Root, ManifestHash: m.ManifestHash}}
				}
				return m, complete, e
			}
			step.lifecycle.resume = func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
				if ref.Root != service.Spec.Root || ref.ManifestHash != service.ManifestHash {
					t.Fatal("scope reuse restarted another service")
				}
				resumes++
				stopped = false
				return service, nil
			}
			action := r.Action
			r.Action = "resume"
			testutil.Must(t, resumeCompletedSignalChild(ctx, r, step.gateway, step.helperSource, step.nativeSource, step.lifecycle))
			testutil.Must(t, resumeCompletedSignalChild(ctx, r, step.gateway, step.helperSource, step.nativeSource, step.lifecycle))
			if resumes != 1 || *starts != 1 || gatewayStarts != 1 {
				t.Fatal("explicit child restart replayed setup")
			}
			r.Action = action
			step.lifecycle.inspect, step.lifecycle.resume = originalInspect, originalResume
			for range 2 {
				reused, _, _, err := prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle)
				testutil.Must(t, err)
				if protocol.Hash(reused) != protocol.Hash(service) || *starts != 1 || gatewayStarts != 1 {
					t.Fatal("gateway release changed/restarted native Signal service")
				}
			}
		})
	}
	if updates != 2 {
		t.Fatal("gateway updates repeated", updates)
	}
	for name, want := range map[string][]byte{"operation.json": childJournal, "inputs.json": childInputs} {
		got, err := os.ReadFile(filepath.Join(step.paths.Control, name))
		testutil.Must(t, err)
		if !bytes.Equal(got, want) {
			t.Fatal("Signal setup history was rewritten", name)
		}
	}
	oldNative := r.SignalSHA256
	r.SignalSHA256 = inputDigest([]byte("changed native archive"))
	if _, _, _, err = prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle); err == nil {
		t.Fatal("native replacement silently reused old service")
	}
	if _, _, err = reconcileNotificationRelease(ctx, r, step.gateway, step.helperSource, release); err == nil {
		t.Fatal("native replacement ignored by gateway release")
	}
	r.SignalSHA256 = oldNative
	oldReference := *gateway.FreshSignal
	gateway.FreshSignal.ManifestHash = inputDigest([]byte("changed canonical service"))
	if _, _, _, err = prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle); err == nil {
		t.Fatal("changed canonical gateway adopted")
	}
	*gateway.FreshSignal = oldReference
	childPath := filepath.Join(step.paths.Control, "inputs.json")
	testutil.Must(t, os.Remove(childPath))
	if _, _, _, err = prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle); err == nil {
		t.Fatal("missing native setup authority ignored")
	}
	testutil.Must(t, os.WriteFile(childPath, childInputs, 0600))
	parent, err := safefs.Open(step.gateway.Control)
	testutil.Must(t, err)
	defer parent.Close()
	var completed OperationJournal
	testutil.Must(t, parent.ReadJSON("operation.json", &completed))
	pendingPins := completed.Pins
	pendingPins.Previous, pendingPins.PreviousOperation = completed.Digest(), completed.ID
	pending, err := (JournalCoordinator{Directory: parent, Boundary: func(name string) error {
		if name == "intent" {
			return errors.New("interrupted before enrollment")
		}
		return nil
	}}).Apply(ctx, pendingPins, []LifecycleStep{&journalStep{name: "notification-enrollment"}})
	if err == nil || pending.Complete {
		t.Fatal("enrollment interruption missing")
	}
	_, _, _, err = prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle)
	testutil.Must(t, err)
	var stillPending OperationJournal
	testutil.Must(t, parent.ReadJSON("operation.json", &stillPending))
	if stillPending.Digest() != pending.Digest() || *starts != 1 {
		t.Fatal("Signal reuse completed or replayed pending gateway enrollment")
	}
	priorFile := filepath.Join(step.gateway.Control, "history", completed.ID+".json")
	priorBytes, err := os.ReadFile(priorFile)
	testutil.Must(t, err)
	testutil.Must(t, os.Remove(priorFile))
	if _, _, _, err = prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle); err == nil {
		t.Fatal("pending enrollment reused Signal without predecessor authority")
	}
	testutil.Must(t, os.WriteFile(priorFile, priorBytes, 0600))
	// Pending mapping and subsequent delivery also reuse only the independently
	// completed Signal child; they must not relink or complete gateway work.
	for _, name := range []string{"notification-mapping", "notification-delivery"} {
		pending.Complete, pending.Children[0].Complete, pending.Children[0].Receipt = true, true, protocol.Hash(*gateway)
		testutil.Must(t, parent.WriteJSON("operation.json", pending, true))
		pins := pending.Pins
		pins.Previous, pins.PreviousOperation = pending.Digest(), pending.ID
		pending, err = (JournalCoordinator{Directory: parent, Boundary: func(at string) error {
			if at == "intent" {
				return errors.New("interrupted successor")
			}
			return nil
		}}).Apply(ctx, pins, []LifecycleStep{&journalStep{name: name}})
		if err == nil || pending.Complete {
			t.Fatal("successor interruption missing", name)
		}
		_, _, _, err = prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle)
		testutil.Must(t, err)
		stillPending = OperationJournal{}
		testutil.Must(t, parent.ReadJSON("operation.json", &stillPending))
		if stillPending.Digest() != pending.Digest() || *starts != 1 {
			t.Fatal("Signal reuse mutated pending gateway successor", name)
		}
	}
	testutil.Must(t, parent.WriteJSON("operation.json", completed, true))
	r.Inputs["signal_account"] = []byte("changed")
	if _, _, _, err = prepareDeclaredSignal(ctx, r, step.gateway, step.helperSource, step.nativeSource, nil, step.lifecycle); err == nil {
		t.Fatal("credential drift adopted")
	}
	if *starts != 1 || gatewayStarts != 1 {
		t.Fatal("credential drift restarted services")
	}
}

func TestDeclaredSignalPreflightsCombinedProviderBeforeLinking(t *testing.T) {
	step, _, starts := signalSetupFixture(t)
	r, _ := bootstrapControlFixture(t)
	r.Desired.Backup = nil
	r.Desired.Notification.Providers["signal"] = step.provider
	r.DesiredHash, _ = r.Desired.Fingerprint()
	r.Facts = step.request.Facts
	r.HelperSHA256, r.SignalSHA256 = step.request.HelperSHA256, step.request.SignalSHA256
	r.Inputs["signal_account"], r.Inputs["signal_recipient"] = step.account, step.recipient
	r.Inputs["token"] = []byte("invalid-token")
	r.InteractiveLink = true
	var display bytes.Buffer
	if _, _, _, err := prepareDeclaredSignal(context.Background(), r, step.gateway, step.helperSource, step.nativeSource, &display, step.lifecycle); err == nil {
		t.Fatal("invalid combined credentials accepted")
	}
	if display.Len() != 0 || *starts != 0 {
		t.Fatal("invalid credentials started linking")
	}
	if _, err := os.Stat(step.paths.Account); !os.IsNotExist(err) {
		t.Fatal("invalid credentials reserved account", err)
	}
}
