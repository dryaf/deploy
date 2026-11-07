package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

type mappingTestLED struct {
	states    map[string]led.State
	mutations int
}

func (a *mappingTestLED) List(context.Context) ([]led.Entry, error) {
	var entries []led.Entry
	for _, s := range a.states {
		entries = append(entries, led.Entry{State: s, OwnerUID: os.Getuid()})
	}
	return entries, nil
}
func (a *mappingTestLED) Status(context.Context) (led.DaemonStatus, error) {
	s := led.DaemonStatus{LEDs: map[string]json.RawMessage{"green": json.RawMessage(`{"enabled":true,"available":true,"default_pattern":"off"}`)}, Patterns: map[string]json.RawMessage{}}
	for _, p := range []string{"blink-slow", "blink-fast", "heartbeat", "blink-1", "blink-2"} {
		s.Patterns[p] = json.RawMessage(`[]`)
	}
	return s, nil
}
func (a *mappingTestLED) Set(_ context.Context, s led.State) error {
	a.mutations++
	a.states[s.ID] = s
	return nil
}
func (a *mappingTestLED) Clear(_ context.Context, id string) error {
	a.mutations++
	delete(a.states, id)
	return nil
}

type mappingFixture struct {
	r           ControlRequest
	paths       ResolvedPaths
	d           *safefs.Directory
	input       install.RuntimeInput
	reservation backup.ProducerReservation
	o           install.InstallationObservation
	api         *mappingTestLED
	calls       int
	native      *signalSetupStep
	service     *install.FreshSignalManifest
}

func newMappingFixture(t *testing.T, withOutput ...bool) *mappingFixture {
	return newMappingFixtureWithRoutes(t, false, withOutput...)
}
func newMappingFixtureWithRoutes(t *testing.T, extra bool, withOutput ...bool) *mappingFixture {
	return newMappingFixtureWithProfiles(t, extra, false, withOutput...)
}
func newMappingFixtureWithProfiles(t *testing.T, extra, signalPrimary bool, withOutput ...bool) *mappingFixture {
	return newMappingFixtureWithOptions(t, mappingFixtureOptions{extraRoutes: extra, signalPrimary: signalPrimary}, withOutput...)
}

type mappingFixtureOptions struct {
	extraRoutes, signalPrimary, signalOnly, unbound bool
	configure                                       func(*ControlRequest)
}

