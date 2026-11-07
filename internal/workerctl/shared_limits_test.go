package workerctl

import (
	"context"
	"errors"
	"fmt"
	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSharedLimitsRetainEnrollmentAcrossLostReplies(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless mapping")
	}
	for _, boundary := range []string{"none", "shared-limits-intent", "shared-limits-input-published", "shared-limits-domain-complete", "scope-reply", "native:shared-limits-intent", "native:shared-limits-domain-complete", "led:none", "led:shared-limits-domain-complete", "led:scope-reply"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			ledCase := strings.HasPrefix(boundary, "led:")
			boundary = strings.TrimPrefix(boundary, "led:")
			nativeCase := strings.HasPrefix(boundary, "native:")
			boundary = strings.TrimPrefix(boundary, "native:")
			f := newMappingFixture(t, ledCase, true, nativeCase, false, !ledCase)
			if ledCase {
				_, e := applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, f.lifecycle(t))
				testutil.Must(t, e)
			}
			originalEnrollment, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			raw, err := readLimitsRuntime(filepath.Join(f.paths.Config, "consumers/runtime.json"))
			if ledCase {
				raw, err = readLimitsRuntime(filepath.Join(f.paths.Control, "mapping-"+f.journal(t).Children[0].Operation+"-runtime.json"))
			}
			testutil.Must(t, err)
			stop := errors.New("interrupted limit update")
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
				_, err = worker.MigrateEnrolledLimits(ctx, f.paths.State, f.o.Manifest.Identity, before.Worker, after.Worker, op)
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
			for revision, limit := range []int{2000, 900, 1200} {
				f.r.Desired.Notification.Limits.MaxMessages = limit
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				got, handled, err := reconcileSharedLimits(ctx, f.r, f.paths, "/helper", l)
				if revision == 0 && boundary != "none" {
					if !errors.Is(err, stop) || !handled {
						t.Fatal("boundary missed", boundary, got, err)
					}
					got, handled, err = reconcileSharedLimits(ctx, f.r, f.paths, "/helper", l)
				}
				if err != nil || !handled || got.Code != "ok" || got.State != "verification" {
					t.Fatal(got, handled, err)
				}
				var input install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
				current := f.journal(t)
				projected := f.input
				projected.Worker.MaxMessages = input.Worker.MaxMessages
				wrong := projected
				wrong.Worker.MaxMessages++
				if err = retainBootstrapLimitsInput(f.d, current, &wrong); err == nil {
					t.Fatal("unreviewed limits matched bootstrap pins")
				}
				testutil.Must(t, retainBootstrapLimitsInput(f.d, current, &projected))
				if protocol.Hash(projected) != protocol.Hash(f.input) {
					t.Fatal("limit projection changed original bootstrap input")
				}
				if !current.Complete || !sharedLimitsJournal(current) {
					t.Fatal("limit update did not complete")
				}
				tree := planTree(t, f.paths.Control)
				if _, handled, err := reconcileSharedLimits(ctx, f.r, f.paths, "/helper", l); err != nil || !handled || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
					t.Fatal("completed retry changed state", err)
				}

			}
			if nativeCase && (restarts != 1 || !reflect.DeepEqual(nativeBefore, planTree(t, filepath.Join(f.paths.Control, "signal")))) {
				t.Fatal("native ownership changed", restarts)
			}
			if domainCalls != 3 {
				t.Fatal("lost reply replayed limits", domainCalls)
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
				t.Fatal("limit update rewrote original producer enrollment")
			}
		})
	}
}

