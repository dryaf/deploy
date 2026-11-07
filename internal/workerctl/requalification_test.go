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
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestRequalificationResumesPauseAndRenewsDeliveryWithoutEnrollment(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, boundary := range []string{"requalification-intent", "requalification-domain-complete", "pause-reply", "native:requalification-intent", "native:requalification-domain-complete", "native:pause-reply", "led:requalification-intent", "led:requalification-domain-complete", "led:pause-reply"} {
		t.Run(boundary, func(t *testing.T) {
			ledCase := strings.HasPrefix(boundary, "led:")
			boundary = strings.TrimPrefix(boundary, "led:")
			nativeCase := strings.HasPrefix(boundary, "native:")
			boundary = strings.TrimPrefix(boundary, "native:")
			f := newMappingFixture(t, false, true, nativeCase, false, !ledCase)
			var nativeBefore map[string]string
			if nativeCase {
				nativeBefore = planTree(t, filepath.Join(f.paths.Control, "signal"))
			}
			ctx := context.Background()
			if ledCase {
				_, err := applyEnrolledLEDBindings(ctx, f.r, f.paths, "/helper", f.d, f.journal(t), f.input, f.reservation, f.lifecycle(t))
				testutil.Must(t, err)
				raw, err := retainedEnrolledRuntime(f.d, f.paths, f.journal(t))
				testutil.Must(t, err)
				testutil.Must(t, safefs.DecodeJSON(raw, &f.input, maxControlFrame))
			}
			original := f.journal(t)
			enrollment, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			ready := true
			var request install.DeliveryAdmissionRequest
			activations, pauses, connections := 0, 0, 0
			outputReceipts := map[string]string{}
			delivery := notificationDeliveryLifecycle{
				scopeInspect: func(_, op, _, _ string) (string, bool, error) {
					receipt, ok := outputReceipts[op]
					return receipt, ok, nil
				},
				review: func(_ context.Context, root, hash string) (install.DeliveryAdmissionRequest, bool, error) {
					if root != f.paths.Installation || hash != protocol.Hash(f.o.Manifest) {
						t.Fatal("changed delivery review")
					}
					request = install.DeliveryAdmissionRequest{Root: root, ManifestHash: hash, QualificationHash: protocol.Hash(testutil.ID(t, "n"))}
					return request, ready, nil
				},
				inspect: func(_ context.Context, got install.DeliveryAdmissionRequest) (install.ManagedManifest, bool, error) {
					if got != request {
						t.Fatal("selected a different qualification")
					}
					return f.o.Manifest, f.o.Manifest.Mode == "delivery", nil
				},
				ensure: func(_ context.Context, got install.DeliveryAdmissionRequest) (install.ManagedManifest, error) {
					if got != request {
						t.Fatal("unreviewed admission")
					}
					f.o.Manifest.Mode = "delivery"
					f.o.Manifest.DeliveryOwnerHash = protocol.Hash("unchanged owner")
					f.o.Manifest.DeliveryActivation = &install.ManagedDeliveryActivation{Operation: testutil.ID(t, "n"), Hash: protocol.Hash(got)}
					f.o.Manifest.UnitHash = protocol.Hash("delivery unit")
					activations++
					return f.o.Manifest, nil
				},
				backupStatus: func(context.Context, string) (backup.SetupObservation, error) {
					hash, _ := f.r.Desired.Fingerprint()
					return backup.SetupObservation{Stage: "complete", DesiredHash: hash}, nil
				},
				connect: func(_ context.Context, home, hash, receipt string) error {
					if receipt != protocol.Hash(f.o.Manifest) {
						t.Fatal("wrong report binding")
					}
					connections++
					return nil
				},
			}
			_, err = advanceNotificationDelivery(ctx, f.r, f.paths, f.d, original, ControlResult{}, delivery)
			testutil.Must(t, err)
			var pausedFrom string
			l := requalificationLifecycle{inspect: f.inspect, delivery: delivery}
			stopped, nativeResumes := false, 0
			if nativeCase {
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
				if err := verifyLEDOutputInputs(r, p, d, r.Desired); err != nil {
					return err
				}
				prior, err := requalificationEnrollment(d, j)
				if err != nil {
					return err
				}
				return verifyReservedConsumerInput(r, p, d, prior, f.input, f.reservation)
			}
			l.pause = func(_ context.Context, r install.DeliveryPauseRequest) (install.ManagedManifest, error) {
				if r.Root != f.paths.Installation || r.ManifestHash != protocol.Hash(f.o.Manifest) {
					t.Fatal("unreviewed pause")
				}
				pausedFrom = r.ManifestHash
				f.o.Manifest.Mode, f.o.Manifest.DeliveryActivation = "verification", nil
				f.o.Manifest.UnitHash = protocol.Hash("verification unit")
				pauses++
				if boundary == "pause-reply" {
					return f.o.Manifest, errors.New("lost pause reply")
				}
				return f.o.Manifest, nil
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
			for cycle := 0; cycle < 2; cycle++ {
				if ledCase {
					toggleRequalificationLEDOutput(t, f, cycle == 1, outputReceipts)
				}
				before := f.o.Manifest
				ready = false
				r := f.r
				r.Action = "qualify-begin"
				l.boundary = func(at string) error {
					if at == boundary {
						return errors.New("interrupted pause")
					}
					return nil
				}
				if _, handled, err := reconcileRequalification(ctx, r, f.paths, l); !handled || err == nil {
					t.Fatal("missed interruption", err)
				}
				l.boundary = nil
				if nativeCase {
					changed := r
					changed.SignalSHA256 = protocol.Hash("different native artifact")
					retained := planTree(t, f.paths.Control)
					priorPauses := pauses
					if _, _, err = reconcileRequalification(ctx, changed, f.paths, l); err == nil || pauses != priorPauses || !reflect.DeepEqual(retained, planTree(t, f.paths.Control)) {
						t.Fatal("changed Signal artifact accepted during renewal", err)
					}
				}
				if nativeCase {
					stopped = true
					r.Action = "resume"
				}
				for range 2 {
					got, handled, err := reconcileRequalification(ctx, r, f.paths, l)
					if err != nil || !handled || got.State != "needs_qualification" || pauses != cycle+1 {
						t.Fatal("pause resume", got, err, pauses)
					}
				}
				j := f.journal(t)
				origin, err := requalificationEnrollment(f.d, j)
				testutil.Must(t, err)
				if origin.Digest() != original.Digest() {
					t.Fatal("renewal lost original enrollment")
				}
				testutil.Must(t, validateRequalificationManifest(before, f.o.Manifest))
				var got ControlResult
				if ledCase {
					testutil.Must(t, verifyMappingScope(f.d, j, delivery.scopeInspect))
					for op, receipt := range outputReceipts {
						delete(outputReceipts, op)
						if verifyMappingScope(f.d, j, delivery.scopeInspect) == nil {
							t.Fatal("renewal ignored earlier backup output receipt")
						}
						outputReceipts[op] = receipt
					}

				} else {
					got, err = planNotificationControl(ctx, r, f.paths, f.inspect)
					if err != nil || got.State != "up_to_date" {
						t.Fatal("plan", got, err)
					}
					got, err = inspectNotificationControl(ctx, r, f.paths, f.inspect, nil)
					if err != nil || got.State != "needs_qualification" {
						t.Fatal("status", got, err)
					}
				}
				r.Action = "qualify-begin"
				if !ledCase {
					session := install.QualificationSession{ID: testutil.ID(t, "n")}
					_, err = qualifyNotificationControl(ctx, r, f.paths, qualificationLifecycle{begin: func(_ context.Context, root, hash string) (install.QualificationSession, error) {
						if hash != protocol.Hash(f.o.Manifest) {
							t.Fatal("qualification bound old manifest")
						}
						return session, nil
					}})
					testutil.Must(t, err)
				}
				r.Action = "apply"
				got, handled, err := reconcileRequalification(ctx, r, f.paths, l)
				if err != nil || !handled || got.State != "needs_qualification" || activations != cycle+1 {
					t.Fatal("unqualified apply", got, err)
				}
				path := filepath.Join(f.paths.Control, "requalification-"+j.Children[0].Operation+".json")
				proof, err := os.ReadFile(path)
				testutil.Must(t, err)
				testutil.Must(t, os.Remove(path))
				tree := planTree(t, f.paths.Control)
				if _, _, err = reconcileRequalification(ctx, r, f.paths, l); err == nil || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
					t.Fatal("lost proof recreated", err)
				}
				if _, err = planNotificationControl(ctx, r, f.paths, f.inspect); err == nil {
					t.Fatal("plan ignored missing pause proof")
				}
				if _, err = inspectNotificationControl(ctx, r, f.paths, f.inspect, nil); err == nil {
					t.Fatal("status ignored missing pause proof")
				}
				testutil.Must(t, os.WriteFile(path, proof, 0600))
				ready = true
				review := l.delivery.review
				l.delivery.review = func(_ context.Context, root, hash string) (install.DeliveryAdmissionRequest, bool, error) {
					return install.DeliveryAdmissionRequest{Root: root, ManifestHash: hash, QualificationHash: j.Pins.Artifacts["qualification"]}, true, nil
				}
				got, handled, err = reconcileRequalification(ctx, r, f.paths, l)
				if err != nil || !handled || got.State != "needs_qualification" || activations != cycle+1 {
					t.Fatal("old observations renewed delivery", got, err)
				}
				l.delivery.review = review
				got, handled, err = reconcileRequalification(ctx, r, f.paths, l)
				if err != nil || !handled || got.State != "reporting_connected" || activations != cycle+2 {
					t.Fatal("renewed delivery", got, err, activations)
				}
				if f.o.Manifest.Identity != before.Identity || f.o.Manifest.DeliveryOwnerHash != before.DeliveryOwnerHash {
					t.Fatal("renewal changed owner")
				}
			}
			retained, err := safefs.ReadFile(f.d, "consumer-enrollment.json", maxControlFrame)
			testutil.Must(t, err)
			if nativeCase && (nativeResumes != 2 || !reflect.DeepEqual(nativeBefore, planTree(t, filepath.Join(f.paths.Control, "signal")))) {
				t.Fatal("renewal changed native account history")
			}
			if !reflect.DeepEqual(enrollment, retained) || connections != 3 {
				t.Fatal("renewal rewrote enrollment or lost reporting")
			}
		})
	}
}

