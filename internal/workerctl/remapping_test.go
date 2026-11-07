package workerctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeclaredLEDMappingRevisionsRetainEnrollment(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless mapping")
	}
	for _, boundary := range []string{"none", "mapping-intent", "mapping-input-published", "mapping-domain-complete", "scope-reply", "native:mapping-intent", "native:mapping-domain-complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			nativeCase := strings.HasPrefix(boundary, "native:")
			boundary = strings.TrimPrefix(boundary, "native:")
			f := newMappingFixture(t, false, true, nativeCase)
			_, err := applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, f.lifecycle(t))
			testutil.Must(t, err)
			originalEnrollment, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			currentRuntime, err := readMappingIntent(f.d, f.journal(t))
			testutil.Must(t, err)
			raw := currentRuntime.AfterRuntime
			stop := errors.New("interrupted mapping revision")
			fired, domainCalls := false, 0
			receipts := map[string]string{}
			l := notificationRemappingLifecycle{notificationMappingLifecycle: notificationMappingLifecycle{inspect: f.inspect}}
			l.delivery.review = func(context.Context, string, string) (install.DeliveryAdmissionRequest, bool, error) {
				return install.DeliveryAdmissionRequest{}, false, nil
			}
			l.boundary = func(at string) error {
				if !fired && at == boundary {
					fired = true
					return stop
				}
				return nil
			}
			l.migrate = func(ctx context.Context, spec install.ManagedSpec, expected string) (install.ManagedManifest, error) {
				if expected != protocol.Hash(f.o.Manifest) {
					return install.ManagedManifest{}, errors.New("changed manifest")
				}
				var b, a install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &b, maxControlFrame))
				next, err := readLimitsRuntime(spec.RuntimeFile)
				testutil.Must(t, err)
				testutil.Must(t, safefs.DecodeJSON(next, &a, maxControlFrame))
				before, err := b.Resolve()
				testutil.Must(t, err)
				after, err := a.Resolve()
				testutil.Must(t, err)
				op := testutil.ID(t, "n")
				_, err = worker.MigrateManagedLEDBindings(ctx, f.paths.State, before.Worker, after.Worker, before.LEDSocket, f.api, f.o.Manifest.Identity, op)
				if err != nil {
					return f.o.Manifest, err
				}
				domainCalls++
				raw = next
				f.o.Manifest.RuntimeHash, f.o.Manifest.ConfigHash = inputDigest(next), protocol.Hash(after)
				f.o.Manifest.Revision = testutil.ID(t, "n")
				f.o.Manifest.HelperRevision, f.o.Manifest.HelperHash = f.o.Manifest.Revision, f.o.Manifest.BinaryHash
				return f.o.Manifest, nil
			}
			l.scope = func(_ context.Context, _ string, request backup.VerificationNotificationScopeChange, change func(backup.ProducerReservation) (string, error)) (string, error) {
				if receipt := receipts[request.Operation]; receipt != "" {
					return receipt, nil
				}
				receipt, err := change(f.reservation)
				if err != nil {
					return "", err
				}
				if boundary == "scope-reply" && !fired {
					fired = true
					return "", stop
				}
				receipts[request.Operation] = receipt
				return receipt, nil
			}
			l.scopeInspect = func(_, op, _, _ string) (string, bool, error) {
				receipt := receipts[op]
				return receipt, receipt != "", nil
			}
			var nativeBefore map[string]string
			restarts := 0
			if nativeCase {
				nativeBefore = planTree(t, filepath.Join(f.paths.Control, "signal"))
				native := f.native.lifecycle
				native.gateway = f.inspect
				stopped := true
				native.inspect = func(context.Context, install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
					if stopped {
						return *f.service, false, install.FreshSignalResumeRequired{Reference: *f.o.Manifest.FreshSignal}
					}
					return *f.service, true, nil
				}
				native.resume = func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
					if ref != *f.o.Manifest.FreshSignal || !stopped {
						t.Fatal("unowned native restart")
					}
					stopped = false
					restarts++
					return *f.service, nil
				}
				l.nativeResume = func(ctx context.Context, r ControlRequest, p ResolvedPaths, helper string) error {
					if !fired || !stopped {
						return nil
					}
					r.Action = "resume"
					return resumeCompletedSignalChild(ctx, r, p, helper, f.native.nativeSource, native)
				}
			}
			l.delivery.scopeInspect = l.scopeInspect
			originalBinding := f.r.Desired.Notification.StatusLEDs.Bindings["backup"]
			for revision, subjects := range [][]string{{"source/database"}, {"source/other"}, nil, {"*"}} {
				entry := originalBinding
				entry.Subjects = subjects
				if subjects == nil {
					f.r.Desired.Notification.StatusLEDs.Bindings = nil
				} else {
					f.r.Desired.Notification.StatusLEDs.Bindings = map[string]config.LEDBindingDeclaration{"backup": entry}
				}
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				got, handled, err := reconcileDeclaredLEDMappings(ctx, f.r, f.paths, "/helper", l)
				if revision == 0 && boundary != "none" {
					if !errors.Is(err, stop) || !handled {
						t.Fatal("boundary missed", boundary, got, err)
					}
					got, handled, err = reconcileDeclaredLEDMappings(ctx, f.r, f.paths, "/helper", l)
				}
				if err != nil || !handled || got.Code != "ok" || got.State != "verification" {
					t.Fatal(got, handled, err)
				}
				var input install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
				current := f.journal(t)
				if !current.Complete || !remappingJournal(current) {
					t.Fatal("mapping revision did not complete")
				}
				tree := planTree(t, f.paths.Control)
				if _, handled, err := reconcileDeclaredLEDMappings(ctx, f.r, f.paths, "/helper", l); err != nil || !handled || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
					t.Fatal("completed retry changed state", err)
				}
				output := notificationLEDLifecycle{inspect: f.inspect, set: func(ctx context.Context, root, expected string, disabled bool) (install.ManagedManifest, error) {
					if expected != protocol.Hash(f.o.Manifest) {
						return f.o.Manifest, errors.New("unreviewed output")
					}
					var input install.RuntimeInput
					testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
					c, err := input.Resolve()
					if err != nil {
						return f.o.Manifest, err
					}
					op := testutil.ID(t, "n")
					_, err = worker.ConfigureLEDOutput(ctx, f.paths.State, c.Worker, worker.LEDOutputRequest{Identity: f.o.Manifest.Identity, Operation: op, Socket: c.LEDSocket, Disabled: disabled}, f.api)
					if err == nil {
						f.o.Manifest.LEDOutput = &install.ManagedLEDOutput{Operation: op, Disabled: disabled}
					}
					return f.o.Manifest, err
				}}
				for _, enabled := range []bool{false, true} {
					f.r.Desired.Notification.StatusLEDs.Enabled = enabled
					f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
					testutil.Must(t, err)
					got, handled, err := reconcileEnrolledLEDOutput(ctx, f.r, f.paths, "/helper", l, output)
					if err != nil || !handled || got.LEDOutput == nil || got.LEDOutput.Disabled == enabled {
						t.Fatal("enrolled output", got, handled, err)
					}
					testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
				}

			}
			if nativeCase && (restarts != 1 || !reflect.DeepEqual(nativeBefore, planTree(t, filepath.Join(f.paths.Control, "signal")))) {
				t.Fatal("native ownership changed", restarts)
			}
			if domainCalls != 4 {
				t.Fatal("lost reply replayed mapping", domainCalls)
			}
			current := f.journal(t)
			delete(receipts, current.Children[0].Operation)
			reviewCalled := false
			l.delivery.review = func(context.Context, string, string) (install.DeliveryAdmissionRequest, bool, error) {
				reviewCalled = true
				return install.DeliveryAdmissionRequest{}, false, nil
			}
			if _, err := advanceNotificationDelivery(ctx, f.r, f.paths, f.d, current, ControlResult{}, l.delivery); err == nil || reviewCalled {
				t.Fatal("missing scope admitted delivery", err)
			}
			q := f.r
			q.Action = "qualify-begin"
			if _, err := qualifyNotificationControl(ctx, q, f.paths, qualificationLifecycle{begin: func(context.Context, string, string) (install.QualificationSession, error) {
				reviewCalled = true
				return install.QualificationSession{}, nil
			}}); err == nil || reviewCalled {
				t.Fatal("missing backup scope admitted qualification", err)
			}
			currentEnrollment, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			if !reflect.DeepEqual(originalEnrollment, currentEnrollment) {
				t.Fatal("mapping revision rewrote original producer enrollment")
			}
		})
	}
}

