package workerctl

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeliveryProvidersCoordinateRepeatedRotationAndQualification(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, boundary := range []string{"derived:delivery-provider-intent", "derived:delivery-provider-paused", "derived:rotate-reply", "derived:scope-reply", "derived:native:rotate-reply", "derived:led:scope-reply", "derived:disabled-led:rotate-reply", "delivery-provider-intent", "delivery-provider-paused", "delivery-provider-rotated", "pause-reply", "rotate-reply", "scope-reply", "led:delivery-provider-paused", "disabled-led:delivery-provider-rotated", "native:delivery-provider-intent", "native:delivery-provider-paused", "native:delivery-provider-rotated", "native:pause-reply", "native:rotate-reply", "native:scope-reply", "limits:delivery-provider-intent", "limits:delivery-provider-paused", "limits:delivery-provider-rotated", "limits:pause-reply", "limits:rotate-reply", "limits:scope-reply", "limits:native:rotate-reply", "limits:led:delivery-provider-paused", "limits:disabled-led:delivery-provider-rotated", "resume-native:delivery-provider-paused", "limits:resume-native:delivery-provider-rotated", "limits:signal-only:delivery-provider-paused", "limits:signal-only:rotate-reply", "producer:delivery-provider-intent", "producer:delivery-provider-paused", "producer:delivery-provider-rotated", "producer:pause-reply", "producer:rotate-reply", "producer:scope-reply", "producer:native:rotate-reply", "producer:signal-only:rotate-reply", "producer:led:delivery-provider-paused", "producer:disabled-led:delivery-provider-rotated", "routes:delivery-provider-intent", "routes:delivery-provider-paused", "routes:delivery-provider-rotated", "routes:pause-reply", "routes:rotate-reply", "routes:scope-reply", "routes:native:rotate-reply", "routes:signal-only:rotate-reply", "routes:led:delivery-provider-paused", "routes:disabled-led:delivery-provider-rotated", "destination:delivery-provider-intent", "destination:delivery-provider-paused", "destination:delivery-provider-rotated", "destination:pause-reply", "destination:rotate-reply", "destination:scope-reply", "destination:native:rotate-reply", "destination:led:delivery-provider-paused", "destination:disabled-led:delivery-provider-rotated", "enrollment:delivery-provider-intent", "enrollment:delivery-provider-paused", "enrollment:delivery-provider-rotated", "enrollment:pause-reply", "enrollment:rotate-reply", "enrollment:scope-reply", "enrollment:led:delivery-provider-paused", "enrollment:disabled-led:delivery-provider-rotated", "declaration:delivery-provider-intent", "declaration:scope-reply", "declaration:native:delivery-provider-intent", "declaration:led:scope-reply", "declaration:disabled-led:delivery-provider-intent"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			derivedCase := strings.HasPrefix(boundary, "derived:")
			boundary = strings.TrimPrefix(boundary, "derived:")
			declarationCase := strings.HasPrefix(boundary, "declaration:")
			boundary = strings.TrimPrefix(boundary, "declaration:")
			enrollmentCase := strings.HasPrefix(boundary, "enrollment:")
			boundary = strings.TrimPrefix(boundary, "enrollment:")
			destinationCase := strings.HasPrefix(boundary, "destination:")
			boundary = strings.TrimPrefix(boundary, "destination:")
			routesCase := strings.HasPrefix(boundary, "routes:")
			boundary = strings.TrimPrefix(boundary, "routes:")
			producerCase := strings.HasPrefix(boundary, "producer:")
			boundary = strings.TrimPrefix(boundary, "producer:")
			limitsCase := strings.HasPrefix(boundary, "limits:")
			policyCase := derivedCase || limitsCase || producerCase || routesCase || destinationCase || enrollmentCase || declarationCase
			boundary = strings.TrimPrefix(boundary, "limits:")
			signalOnly := enrollmentCase || strings.HasPrefix(boundary, "signal-only:")
			boundary = strings.Replace(boundary, "signal-only:", "native:", 1)
			initialResume := strings.HasPrefix(boundary, "resume-native:")
			boundary = strings.Replace(boundary, "resume-native:", "native:", 1)
			nativeCase := enrollmentCase || strings.HasPrefix(boundary, "native:")
			boundary = strings.TrimPrefix(boundary, "native:")
			disabledLED := strings.HasPrefix(boundary, "disabled-led:")
			ledCase := disabledLED || strings.HasPrefix(boundary, "led:")
			boundary = strings.TrimPrefix(strings.TrimPrefix(boundary, "disabled-led:"), "led:")
			f := newMappingFixtureWithOptions(t, mappingFixtureOptions{signalOnly: signalOnly, signalPrimary: signalOnly}, false, true, nativeCase, false, !ledCase)
			if ledCase {
				_, err := applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, f.lifecycle(t))
				testutil.Must(t, err)
			}
			original := f.journal(t)
			enrollment, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			raw, err := retainedEnrolledRuntime(f.d, f.paths, original)
			testutil.Must(t, err)
			bootstrap, err := readLimitsRuntime(f.paths.Runtime)
			testutil.Must(t, err)
			ready := true
			receipts := map[string]string{}
			var admission install.DeliveryAdmissionRequest
			delivery := notificationDeliveryLifecycle{
				scopeInspect: func(_, op, _, _ string) (string, bool, error) { v, ok := receipts[op]; return v, ok, nil },
				review: func(_ context.Context, root, hash string) (install.DeliveryAdmissionRequest, bool, error) {
					admission = install.DeliveryAdmissionRequest{Root: root, ManifestHash: hash, QualificationHash: protocol.Hash(testutil.ID(t, "n"))}
					return admission, ready, nil
				},
				inspect: func(_ context.Context, r install.DeliveryAdmissionRequest) (install.ManagedManifest, bool, error) {
					if r != admission {
						t.Fatal("admission changed")
					}
					return f.o.Manifest, f.o.Manifest.Mode == "delivery", nil
				},
				ensure: func(_ context.Context, r install.DeliveryAdmissionRequest) (install.ManagedManifest, error) {
					if r != admission {
						t.Fatal("admission changed")
					}
					f.o.Manifest.Mode = "delivery"
					f.o.Manifest.DeliveryOwnerHash = protocol.Hash("retained owner")
					f.o.Manifest.DeliveryActivation = &install.ManagedDeliveryActivation{Operation: testutil.ID(t, "n"), Hash: protocol.Hash(r)}
					f.o.Manifest.UnitHash = protocol.Hash("active unit")
					return f.o.Manifest, nil
				},
				backupStatus: func(context.Context, string) (backup.SetupObservation, error) {
					full, _ := f.r.Desired.Fingerprint()
					return backup.SetupObservation{Stage: "complete", DesiredHash: full}, nil
				},
				connect: func(context.Context, string, string, string) error { return nil },
			}
			_, err = advanceNotificationDelivery(ctx, f.r, f.paths, f.d, original, ControlResult{}, delivery)
			testutil.Must(t, err)
			if disabledLED {
				toggleRequalificationLEDOutput(t, f, false, receipts)
			}
			initial := f.o.Manifest
			var initialInput install.RuntimeInput
			testutil.Must(t, safefs.DecodeJSON(raw, &initialInput, maxControlFrame))
			initialConfig, e := initialInput.Resolve()
			testutil.Must(t, e)
			var pausedFrom, rotatedFrom string
			pauses, rotations := 0, 0
			fired := false
			lost := errors.New("lost provider rotation reply")
			credentials := []string{f.input.Providers["telegram"].TokenFile, f.input.Providers["telegram"].ChatFile}
			if signalOnly {
				credentials = nil
			}
			slices.Sort(credentials)
			l := deliveryProviderLifecycle{inspect: f.inspect, delivery: delivery, scopeInspect: delivery.scopeInspect}
			if limitsCase {
				l.kind = "limits"
			}
			if declarationCase {
				l.kind = "declaration"
			}
			if enrollmentCase {
				l.kind = "provider-enrollment"
			}
			if destinationCase {
				l.kind = "destination"
			}
			if routesCase {
				l.kind = "routes"
			}
			if derivedCase {
				l.kind = "derived-freshness"
			}
			if producerCase {
				l.kind = "producer"
			}
			stopped, nativeResumes := false, 0
			var nativeBefore map[string]string
			if nativeCase {
				nativeBefore = planTree(t, filepath.Join(f.paths.Control, "signal"))
				native := f.native.lifecycle
				native.gateway = f.inspect
				native.inspect = func(context.Context, install.FreshSignalSpec) (install.FreshSignalManifest, bool, error) {
					if stopped {
						return *f.service, false, install.FreshSignalResumeRequired{Reference: *f.o.Manifest.FreshSignal}
					}
					return *f.service, true, nil
				}
				native.resume = func(_ context.Context, ref install.FreshSignalReference) (install.FreshSignalManifest, error) {
					if ref != *f.o.Manifest.FreshSignal || !stopped {
						t.Fatal("unreviewed native restart")
					}
					stopped = false
					nativeResumes++
					return *f.service, nil
				}
				l.nativeResume = func(ctx context.Context, r ControlRequest, p ResolvedPaths) error {
					return resumeCompletedSignalChild(ctx, r, p, f.native.helperSource, f.native.nativeSource, native)
				}
			}
			l.inputs = func(_ context.Context, r ControlRequest, p ResolvedPaths, d *safefs.Directory, j OperationJournal) error {
				reviewed, err := retainedProviderReviewRequest(r, r.Desired, f.input)
				if err != nil {
					return err
				}
				if err := verifyLEDOutputInputs(reviewed, p, d, r.Desired); err != nil {
					return err
				}
				origin, err := requalificationEnrollment(d, j)
				if err != nil {
					return err
				}
				return verifyReservedConsumerInput(r, p, d, origin, f.input, f.reservation)
			}
			l.review = func(_ context.Context, root, hash string, next []byte) (install.DeliveryProviderReview, error) {
				if root != f.paths.Installation || hash != protocol.Hash(f.o.Manifest) {
					t.Fatal("review changed target")
				}
				var b, a install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &b, maxControlFrame))
				testutil.Must(t, safefs.DecodeJSON(next, &a, maxControlFrame))
				before, e := b.Resolve()
				testutil.Must(t, e)
				after, e := a.Resolve()
				testutil.Must(t, e)
				paused := f.o.Manifest
				paused.Mode, paused.DeliveryActivation = "verification", nil
				validate := install.ValidateDeliveryProviderRotation
				if limitsCase {
					validate = install.ValidatePausedDeliveryLimits
				}
				if declarationCase {
					validate = install.ValidatePausedDeliveryDeclaration
				}
				if enrollmentCase {
					validate = install.ValidatePausedDeliveryProviderEnrollment
				}
				if destinationCase {
					validate = install.ValidatePausedDeliveryDestination
				}
				if routesCase {
					validate = install.ValidatePausedDeliveryRoutes
				}
				if producerCase || derivedCase {
					validate = install.ValidatePausedDeliveryProducerPolicy
				}
				if e = validate(paused, before, after); e != nil {
					return install.DeliveryProviderReview{}, e
				}
				if telegram, ok := after.Providers["telegram"]; ok {
					credentials = append(credentials, telegram.TokenFile, telegram.ChatFile)
				}
				slices.Sort(credentials)
				credentials = slices.Compact(credentials)
				return install.DeliveryProviderReview{Before: f.o.Manifest, Credentials: slices.Clone(credentials)}, nil
			}
			l.inspectPause = func(_ context.Context, r install.DeliveryPauseRequest) (install.ManagedManifest, bool, error) {
				if f.o.Manifest.Mode == "delivery" {
					return f.o.Manifest, false, nil
				}
				if r.ManifestHash != pausedFrom {
					return f.o.Manifest, false, errors.New("wrong pause")
				}
				return f.o.Manifest, true, nil
			}
			l.pause = func(_ context.Context, r install.DeliveryPauseRequest) (install.ManagedManifest, error) {
				if r.ManifestHash != protocol.Hash(f.o.Manifest) {
					t.Fatal("unreviewed pause")
				}
				pausedFrom = r.ManifestHash
				f.o.Manifest.Mode, f.o.Manifest.DeliveryActivation = "verification", nil
				f.o.Manifest.UnitHash = protocol.Hash("paused unit")
				pauses++
				if boundary == "pause-reply" && !fired {
					fired = true
					return f.o.Manifest, lost
				}
				return f.o.Manifest, nil
			}
			l.inspectRotate = func(_ context.Context, s install.ManagedSpec, expected string) (install.ManagedManifest, bool, error) {
				if rotatedFrom == expected {
					return f.o.Manifest, true, nil
				}
				if expected != protocol.Hash(f.o.Manifest) {
					return f.o.Manifest, false, errors.New("wrong rotation")
				}
				return f.o.Manifest, false, nil
			}
			l.rotate = func(_ context.Context, s install.ManagedSpec, expected string) (install.ManagedManifest, error) {
				if expected != protocol.Hash(f.o.Manifest) {
					t.Fatal("changed rotation predecessor")
				}
				if !reflect.DeepEqual(s.FreshSignal, initial.FreshSignal) {
					t.Fatal("rotation changed native service reference")
				}
				rotatedFrom = expected
				var e error
				raw, e = readLimitsRuntime(s.RuntimeFile)
				testutil.Must(t, e)
				var input install.RuntimeInput
				testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
				c, e := input.Resolve()
				testutil.Must(t, e)
				f.o.Manifest.Revision = testutil.ID(t, "n")
				if policyCase {
					if f.o.Manifest.DeliveryOwnerConfigHash == "" {
						f.o.Manifest.DeliveryOwnerConfigHash = protocol.Hash(initialConfig.Worker)
					}
				}
				if !policyCase || destinationCase || enrollmentCase {
					f.o.Manifest.ReportCredentialHistory = &install.ManagedReportCredentialHistory{Operation: f.o.Manifest.Revision, Hash: protocol.Hash("synthetic credential history")}
				}
				f.o.Manifest.RuntimeHash, f.o.Manifest.ConfigHash = inputDigest(raw), protocol.Hash(c)
				f.o.Manifest.UnitHash = protocol.Hash("rotated unit")
				f.o.Manifest.Paths.Executable = filepath.Join(f.paths.Installation, "releases", f.o.Manifest.Revision, "deploy")
				f.o.Manifest.Paths.Config = filepath.Join(f.paths.Installation, "releases", f.o.Manifest.Revision, "runtime.json")
				rotations++
				if boundary == "rotate-reply" && !fired {
					fired = true
					return f.o.Manifest, lost
				}
				return f.o.Manifest, nil
			}
			l.scope = func(_ context.Context, _ string, r backup.VerificationNotificationScopeChange, paths []string, change func(backup.ProducerReservation) (string, error)) (string, error) {
				if !reflect.DeepEqual(paths, credentials) {
					t.Fatal("report exclusions changed")
				}
				if v, ok := receipts[r.Operation]; ok {
					return v, nil
				}
				v, e := change(f.reservation)
				if e != nil {
					return "", e
				}
				receipts[r.Operation] = v
				if boundary == "scope-reply" && !fired {
					fired = true
					return "", lost
				}
				return v, nil
			}
			if !policyCase || destinationCase || enrollmentCase {
				delete(f.r.Inputs, "token")
				delete(f.r.Inputs, "chat_id")
			}
			for cycle := 0; cycle < 2; cycle++ {
				if derivedCase {
					f.r.Desired.Backup.Freshness = fmt.Sprintf("%dh", 48+24*cycle)
				} else if declarationCase {
					p := initialConfig.Worker.Producers[0]
					f.r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {Required: &p.Required, Routes: slices.Clone(p.Routes), MaxMessages: p.MaxMessages, MaxSilenceSeconds: p.MaxSilenceSeconds, MaxEvidenceSeconds: p.MaxEvidenceSeconds, PolicyHealth: p.PolicyHealth, InteractiveContext: p.InteractiveContext, ContextKinds: slices.Clone(p.ContextKinds)}}
					if cycle == 1 {
						f.r.Desired.Notification.Producers = nil
					}
				} else if enrollmentCase && cycle == 1 {
					delete(f.r.Desired.Notification.Providers, "telegram")
				} else if routesCase {
					primary := "telegram"
					if signalOnly {
						primary = "signal"
					}
					f.r.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: primary}, fmt.Sprintf("audit%d", cycle): {Primary: primary}}
					if nativeCase && !signalOnly && cycle == 1 {
						f.r.Desired.Notification.Routes["alerts"] = config.NotificationRoute{Primary: "signal", Fallback: "telegram"}
					}
				} else if producerCase {
					p := initialConfig.Worker.Producers[0]
					f.r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {
						Required: &p.Required, Routes: slices.Clone(p.Routes), MaxMessages: p.MaxMessages + cycle + 1,
						MaxSilenceSeconds: p.MaxSilenceSeconds + 60*(cycle+1), MaxEvidenceSeconds: p.MaxEvidenceSeconds + 3600*(cycle+1),
						PolicyHealth: p.PolicyHealth, InteractiveContext: p.InteractiveContext, ContextKinds: slices.Clone(p.ContextKinds),
					}}
				} else if limitsCase {
					f.r.Desired.Notification.Limits.MaxMessages = 9000 - 1000*cycle
				} else {
					p := f.r.Desired.Notification.Providers["telegram"]
					p.TokenInput, p.ChatIDInput = "", ""
					p.TokenFile = filepath.Join(f.paths.Config, fmt.Sprintf("delivery-token-%d", cycle))
					p.ChatIDFile = filepath.Join(f.paths.Config, fmt.Sprintf("delivery-chat-%d", cycle))
					testutil.Must(t, os.WriteFile(p.TokenFile, []byte(fmt.Sprintf("123:new-token-%d", cycle)), 0600))
					chat := "-456"
					if destinationCase {
						chat = fmt.Sprintf("-%d", 900+cycle)
					}
					testutil.Must(t, os.WriteFile(p.ChatIDFile, []byte(chat), 0600))
					f.r.Desired.Notification.Providers["telegram"] = p
				}
				f.r.DesiredHash, err = f.r.Desired.NotificationFingerprint()
				testutil.Must(t, err)
				ready = false
				fired = false
				l.boundary = func(at string) error {
					if at == boundary && !fired {
						fired = true
						return lost
					}
					return nil
				}
				if initialResume {
					f.r.Action = "resume"
					stopped = true
					path := f.input.Providers["telegram"].TokenFile
					saved, e := os.ReadFile(path)
					testutil.Must(t, e)
					testutil.Must(t, os.WriteFile(path, []byte("123:unreviewed-before-intent"), 0600))
					tree := planTree(t, f.paths.Control)
					resumes := nativeResumes
					if _, _, e = reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", l); e == nil || resumes != nativeResumes || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
						t.Fatal("changed old input restarted native service before intent", e)
					}
					testutil.Must(t, os.WriteFile(path, saved, 0600))
				}
				if _, handled, e := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", l); !handled || !errors.Is(e, lost) {
					t.Fatal("missed provider boundary", handled, e)
				}
				l.boundary = nil
				if nativeCase {
					stopped = true
					f.r.Action = "resume"
					changed := f.r
					changed.SignalSHA256 = protocol.Hash("changed native artifact")
					retained := planTree(t, f.paths.Control)
					priorPauses, priorRotations, priorResumes := pauses, rotations, nativeResumes
					if _, _, e := reconcileDeliveryProviders(ctx, changed, f.paths, "/helper", l); e == nil || pauses != priorPauses || rotations != priorRotations || nativeResumes != priorResumes || !reflect.DeepEqual(retained, planTree(t, f.paths.Control)) {
						t.Fatal("changed Signal artifact resumed effects", e)
					}
					changed = f.r
					changed.Inputs = maps.Clone(f.r.Inputs)
					changed.Inputs["signal_recipient"] = []byte("+19995550101")
					if _, _, e := reconcileDeliveryProviders(ctx, changed, f.paths, "/helper", l); e == nil || nativeResumes != priorResumes || !reflect.DeepEqual(retained, planTree(t, f.paths.Control)) {
						t.Fatal("changed Signal recipient resumed effects", e)
					}
					pending := f.journal(t)
					intentPath := filepath.Join(f.paths.Control, "delivery-providers-"+pending.Children[0].Operation+".json")
					savedIntent, e := os.ReadFile(intentPath)
					testutil.Must(t, e)
					testutil.Must(t, os.WriteFile(intentPath, []byte("{}"), 0600))
					retained = planTree(t, f.paths.Control)
					if _, _, e = reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", l); e == nil || nativeResumes != priorResumes || !reflect.DeepEqual(retained, planTree(t, f.paths.Control)) {
						t.Fatal("damaged provider intent resumed native service", e)
					}
					testutil.Must(t, os.Remove(intentPath))
					retained = planTree(t, f.paths.Control)
					if _, _, e = reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", l); e == nil || nativeResumes != priorResumes || !reflect.DeepEqual(retained, planTree(t, f.paths.Control)) {
						t.Fatal("missing intent restarted native service", e)
					}
					testutil.Must(t, os.WriteFile(intentPath, savedIntent, 0600))
				}
				if !signalOnly {
					oldPath := f.input.Providers["telegram"].TokenFile
					saved, e := os.ReadFile(oldPath)
					testutil.Must(t, e)
					testutil.Must(t, os.WriteFile(oldPath, []byte("123:unreviewed-original"), 0600))
					tree := planTree(t, f.paths.Control)
					priorPauses, priorRotations, priorResumes := pauses, rotations, nativeResumes
					if _, _, e = reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", l); e == nil || pauses != priorPauses || rotations != priorRotations || nativeResumes != priorResumes || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
						t.Fatal("changed original input resumed domain effects", e)
					}
					testutil.Must(t, os.WriteFile(oldPath, saved, 0600))
				}
				for range 2 {
					got, handled, e := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", l)
					wantState := "needs_qualification"
					if declarationCase {
						wantState = "reporting_connected"
					}
					if !handled || e != nil || got.State != wantState {
						t.Fatal("rotation resume", got, handled, e)
					}
				}
				wantEffects := cycle + 1
				if declarationCase {
					wantEffects = 0
				}
				if pauses != wantEffects || rotations != wantEffects {
					t.Fatal("rotation retry replayed gateway changes", pauses, rotations)
				}
				j := f.journal(t)
				testutil.Must(t, verifyDeliveryProviderReceipt(f.d, j, f.o.Manifest, l.scopeInspect))
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, j, f.input, f.reservation))
				f.r.Action = "apply"
				if declarationCase {
					if protocol.Hash(f.o.Manifest) != protocol.Hash(initial) {
						t.Fatal("declaration changed delivery or qualification")
					}
					continue
				}
				ready = true
				got, handled, e := reconcileDeliveryProviders(ctx, f.r, f.paths, "/helper", l)
				if !handled || e != nil || got.State != "delivery_admitted" && got.State != "reporting_connected" {
					t.Fatal("fresh qualification did not admit rotation", got, e)
				}
				origin, e := requalificationEnrollment(f.d, f.journal(t))
				testutil.Must(t, e)
				testutil.Must(t, verifyReservedConsumerInput(f.r, f.paths, f.d, origin, f.input, f.reservation))
				if f.o.Manifest.Identity != initial.Identity || f.o.Manifest.DeliveryOwnerHash != initial.DeliveryOwnerHash {
					t.Fatal("rotation replaced delivery authority")
				}
			}
			expectedResumes := 2
			if initialResume {
				expectedResumes = 4
			}
			if nativeCase && (nativeResumes != expectedResumes || !reflect.DeepEqual(nativeBefore, planTree(t, filepath.Join(f.paths.Control, "signal")))) {
				t.Fatal("rotation changed native authority or replayed restart", nativeResumes)
			}
			retained, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			if string(retained) != string(enrollment) {
				t.Fatal("rotation replaced enrollment")
			}
			current, err := readLimitsRuntime(f.paths.Runtime)
			testutil.Must(t, err)
			if string(current) != string(bootstrap) {
				t.Fatal("rotation overwrote original private bootstrap")
			}
		})
	}
}

func TestDeliveryProducerPolicyCannotRemoveActiveBackupRoute(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	f := newMappingFixtureWithOptions(t, mappingFixtureOptions{extraRoutes: true}, false, true, false, false, true)
	prior := f.journal(t)
	raw, err := retainedEnrolledRuntime(f.d, f.paths, prior)
	testutil.Must(t, err)
	var input install.RuntimeInput
	testutil.Must(t, safefs.DecodeJSON(raw, &input, maxControlFrame))
	f.r.Desired.Notification.Producers = map[string]config.ProducerDeclaration{"backup-worker": {Routes: []string{"audit"}, MaxMessages: 1000, MaxSilenceSeconds: 300, MaxEvidenceSeconds: 172800, PolicyHealth: true}}
	before := planTree(t, f.paths.Control)
	if _, _, _, err = compileDeliveryLimits(f.r, f.paths, f.d, prior, input, raw, f.reservation); err == nil {
		t.Fatal("removed active reporting route")
	}
	if !reflect.DeepEqual(before, planTree(t, f.paths.Control)) {
		t.Fatal("invalid route retained policy intent")
	}
}