func newMappingFixtureWithOptions(t *testing.T, options mappingFixtureOptions, withOutput ...bool) *mappingFixture {
	extra, signalPrimary := options.extraRoutes, options.signalPrimary
	t.Helper()
	r, paths, endpoint, _ := declaredLEDFixture(t)
	if extra {
		r.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "telegram"}, "audit": {Primary: "telegram"}}
	}
	if signalPrimary {
		r.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "signal"}}
	}
	r.Desired.Notification.StatusLEDs.Bindings = map[string]config.LEDBindingDeclaration{"backup": {Producer: "backup-worker", LED: "green", Subjects: []string{"*"}}}
	if extra && len(withOutput) > 5 && withOutput[5] {
		r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {Routes: []string{"alerts", "audit"}, MaxMessages: 1000, MaxSilenceSeconds: 120, MaxEvidenceSeconds: 93600, PolicyHealth: true}}
	}
	if len(withOutput) > 3 && withOutput[3] {
		r.Desired.Notification.StatusLEDs.Bindings = nil
	}
	if len(withOutput) > 4 && withOutput[4] {
		r.Desired.Notification.StatusLEDs = config.LEDDeclaration{}
	}
	if len(withOutput) > 1 && withOutput[1] {
		r.DesiredScope = 1
	}
	var native *signalSetupStep
	var service *install.FreshSignalManifest
	var secrets map[string][]byte
	if len(withOutput) > 2 && withOutput[2] {
		native, _, _ = signalSetupFixture(t)
		r.Desired.Notification.Providers["signal"] = native.provider
		r.Facts.Arch = native.request.Facts.Arch
		r.SignalSHA256 = native.request.SignalSHA256
		r.HelperSHA256 = native.request.HelperSHA256
		if r.Inputs == nil {
			r.Inputs = map[string][]byte{}
		}
		r.Inputs["signal_account"], r.Inputs["signal_recipient"] = native.account, native.recipient
	}
	if options.signalOnly {
		delete(r.Desired.Notification.Providers, "telegram")
		delete(r.Inputs, "token")
		delete(r.Inputs, "chat_id")
	}
	if options.configure != nil {
		options.configure(&r)
	}
	r.DesiredHash, _ = declarationFingerprint(r.Desired, r.DesiredScope)
	r.ledEndpoint = &endpoint
	if len(withOutput) > 4 && withOutput[4] {
		r.ledEndpoint = nil
	}
	if native != nil {
		r.InteractiveLink = true
		var display bytes.Buffer
		m, _, inputs, err := prepareDeclaredSignal(context.Background(), r, paths, native.helperSource, native.nativeSource, &display, native.lifecycle)
		if err != nil {
			t.Fatal(err)
		}
		r.InteractiveLink = false
		service, secrets = &m, inputs
	}
	f := &mappingFixture{r: r, paths: paths, api: &mappingTestLED{states: map[string]led.State{}}, native: native, service: service}
	ctx := context.Background()
	bootstrap := notificationLifecycle{
		inspect: func(context.Context, install.ManagedSpec) (install.ManagedManifest, bool, error) {
			return f.o.Manifest, f.o.Phase == "complete", nil
		},
		ensure: func(_ context.Context, spec install.ManagedSpec) (install.ManagedManifest, error) {
			d, err := safefs.Open(spec.State)
			if err != nil {
				t.Fatal(err)
			}
			var b worker.GatewayBootstrap
			err = d.ReadJSON("bootstrap.json", &b)
			d.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Mkdir(spec.Root, 0700); err != nil {
				t.Fatal(err)
			}
			raw, err := readLimitsRuntime(spec.RuntimeFile)
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
			revision, _ := safefs.NewID("n")
			f.o = install.InstallationObservation{Phase: "complete", Unbound: true, LEDs: true, Manifest: install.ManagedManifest{Version: 1, Root: spec.Root, Installation: revision, Revision: revision, Identity: b.Identity, Mode: "verification", Paths: install.ServicePaths{State: spec.State}, UnitDirectory: spec.UnitDirectory, FreshSignal: spec.FreshSignal, BinaryHash: spec.BinarySHA256, HelperHash: spec.BinarySHA256, RuntimeHash: inputDigest(raw), ConfigHash: protocol.Hash(c)}}
			return f.o.Manifest, nil
		},
	}
	result, err := applyNotificationBootstrapWithSignal(ctx, r, paths, "/helper", bootstrap, service, secrets)
	if err != nil || len(r.Desired.Notification.StatusLEDs.Bindings) > 0 && !strings.Contains(result.Next, "bindings wait") {
		t.Fatal("bootstrap did not explain deferred mappings", result, err)
	}
	f.d, err = safefs.Open(paths.Control)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.d.Close() })
	raw, err := readLimitsRuntime(paths.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err = safefs.DecodeJSON(raw, &f.input, maxControlFrame); err != nil {
		t.Fatal(err)
	}
	if len(withOutput) > 0 && withOutput[0] {
		l := notificationLEDLifecycle{inspect: f.inspect}
		l.set = func(ctx context.Context, root, expected string, disabled bool) (install.ManagedManifest, error) {
			if expected != protocol.Hash(f.o.Manifest) {
				t.Fatal("unreviewed output")
			}
			c, e := f.input.Resolve()
			if e != nil {
				return f.o.Manifest, e
			}
			op, _ := safefs.NewID("n")
			_, e = worker.ConfigureLEDOutput(ctx, paths.State, c.Worker, worker.LEDOutputRequest{Identity: f.o.Manifest.Identity, Operation: op, Socket: c.LEDSocket, Disabled: disabled}, f.api)
			if e == nil {
				f.o.Manifest.LEDOutput = &install.ManagedLEDOutput{Operation: op, Disabled: disabled}
			}
			return f.o.Manifest, e
		}
		for _, enabled := range []bool{false, true} {
			r.Desired.Notification.StatusLEDs.Enabled = enabled
			r.DesiredHash, _ = declarationFingerprint(r.Desired, r.DesiredScope)
			if _, handled, e := reconcileDeclaredLEDOutput(ctx, r, paths, l); e != nil || !handled {
				t.Fatal("mapping output predecessor", e)
			}
		}
		f.r = r
	}
	if options.unbound {
		return f
	}
	w, _ := safefs.NewID("w")
	e, _ := safefs.NewID("n")
	g, _ := safefs.NewID("g")
	f.reservation = backup.ProducerReservation{Version: 1, Producer: protocol.Producer{ID: w, Epoch: e}, Generation: g}
	old := f.journal(t)
	_, err = enrollReservedBackup(ctx, r, paths, "/helper", f.d, old, f.input, f.reservation, notificationEnrollmentLifecycle{
		inspect: f.inspect,
		enroll: func(ctx context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
			if expected != protocol.Hash(f.o.Manifest) {
				t.Fatal("unreviewed enrollment")
			}
			raw, err := readLimitsRuntime(spec.RuntimeFile)
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
			if len(c.Worker.LEDBindings) != 0 {
				t.Fatal("enrollment bypassed mapping lifecycle")
			}
			op, _ := safefs.NewID("n")
			if _, err = worker.EnrollBootstrap(ctx, paths.State, f.o.Manifest.Identity, c.Worker, op); err != nil {
				t.Fatal(err)
			}
			f.o.Unbound = false
			f.o.Manifest.RuntimeHash, f.o.Manifest.ConfigHash = inputDigest(raw), protocol.Hash(c)
			f.o.Manifest.Revision, _ = safefs.NewID("n")
			return f.o.Manifest, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *mappingFixture) journal(t *testing.T) OperationJournal {
	t.Helper()
	var j OperationJournal
	if err := f.d.ReadJSON("operation.json", &j); err != nil {
		t.Fatal(err)
	}
	return j
}
func (f *mappingFixture) inspect(context.Context, string) (install.InstallationObservation, error) {
	return f.o, nil
}
func (f *mappingFixture) lifecycle(t *testing.T) notificationMappingLifecycle {
	return notificationMappingLifecycle{inspect: f.inspect, migrate: func(ctx context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
		f.calls++
		if expected != protocol.Hash(f.o.Manifest) || spec.RuntimeFile == filepath.Join(f.paths.Config, "consumers/runtime.json") || spec.BinarySHA256 != f.r.HelperSHA256 {
			t.Fatal("unreviewed mapping")
		}
		before, err := install.ReadRuntime(filepath.Join(f.paths.Config, "consumers/runtime.json"))
		if err != nil {
			t.Fatal(err)
		}
		after, err := install.ReadRuntime(spec.RuntimeFile)
		if err != nil {
			t.Fatal(err)
		}
		op, _ := safefs.NewID("n")
		if _, err = worker.MigrateManagedLEDBindings(ctx, f.paths.State, before.Worker, after.Worker, before.LEDSocket, f.api, f.o.Manifest.Identity, op); err != nil {
			return install.ManagedManifest{}, err
		}
		raw, err := readLimitsRuntime(spec.RuntimeFile)
		if err != nil {
			t.Fatal(err)
		}
		f.o.Manifest.RuntimeHash, f.o.Manifest.ConfigHash = inputDigest(raw), protocol.Hash(after)
		f.o.Manifest.Revision, _ = safefs.NewID("n")
		f.o.Manifest.HelperRevision, f.o.Manifest.HelperHash = f.o.Manifest.Revision, f.o.Manifest.BinaryHash
		return f.o.Manifest, nil
	}}
}

func TestDeclaredLEDMappingLostReplyAndQualificationGates(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, withOutput := range []bool{false, true} {
		for _, boundary := range []string{"mapping-intent", "mapping-input-published", "mapping-domain-complete", "domain-reply"} {
			t.Run(boundary+map[bool]string{true: "/output", false: "/plain"}[withOutput], func(t *testing.T) {
				f := newMappingFixture(t, withOutput)
				ctx := context.Background()
				before := f.o.Manifest
				consumerPath := filepath.Join(f.paths.Config, "consumers/runtime.json")
				consumer, err := readLimitsRuntime(consumerPath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = qualifyNotificationControl(ctx, f.r, f.paths, qualificationLifecycle{}); err == nil {
					t.Fatal("qualified before mapping completion")
				}
				if _, err = advanceNotificationDelivery(ctx, f.r, f.paths, f.d, f.journal(t), ControlResult{}, notificationDeliveryLifecycle{}); err == nil {
					t.Fatal("admitted before mapping completion")
				}
				l := f.lifecycle(t)
				stop := errors.New("interrupted mapping")
				l.boundary = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				migrate := l.migrate
				if boundary == "domain-reply" {
					l.migrate = func(ctx context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
						m, err := migrate(ctx, spec, expected)
						if err != nil {
							return m, err
						}
						return m, stop
					}
				}
				if _, err = applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, l); !errors.Is(err, stop) {
					t.Fatal("boundary", boundary, err)
				}
				l.boundary, l.migrate = nil, migrate
				for range 2 {
					got, err := applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, l)
					if err != nil || got.State != "verification" || got.Identity == nil || *got.Identity != before.Identity || f.calls != 1 {
						t.Fatal("mapping replay", got, f.calls, err)
					}
				}
				retained, err := readLimitsRuntime(consumerPath)
				if err != nil || string(retained) != string(consumer) {
					t.Fatal("rewrote original enrollment", err)
				}
				for _, state := range f.api.states {
					if state.Pattern == "heartbeat" {
						t.Fatal("mapping invented healthy evidence")
					}
				}
				if got, err := planNotificationControl(ctx, f.r, f.paths, f.inspect); err != nil || got.State != "up_to_date" {
					t.Fatal("mapped plan", got, err)
				}
				if _, err := inspectNotificationControl(ctx, f.r, f.paths, f.inspect, nil); err != nil {
					t.Fatal("mapped status", err)
				}
				if err := verifyMappingReceipt(f.d, f.journal(t), f.o.Manifest); err != nil {
					t.Fatal("mapping receipt", err)
				}
				if err := verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), f.input, f.reservation); err != nil {
					t.Fatal("mapped admission input", err)
				}
				qualified := 0
				f.r.Action = "qualify-begin"
				_, err = qualifyNotificationControl(ctx, f.r, f.paths, qualificationLifecycle{begin: func(_ context.Context, root, expected string) (install.QualificationSession, error) {
					qualified++
					if root != f.paths.Installation || expected != protocol.Hash(f.o.Manifest) {
						t.Fatal("qualified obsolete enrollment")
					}
					return install.QualificationSession{}, nil
				}})
				if err != nil || qualified != 1 {
					t.Fatal("mapped qualification", err)
				}
				completed := f.journal(t)
				intent := filepath.Join(f.paths.Control, "mapping-"+completed.Children[0].Operation+".json")
				if err = os.Rename(intent, intent+".lost"); err != nil {
					t.Fatal(err)
				}
				if _, err = applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, completed, f.input, f.reservation, l); err == nil || f.calls != 1 {
					t.Fatal("recreated missing mapping authority", err)
				}
				if _, err = inspectNotificationControl(ctx, f.r, f.paths, f.inspect, nil); err == nil {
					t.Fatal("missing mapping reported healthy")
				}
			})
		}
	}

}

