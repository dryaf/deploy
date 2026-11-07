package workerctl

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestCommandRequalificationResumesWithoutAdmittingIntake(t *testing.T) {
	for _, mode := range []string{"read", "mutation"} {
		for _, boundary := range []string{"requalification-intent", "pause-reply", "requalification-domain-complete"} {
			t.Run(mode+"/"+boundary, func(t *testing.T) {
				ctx := context.Background()
				f := newCommandActivityFixture(t, mode)
				originalEnsure := f.lifecycle.ensure
				f.lifecycle.ensure = func(ctx context.Context, r install.CommandPolicyRequest) (install.ManagedManifest, error) {
					m, err := originalEnsure(ctx, r)
					if err != nil {
						return m, err
					}
					m.DeliveryOwnerHash = protocol.Hash("retained delivery owner")
					m.CommandHistoryHash = m.Commands.Hash
					f.manifest = m
					f.requests[protocol.Hash(r)] = m
					return m, nil
				}
				f.toggle(false)
				_, _, err := reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
				testutil.Must(t, err)
				ready, fired := false, false
				lostDelivery := false
				pauses, renewals := 0, 0
				paused := map[string]install.ManagedManifest{}
				admitted := map[string]install.ManagedManifest{}
				l := commandRequalificationLifecycle{commands: f.lifecycle}
				l.inspect = func(context.Context, string) (install.InstallationObservation, error) {
					return install.InstallationObservation{Phase: "complete", Manifest: f.manifest}, nil
				}
				l.inputs = func(_ context.Context, r ControlRequest, p ResolvedPaths, d *safefs.Directory, _ OperationJournal) error {
					return f.lifecycle.inputs(r, p, d, config.DesiredWorkers{})
				}
				l.boundary = func(at string) error {
					if at == boundary && !fired {
						fired = true
						return errors.New("lost coordinator reply")
					}
					return nil
				}
				l.pause = func(_ context.Context, r install.DeliveryPauseRequest) (install.ManagedManifest, error) {
					if r.ManifestHash != protocol.Hash(f.manifest) {
						t.Fatal("unreviewed pause")
					}
					f.manifest.Mode, f.manifest.DeliveryActivation, f.manifest.Commands = "verification", nil, nil
					f.manifest.UnitHash = protocol.Hash("verification unit")
					paused[r.ManifestHash] = f.manifest
					pauses++
					if boundary == "pause-reply" && !fired {
						fired = true
						return f.manifest, errors.New("lost domain reply")
					}
					return f.manifest, nil
				}
				l.inspectPause = func(_ context.Context, r install.DeliveryPauseRequest) (install.ManagedManifest, bool, error) {
					m, ok := paused[r.ManifestHash]
					return m, ok, nil
				}
				l.delivery.review = func(_ context.Context, root, hash string) (install.DeliveryAdmissionRequest, bool, error) {
					if hash != protocol.Hash(f.manifest) || f.manifest.Mode != "verification" {
						t.Fatal("wrong paused manifest")
					}
					return install.DeliveryAdmissionRequest{Root: root, ManifestHash: hash, QualificationHash: protocol.Hash(fmt.Sprint("fresh", renewals))}, ready, nil
				}
				l.delivery.inspect = func(_ context.Context, r install.DeliveryAdmissionRequest) (install.ManagedManifest, bool, error) {
					m, ok := admitted[protocol.Hash(r)]
					return m, ok, nil
				}
				l.delivery.ensure = func(_ context.Context, r install.DeliveryAdmissionRequest) (install.ManagedManifest, error) {
					if r.ManifestHash != protocol.Hash(f.manifest) {
						t.Fatal("replayed activation")
					}
					f.manifest.Mode = "delivery"
					f.manifest.DeliveryActivation = &install.ManagedDeliveryActivation{Operation: testutil.ID(t, "n"), Hash: protocol.Hash(r)}
					f.manifest.UnitHash = protocol.Hash("delivery unit")
					admitted[protocol.Hash(r)] = f.manifest
					renewals++
					if lostDelivery {
						lostDelivery = false
						return f.manifest, errors.New("lost renewal reply")
					}
					return f.manifest, nil
				}
				l.delivery.connect = f.lifecycle.connect
				for cycle := 0; cycle < 2; cycle++ {
					ready, fired = false, false
					f.r.Action = "qualify-begin"
					_, handled, err := reconcileCommandRequalification(ctx, f.r, f.paths, l)
					if !handled || err == nil || !fired {
						t.Fatal("missed interruption", handled, err)
					}
					retained := f.journal(t)
					changed := f.r
					changed.HelperSHA256 = protocol.Hash("different helper")
					if _, _, err = reconcileCommandRequalification(ctx, changed, f.paths, l); err == nil || retained.Digest() != f.journal(t).Digest() {
						t.Fatal("retargeted renewal")
					}
					changedBackup := f.r
					backupPolicy := *changedBackup.Desired.Backup
					backupPolicy.Schedule = "weekly"
					changedBackup.Desired.Backup = &backupPolicy
					if _, _, err = reconcileCommandRequalification(ctx, changedBackup, f.paths, l); err == nil || retained.Digest() != f.journal(t).Digest() {
						t.Fatal("pending pause accepted changed backup YAML")
					}
					for range 2 {
						_, handled, err = reconcileCommandRequalification(ctx, f.r, f.paths, l)
						if !handled {
							t.Fatal("unhandled pause")
						}
						testutil.Must(t, err)
					}
					f.r.Action = "apply"
					result, _, err := reconcileCommandRequalification(ctx, f.r, f.paths, l)
					testutil.Must(t, err)
					if result.State != "needs_qualification" || renewals != cycle {
						t.Fatal("invented qualification", result)
					}
					ready, lostDelivery = true, true
					_, handled, err = reconcileCommandRequalification(ctx, f.r, f.paths, l)
					if !handled || err == nil || lostDelivery {
						t.Fatal("missed renewal completion interruption", handled, err)
					}
					retained = f.journal(t)
					changedBackup.Action = "apply"
					if _, _, err = reconcileCommandRequalification(ctx, changedBackup, f.paths, l); err == nil || retained.Digest() != f.journal(t).Digest() {
						t.Fatal("pending renewal accepted changed backup YAML")
					}

					for range 2 {
						result, handled, err = reconcileCommandRequalification(ctx, f.r, f.paths, l)
						if err != nil || !handled || result.State != "commands_disabled" {
							t.Fatal(result, handled, err)
						}
						testutil.Must(t, verifyCommandRenewalReceipt(ctx, f.d, f.journal(t), f.manifest, l))
					}
					if pauses != cycle+1 || renewals != cycle+1 || f.manifest.Commands != nil {
						t.Fatal("replayed effects or admitted intake", pauses, renewals)
					}
					before := f.journal(t)
					tree := planTree(t, f.paths.Control)
					receiver, err := retainedCommandReceiver(ctx, f.d, before, f.lifecycle.notificationCommandLifecycle)
					testutil.Must(t, err)
					if !reflect.DeepEqual(receiver, f.receiver) || !reflect.DeepEqual(tree, planTree(t, f.paths.Control)) {
						t.Fatal("renewal replaced receiver")
					}
					f.toggle(true)
					result, handled, err = reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
					if err != nil || !handled || result.State != "commands_admitted" {
						t.Fatal(result, handled, err)
					}
					f.toggle(false)
					_, _, err = reconcileCommandActivity(ctx, f.r, f.paths, f.lifecycle)
					testutil.Must(t, err)
				}
			})
		}
	}
}