func TestSharedFreshnessRetainsEnrollmentAcrossLostReplies(t *testing.T) {
	testSharedFreshnessRetainsEnrollmentAcrossLostReplies(t, false)
}
func TestSharedDerivedFreshnessRetainsEnrollmentAcrossLostReplies(t *testing.T) {
	testSharedFreshnessRetainsEnrollmentAcrossLostReplies(t, true)
}
func testSharedFreshnessRetainsEnrollmentAcrossLostReplies(t *testing.T, derived bool) {
	if os.Getuid() == 0 {
		t.Skip("rootless mapping")
	}
	for _, boundary := range []string{"none", "shared-limits-intent", "shared-limits-input-published", "shared-limits-domain-complete", "scope-reply", "native:shared-limits-intent", "native:shared-limits-domain-complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			nativeCase := strings.HasPrefix(boundary, "native:")
			boundary = strings.TrimPrefix(boundary, "native:")
			f := newMappingFixture(t, false, true, nativeCase, false, true)
			originalEnrollment, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			raw, err := readLimitsRuntime(filepath.Join(f.paths.Config, "consumers/runtime.json"))
			testutil.Must(t, err)
			stop := errors.New("interrupted limit update")
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
				_, err = worker.MigrateEnrolledFreshness(ctx, f.paths.State, f.o.Manifest.Identity, before.Worker, after.Worker, op)
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
			var initial install.RuntimeInput
			testutil.Must(t, safefs.DecodeJSON(raw, &initial, maxControlFrame))
			reconcile := reconcileSharedFreshness
			silences := []int{initial.Worker.Producers[0].MaxSilenceSeconds, 180, 300, 120}
			if derived {
				silences = []int{180, 300, 120}
				reconcile = func(ctx context.Context, r ControlRequest, p ResolvedPaths, h string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
					return reconcileSharedPolicy(ctx, r, p, h, l, "derived-freshness")
				}
			}
			for revision, silence := range silences {
				evidence := 172800
				if revision == 0 {
					evidence = initial.Worker.Producers[0].MaxEvidenceSeconds
				}
				if derived {
					f.r.Desired.Backup.Freshness = []string{"48h", "72h", "50h"}[revision]
				} else {
					f.r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {Routes: []string{"alerts"}, MaxMessages: 1000, MaxSilenceSeconds: silence, MaxEvidenceSeconds: evidence, PolicyHealth: true}}
				}
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				got, handled, err := reconcile(ctx, f.r, f.paths, "/helper", l)
				if revision == 0 && boundary != "none" {
					if !errors.Is(err, stop) || !handled {
						t.Fatal("boundary missed", boundary, got, err)
					}
					got, handled, err = reconcile(ctx, f.r, f.paths, "/helper", l)
				}
				if err != nil || !handled || got.Code != "ok" || got.State != "verification" {
					t.Fatal(got, handled, err)
				}
				var input install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
				current := f.journal(t)
				projected := f.input
				projected.Worker.MaxMessages = input.Worker.MaxMessages
				wrong := projected
				wrong.Worker.MaxMessages++
				if err = retainBootstrapLimitsInput(f.d, current, &wrong); err == nil {
					t.Fatal("unreviewed limits matched bootstrap pins")
				}
				testutil.Must(t, retainBootstrapLimitsInput(f.d, current, &projected))
				if protocol.Hash(projected) != protocol.Hash(f.input) {
					t.Fatal("limit projection changed original bootstrap input")
				}
				if !current.Complete || !sharedLimitsJournal(current) {
					t.Fatal("limit update did not complete")
				}
				tree := planTree(t, f.paths.Control)
				if _, handled, err := reconcile(ctx, f.r, f.paths, "/helper", l); err != nil || !handled || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
					t.Fatal("completed retry changed state", err)
				}

			}
			if nativeCase && (restarts != 1 || !reflect.DeepEqual(nativeBefore, planTree(t, filepath.Join(f.paths.Control, "signal")))) {
				t.Fatal("native ownership changed", restarts)
			}
			if domainCalls != 3 {
				t.Fatal("lost reply replayed limits", domainCalls)
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
				t.Fatal("limit update rewrote original producer enrollment")
			}
		})
	}
}