func TestMappingRevisionRejectsMixedPolicy(t *testing.T) {
	f := newMappingFixture(t, false, true)
	before := f.r.Desired
	after := before
	n := *before.Notification
	after.Notification = &n
	n.StatusLEDs.Bindings = nil
	testutil.Must(t, validateRemappingPolicy(before, after, f.r.DesiredHash))
	n.Limits.MaxMessages++
	if err := validateRemappingPolicy(before, after, f.r.DesiredHash); err == nil || !strings.Contains(err.Error(), "separately") {
		t.Fatal("mixed policy admitted", err)
	}
}

func TestDeclaredMappingRevisionRefusesDriftBeforeIntent(t *testing.T) {
	for _, damage := range []string{"provider", "helper", "enabled", "paths", "credential", "manifest", "production"} {
		t.Run(damage, func(t *testing.T) {
			f := newMappingFixture(t, false, true)
			_, err := applyEnrolledLEDBindings(context.Background(), f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, f.lifecycle(t))
			testutil.Must(t, err)
			f.r.Desired.Notification.StatusLEDs.Bindings = nil
			switch damage {
			case "provider":
				f.r.Desired.Notification.Limits.MaxMessages++
			case "helper":
				f.r.HelperSHA256 = protocol.Hash("new helper")
			case "enabled":
				f.r.Desired.Notification.StatusLEDs.Enabled = false
			case "paths":
				f.paths.Installation += "-other"
			case "credential":
				f.r.Inputs["token"] = []byte("123:changed")
			case "manifest":
				f.o.Manifest.Revision = testutil.ID(t, "n")
			case "production":
				f.o.Manifest.Mode = "production"
			}
			f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
			testutil.Must(t, err)
			tree := planTree(t, f.paths.Control)
			calls := 0
			l := notificationRemappingLifecycle{notificationMappingLifecycle: notificationMappingLifecycle{inspect: f.inspect}, scope: func(context.Context, string, backup.VerificationNotificationScopeChange, func(backup.ProducerReservation) (string, error)) (string, error) {
				calls++
				return "", nil
			}}
			if _, handled, err := reconcileDeclaredLEDMappings(context.Background(), f.r, f.paths, "/helper", l); err == nil || !handled || calls != 0 || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
				t.Fatal("drift changed mapping authority", handled, err, calls)
			}
		})
	}
}