func TestDeclaredLEDMappingDeliveryUsesMappedReceipt(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	f := newMappingFixture(t)
	ctx := context.Background()
	result, err := applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, f.lifecycle(t))
	if err != nil {
		t.Fatal(err)
	}
	mapped := f.journal(t)
	after := f.o.Manifest
	after.Mode, after.DeliveryOwnerHash = "delivery", protocol.Hash("fixture-owner")
	after.DeliveryActivation = &install.ManagedDeliveryActivation{Operation: "fixture-activation", Hash: protocol.Hash("fixture-proof")}
	admitted, calls := false, 0
	request := install.DeliveryAdmissionRequest{Root: f.paths.Installation, ManifestHash: protocol.Hash(f.o.Manifest), QualificationHash: protocol.Hash("fixture-qualified")}
	l := notificationDeliveryLifecycle{
		review: func(_ context.Context, root, expected string) (install.DeliveryAdmissionRequest, bool, error) {
			if root != request.Root || expected != request.ManifestHash {
				t.Fatal("delivery used obsolete enrollment")
			}
			return request, true, nil
		},
		inspect: func(_ context.Context, got install.DeliveryAdmissionRequest) (install.ManagedManifest, bool, error) {
			if got != request {
				t.Fatal("changed delivery pins")
			}
			return after, admitted, nil
		},
		ensure: func(_ context.Context, got install.DeliveryAdmissionRequest) (install.ManagedManifest, error) {
			calls++
			if got != request {
				t.Fatal("changed admission")
			}
			admitted = true
			return after, errors.New("lost admission reply")
		},
		backupStatus: func(context.Context, string) (backup.SetupObservation, error) {
			return backup.SetupObservation{Stage: "started", DesiredHash: f.r.DesiredHash}, nil
		},
	}
	if _, err = advanceNotificationDelivery(ctx, f.r, f.paths, f.d, mapped, result, l); err == nil {
		t.Fatal("lost reply hidden")
	}
	for range 2 {
		got, err := advanceNotificationDelivery(ctx, f.r, f.paths, f.d, f.journal(t), result, l)
		if err != nil || got.State != "delivery_admitted" || calls != 1 {
			t.Fatal("mapping delivery retry", got, calls, err)
		}
	}
	if err = verifyReservedConsumerInput(f.r, f.paths, f.d, mapped, f.input, f.reservation); err != nil {
		t.Fatal("delivery input", err)
	}
	// Enrollment input remains auditable after the active runtime contains bindings.
	if err = os.Remove(filepath.Join(f.paths.Config, "consumers/runtime.json")); err != nil {
		t.Fatal(err)
	}
	if err = verifyReservedConsumerInput(f.r, f.paths, f.d, mapped, f.input, f.reservation); err == nil {
		t.Fatal("delivery ignored missing enrollment history")
	}
}