func TestSharedRoutesRetainsEnrollmentAcrossLostReplies(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless mapping")
	}
	for _, boundary := range []string{"none", "shared-limits-intent", "shared-limits-input-published", "shared-limits-domain-complete", "scope-reply", "native:shared-limits-intent", "native:shared-limits-domain-complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			nativeCase := strings.HasPrefix(boundary, "native:")
			boundary = strings.TrimPrefix(boundary, "native:")
			f := newMappingFixture(t, false, true, nativeCase, false, true)
			originalEnrollment, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			raw, err := readLimitsRuntime(filepath.Join(f.paths.Config, "consumers/runtime.json"))
			testutil.Must(t, err)
			stop := errors.New("interrupted limit update")
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
				_, err = worker.MigrateEnrolledRoutes(ctx, f.paths.State, f.o.Manifest.Identity, before.Worker, after.Worker, op)
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
			for revision, name := range []string{"audit", "logs", "trace"} {
				f.r.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "telegram"}, name: {Primary: "telegram"}}
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				got, handled, err := reconcileSharedRoutes(ctx, f.r, f.paths, "/helper", l)
				if revision == 0 && boundary != "none" {
					if !errors.Is(err, stop) || !handled {
						t.Fatal("boundary missed", boundary, got, err)
					}
					got, handled, err = reconcileSharedRoutes(ctx, f.r, f.paths, "/helper", l)
				}
				if err != nil || !handled || got.Code != "ok" || got.State != "verification" {
					t.Fatal(got, handled, err)
				}
				var input install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
				current := f.journal(t)
				projected := f.input
				projected.Worker.Routes = input.Worker.Routes
				projected.Worker.MaxMessages = input.Worker.MaxMessages
				wrong := projected
				wrong.Worker.MaxMessages++
				if err = retainBootstrapLimitsInput(f.d, current, &wrong); err == nil {
					t.Fatal("unreviewed limits matched bootstrap pins")
				}
				testutil.Must(t, retainBootstrapLimitsInput(f.d, current, &projected))
				if protocol.Hash(projected) != protocol.Hash(f.input) {
					t.Fatal("limit projection changed original bootstrap input")
				}
				if !current.Complete || !sharedLimitsJournal(current) {
					t.Fatal("limit update did not complete")
				}
				tree := planTree(t, f.paths.Control)
				if _, handled, err := reconcileSharedRoutes(ctx, f.r, f.paths, "/helper", l); err != nil || !handled || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
					t.Fatal("completed retry changed state", err)
				}

			}
			if nativeCase && (restarts != 1 || !reflect.DeepEqual(nativeBefore, planTree(t, filepath.Join(f.paths.Control, "signal")))) {
				t.Fatal("native ownership changed", restarts)
			}
			if domainCalls != 3 {
				t.Fatal("lost reply replayed limits", domainCalls)
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
				t.Fatal("limit update rewrote original producer enrollment")
			}
		})
	}
}