func TestRequalificationRejectsChangedAuthorityBeforePausing(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless coordinator")
	}
	for _, damage := range []string{"yaml", "helper", "path", "pending", "commands", "command-history", "recovered", "legacy-signal", "inputs", "receipt"} {
		t.Run(damage, func(t *testing.T) {
			f := newMappingFixture(t, false, true, false, false, true)
			original := f.journal(t)
			testutil.Must(t, f.d.Mkdir("history"))
			testutil.Must(t, f.d.WriteJSON("history/"+original.ID+".json", original, false))
			active := f.o.Manifest
			active.Mode = "delivery"
			active.UnitHash = protocol.Hash("delivery unit")
			active.DeliveryOwnerHash = protocol.Hash("owner")
			active.DeliveryActivation = &install.ManagedDeliveryActivation{Operation: testutil.ID(t, "n"), Hash: protocol.Hash("activation")}
			switch damage {
			case "commands":
				active.Commands = &install.ManagedCommandAdmission{}
			case "command-history":
				active.CommandHistoryHash = protocol.Hash("retained commands")
			case "recovered":
				active.RecoveryCommitHash = protocol.Hash("recovery")
			case "legacy-signal":
				active.SignalVerification = &install.ManagedSignalBinding{}
			}
			f.o.Manifest = active
			current := original
			current.ID = testutil.ID(t, "n")
			current.Pins.Previous, current.Pins.PreviousOperation = original.Digest(), original.ID
			current.Children = []ChildReference{{Name: "notification-delivery", Operation: testutil.ID(t, "n"), Complete: true, Receipt: protocol.Hash(active)}}
			if damage == "pending" {
				current.Complete = false
				current.Children[0].Complete = false
				current.Children[0].Receipt = ""
			}
			if damage == "receipt" {
				current.Children[0].Receipt = protocol.Hash("another manifest")
			}
			testutil.Must(t, f.d.WriteJSON("operation.json", current, true))
			r := f.r
			r.Action = "qualify-begin"
			switch damage {
			case "yaml":
				r.DesiredHash = protocol.Hash("different YAML")
			case "helper":
				r.HelperSHA256 = protocol.Hash("different helper")
			case "path":
				f.paths.Installation += ".other"
			}
			l := requalificationLifecycle{inspect: f.inspect, inputs: func(context.Context, ControlRequest, ResolvedPaths, *safefs.Directory, OperationJournal) error {
				if damage == "inputs" {
					return errors.New("changed private inputs")
				}
				return nil
			}, pause: func(context.Context, install.DeliveryPauseRequest) (install.ManagedManifest, error) {
				t.Fatal("unreviewed pause")
				return active, nil
			}}
			tree := planTree(t, f.paths.Control)
			if _, handled, err := reconcileRequalification(context.Background(), r, f.paths, l); !handled || err == nil {
				t.Fatal("invalid requalification accepted", err)
			}
			if !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
				t.Fatal("rejected requalification changed authority")
			}
		})
	}
}