func TestDeclaredLEDMappingRejectsPolicyAndReceiptDrift(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, change := range []string{"generation", "bindings", "runtime", "helper", "receipt", "output-history"} {
		t.Run(change, func(t *testing.T) {
			f := newMappingFixture(t)
			old := f.journal(t)
			switch change {
			case "generation":
				f.reservation.Generation, _ = safefs.NewID("g")
			case "bindings":
				b := f.r.Desired.Notification.StatusLEDs.Bindings["backup"]
				b.Subjects = []string{"changed"}
				f.r.Desired.Notification.StatusLEDs.Bindings["backup"] = b
				f.r.DesiredHash, _ = f.r.Desired.Fingerprint()
			case "runtime":
				if err := os.WriteFile(filepath.Join(f.paths.Config, "consumers/runtime.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "helper":
				f.r.HelperSHA256 = strings.Repeat("b", 64)
			case "receipt":
				f.o.Manifest.Identity.Recovery, _ = safefs.NewID("g")
			case "output-history":
				f.o.Manifest.LEDOutput = &install.ManagedLEDOutput{Disabled: true}
				old.Children[0].Receipt = protocol.Hash(f.o.Manifest)
				if err := f.d.WriteJSON("operation.json", old, true); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := applyEnrolledLEDBindings(context.Background(), f.r, f.paths, "/helper", f.d, old, f.input, f.reservation, f.lifecycle(t)); err == nil {
				t.Fatal("drift accepted")
			}
			if f.calls != 0 || f.api.mutations != 0 || f.journal(t).Digest() != old.Digest() {
				t.Fatal("refusal mutated authority")
			}
		})
	}
}