func TestSharedProducerPolicyRetainsEnrollmentAcrossLostReplies(t *testing.T) {
	testSharedProducerPolicyRetainsEnrollmentAcrossLostReplies(t, false, false)
}
func TestSharedReportRouteRetainsEnrollmentAcrossLostReplies(t *testing.T) {
	testSharedProducerPolicyRetainsEnrollmentAcrossLostReplies(t, true, false)
}
func TestSharedExplicitProducerReportRouteRetainsEnrollmentAcrossLostReplies(t *testing.T) {
	testSharedProducerPolicyRetainsEnrollmentAcrossLostReplies(t, true, true)
}
func testSharedProducerPolicyRetainsEnrollmentAcrossLostReplies(t *testing.T, reportRoute, explicit bool) {
	if os.Getuid() == 0 {
		t.Skip("rootless mapping")
	}
	boundaries := []string{"none", "shared-limits-intent", "shared-limits-input-published", "shared-limits-domain-complete", "scope-reply"}
	// Exercise notification-first native setup with declared producer policy,
	// then verify that later route selection retains the same service.
	boundaries = append(boundaries, "native:shared-limits-intent", "native:shared-limits-domain-complete")
	for _, boundary := range boundaries {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			nativeCase := strings.HasPrefix(boundary, "native:")
			boundary = strings.TrimPrefix(boundary, "native:")
			f := newMappingFixtureWithRoutes(t, reportRoute, false, true, nativeCase, false, true, explicit)
			originalEnrollment, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			raw, err := readLimitsRuntime(filepath.Join(f.paths.Config, "consumers/runtime.json"))
			testutil.Must(t, err)
			stop := errors.New("interrupted limit update")
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
				_, err = worker.MigrateEnrolledProducerPolicy(ctx, f.paths.State, f.o.Manifest.Identity, before.Worker, after.Worker, op)
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
			var initial install.RuntimeInput
			testutil.Must(t, safefs.DecodeJSON(raw, &initial, maxControlFrame))
			reconcile := reconcileSharedProducerPolicy
			silences := []int{initial.Worker.Producers[0].MaxSilenceSeconds, 180, 300, 120}
			if reportRoute {
				silences = []int{180, 300, 120}
				reconcile = func(ctx context.Context, r ControlRequest, p ResolvedPaths, h string, l notificationRemappingLifecycle) (ControlResult, bool, error) {
					return reconcileSharedPolicy(ctx, r, p, h, l, "report-route")
				}
			}
			for revision, silence := range silences {
				evidence := 172800
				if revision == 0 {
					evidence = initial.Worker.Producers[0].MaxEvidenceSeconds
				}
				if reportRoute {
					f.r.Desired.Backup.Notifications.Route = []string{"audit", "alerts", "audit"}[revision]
				} else {
					f.r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {Routes: []string{"alerts"}, MaxMessages: 1000 - revision*100, MaxSilenceSeconds: silence, MaxEvidenceSeconds: evidence, PolicyHealth: true, InteractiveContext: revision > 0}}
					if revision > 0 {
						p := f.r.Desired.Notification.Producers["backup-worker"]
						p.ContextKinds = []string{"source"}
						f.r.Desired.Notification.Producers["backup-worker"] = p
					}
				}
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				got, handled, err := reconcile(ctx, f.r, f.paths, "/helper", l)
				if revision == 0 && boundary != "none" {
					if !errors.Is(err, stop) || !handled {
						t.Fatal("boundary missed", boundary, got, err)
					}
					got, handled, err = reconcile(ctx, f.r, f.paths, "/helper", l)
				}
				if err != nil || !handled || got.Code != "ok" || got.State != "verification" {
					t.Fatal(got, handled, err)
				}
				var input install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
				current := f.journal(t)
				projected := f.input
				projected.Worker.MaxMessages = input.Worker.MaxMessages
				wrong := projected
				wrong.Worker.MaxMessages++
				if err = retainBootstrapLimitsInput(f.d, current, &wrong); err == nil {
					t.Fatal("unreviewed limits matched bootstrap pins")
				}
				testutil.Must(t, retainBootstrapLimitsInput(f.d, current, &projected))
				if protocol.Hash(projected) != protocol.Hash(f.input) {
					t.Fatal("limit projection changed original bootstrap input")
				}
				if !current.Complete || !sharedLimitsJournal(current) {
					t.Fatal("limit update did not complete")
				}
				tree := planTree(t, f.paths.Control)
				if _, handled, err := reconcile(ctx, f.r, f.paths, "/helper", l); err != nil || !handled || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
					t.Fatal("completed retry changed state", err)
				}

			}
			if nativeCase && (restarts != 1 || !reflect.DeepEqual(nativeBefore, planTree(t, filepath.Join(f.paths.Control, "signal")))) {
				t.Fatal("native ownership changed", restarts)
			}
			wantCalls := 3
			if explicit {
				wantCalls = 0
			}
			if domainCalls != wantCalls {
				t.Fatal("lost reply replayed limits", domainCalls)
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
				t.Fatal("limit update rewrote original producer enrollment")
			}
		})
	}
}

func TestSharedProvidersRetainsEnrollmentAcrossLostReplies(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless mapping")
	}
	for _, boundary := range []string{"none", "shared-limits-intent", "shared-limits-input-published", "shared-limits-domain-complete", "scope-reply", "native:shared-limits-intent", "native:shared-limits-domain-complete", "led:shared-limits-domain-complete", "profiles:shared-limits-intent", "profiles:shared-limits-input-published", "profiles:shared-limits-domain-complete", "profiles:scope-reply"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			ledCase := strings.HasPrefix(boundary, "led:")
			boundary = strings.TrimPrefix(boundary, "led:")
			profilesCase := strings.HasPrefix(boundary, "profiles:")
			boundary = strings.TrimPrefix(boundary, "profiles:")
			nativeCase := profilesCase || strings.HasPrefix(boundary, "native:")
			boundary = strings.TrimPrefix(boundary, "native:")
			f := newMappingFixtureWithProfiles(t, false, profilesCase, ledCase, true, nativeCase, false, !ledCase)
			if ledCase {
				_, e := applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, f.lifecycle(t))
				testutil.Must(t, e)
			}
			originalEnrollment, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			raw, err := retainedEnrolledRuntime(f.d, f.paths, f.journal(t))
			testutil.Must(t, err)
			stop := errors.New("interrupted limit update")
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
				testutil.Must(t, install.ValidateManagedEnrolledProviders(f.o.Manifest, before, after))
				op := testutil.ID(t, "n")
				_, err = worker.MigrateEnrolledProviders(ctx, f.paths.State, f.o.Manifest.Identity, before.Worker, after.Worker, op)
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
			delete(f.r.Inputs, "token")
			delete(f.r.Inputs, "chat_id")
			for revision := 0; revision < 3; revision++ {
				next := f.r.Desired.Notification.Providers["telegram"]
				next.TokenFile = filepath.Join(f.paths.Config, fmt.Sprintf("token-%d", revision))
				next.ChatIDFile = filepath.Join(f.paths.Config, fmt.Sprintf("chat-%d", revision))
				next.TokenInput, next.ChatIDInput = "", ""
				token := []byte(fmt.Sprintf("123:rotated-%d", revision))
				chat := []byte("456")
				if revision == 1 {
					chat = []byte("987")
				}
				testutil.Must(t, os.WriteFile(next.TokenFile, token, 0600))
				testutil.Must(t, os.WriteFile(next.ChatIDFile, chat, 0600))
				providers := config.ProviderDeclarations{}
				for name, p := range f.r.Desired.Notification.Providers {
					providers[name] = p
				}
				providers["telegram"] = next
				if profilesCase && revision == 0 {
					delete(providers, "telegram")
				}
				f.r.Desired.Notification.Providers = providers
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				got, handled, err := reconcileSharedProviders(ctx, f.r, f.paths, "/helper", l)
				if revision == 0 && boundary != "none" {
					if !errors.Is(err, stop) || !handled {
						t.Fatal("boundary missed", boundary, got, err)
					}
					selected := f.r.Desired.Notification.Providers["telegram"].TokenFile
					if profilesCase {
						selected = f.input.Providers["telegram"].TokenFile
					}
					saved, e := os.ReadFile(selected)
					testutil.Must(t, e)
					testutil.Must(t, os.WriteFile(selected, []byte("123:unreviewed-change"), 0600))
					tree := planTree(t, f.paths.Control)
					if _, _, e = reconcileSharedProviders(ctx, f.r, f.paths, "/helper", l); e == nil || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
						t.Fatal("changed pending credential accepted or changed state", e)
					}
					testutil.Must(t, os.WriteFile(selected, saved, 0600))
					got, handled, err = reconcileSharedProviders(ctx, f.r, f.paths, "/helper", l)
				}
				if err != nil || !handled || got.Code != "ok" || got.State != "verification" {
					t.Fatal(got, handled, err)
				}
				var input install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, f.journal(t), input, f.reservation))
				current := f.journal(t)
				projected := f.input
				projected.Providers = input.Providers
				projected.Worker.Routes = input.Worker.Routes
				projected.Worker.MaxMessages = input.Worker.MaxMessages
				wrong := projected
				wrong.Worker.MaxMessages++
				if err = retainBootstrapLimitsInput(f.d, current, &wrong); err == nil {
					t.Fatal("unreviewed limits matched bootstrap pins")
				}
				testutil.Must(t, retainBootstrapLimitsInput(f.d, current, &projected))
				if protocol.Hash(projected) != protocol.Hash(f.input) {
					t.Fatal("limit projection changed original bootstrap input")
				}
				if !current.Complete || !sharedLimitsJournal(current) {
					t.Fatal("limit update did not complete")
				}
				tree := planTree(t, f.paths.Control)
				if _, handled, err := reconcileSharedProviders(ctx, f.r, f.paths, "/helper", l); err != nil || !handled || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
					t.Fatal("completed retry changed state", err)
				}

			}
			if nativeCase && (restarts != 1 || !reflect.DeepEqual(nativeBefore, planTree(t, filepath.Join(f.paths.Control, "signal")))) {
				t.Fatal("native ownership changed", restarts)
			}
			if domainCalls != 3 {
				t.Fatal("lost reply replayed limits", domainCalls)
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
				t.Fatal("limit update rewrote original producer enrollment")
			}
		})
	}
}
