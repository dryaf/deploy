package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func sameDestinationProviderInput(t *testing.T, spec ManagedSpec) RuntimeInput {
	t.Helper()
	raw, err := os.ReadFile(spec.RuntimeFile)
	testutil.Must(t, err)
	var input RuntimeInput
	testutil.Must(t, json.Unmarshal(raw, &input))
	p := input.Providers["telegram"]
	token, err := provider.ReadSecret(p.TokenFile)
	testutil.Must(t, err)
	chat, err := provider.ReadSecret(p.ChatFile)
	testutil.Must(t, err)
	p.TokenFile = filepath.Join(filepath.Dir(spec.RuntimeFile), "rotated-token")
	p.ChatFile = filepath.Join(filepath.Dir(spec.RuntimeFile), "rotated-chat")
	testutil.Must(t, os.WriteFile(p.TokenFile, []byte(token+"new"), 0600))
	testutil.Must(t, os.WriteFile(p.ChatFile, []byte(chat), 0600))
	input.Providers["telegram"] = p
	return input
}

func qualifyPausedDirectDelivery(t *testing.T, m ManagedManifest) DeliveryAdmissionRequest {
	t.Helper()
	ctx := context.Background()
	session, err := BeginQualification(ctx, m.Root, protocol.Hash(m))
	testutil.Must(t, err)
	if session.Recorded || session.EvidenceComplete {
		t.Fatal("rotation reused old observations")
	}
	for _, check := range session.Checks {
		_, err = ObserveQualification(ctx, m.Root, protocol.Hash(m), QualificationObservation{Session: session.ID, Scope: check.Scope, ID: check.ID, Result: "passed", EvidenceHash: protocol.Hash("synthetic offline observation")})
		testutil.Must(t, err)
	}
	_, err = CommitQualification(ctx, m.Root, protocol.Hash(m), session.ID)
	testutil.Must(t, err)
	review, ready, err := ReviewDeliveryAdmission(ctx, m.Root, protocol.Hash(m))
	testutil.Must(t, err)
	if !ready {
		t.Fatal("fixture observations did not qualify")
	}
	return review
}

func TestPausedDeliveryProviderRotationRetainsActiveQueueAndReporting(t *testing.T) {
	for _, boundary := range []string{"prepared", "stop-returned", "files", "published", "start-returned", "complete", "native:prepared", "native:published", "native:start-returned", "native:complete"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			nativeCase := strings.HasPrefix(boundary, "native:")
			boundary = strings.TrimPrefix(boundary, "native:")
			var spec ManagedSpec
			var i managedInstaller
			var services *fakeDeliveryServices
			var c Runtime
			var before ManagedManifest
			var qualification string
			var err error
			retainedNative := map[string][]byte{}
			if nativeCase {
				var native FreshSignalManifest
				spec, i, c, native = freshSignalGatewayFixture(t, false, true)
				services = i.services.(*fakeDeliveryServices)
				before, err = i.ensureVerification(ctx, spec)
				testutil.Must(t, err)
				qualification = qualifyPausedDirectDelivery(t, before).QualificationHash
				for _, root := range []string{native.Spec.Root, native.Spec.AccountState, native.Spec.Inbox.State} {
					testutil.Must(t, filepath.WalkDir(root, func(path string, e os.DirEntry, err error) error {
						if err != nil || !e.Type().IsRegular() {
							return err
						}
						retainedNative[path], err = os.ReadFile(path)
						return err
					}))
				}
			} else {
				var q QualificationRecord
				spec, i, services, c, before, q = deliveryFixture(t)
				qualification = protocol.Hash(q)
			}
			active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), qualification, false)
			testutil.Must(t, err)
			s, err := openManagedStore(active, c)
			testutil.Must(t, err)
			producer := c.Worker.Producers[0].Producer
			testutil.Must(t, s.Activate(producer, "one"))
			now := time.Now().UTC()
			msg := protocol.Message{Producer: producer, Sequence: 1, ID: "retained-before-rotation", Generation: "one", Subject: "subject", Kind: "report", Severity: "info", ObservedAt: now, ExpiresAt: now.Add(time.Minute), Text: "retained report", Route: "ops", ReportKey: "day", ReportType: "daily", ReportRevision: 1, BasisRevision: 1}
			queued, err := s.Enqueue(ctx, msg)
			testutil.Must(t, err)
			testutil.Must(t, s.Close())
			originalProfile, err := InspectManagedReportProfile(spec.Root, active.Identity, producer, "ops")
			testutil.Must(t, err)
			paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(active), "", false)
			testutil.Must(t, err)
			input := sameDestinationProviderInput(t, spec)
			raw, err := input.Bytes()
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
			next, err := input.Resolve()
			testutil.Must(t, err)
			lost := errors.New("lost rotation reply")
			i.fault = func(at string) error {
				if at == boundary {
					return lost
				}
				return nil
			}
			if _, err = i.rotatePausedDeliveryProvider(ctx, spec, protocol.Hash(paused)); !errors.Is(err, lost) {
				t.Fatal("missed rotation boundary", err)
			}
			i.fault = nil
			inspectedStarts, inspectedStops := services.starts, services.stops
			_, inspected, err := i.inspectPausedDeliveryProvider(ctx, spec, protocol.Hash(paused))
			testutil.Must(t, err)
			if inspected != (boundary == "complete") || inspectedStarts != services.starts || inspectedStops != services.stops {
				t.Fatal("inspection replayed an incomplete provider rotation")
			}
			testutil.Must(t, os.WriteFile(spec.RuntimeFile, append(append([]byte(nil), raw...), '\n'), 0600))
			if _, err = i.rotatePausedDeliveryProvider(ctx, spec, protocol.Hash(paused)); err == nil {
				t.Fatal("changed retry runtime accepted")
			}
			testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
			rotated, err := i.rotatePausedDeliveryProvider(ctx, spec, protocol.Hash(paused))
			testutil.Must(t, err)
			starts, stops := services.starts, services.stops
			again, err := i.rotatePausedDeliveryProvider(ctx, spec, protocol.Hash(paused))
			testutil.Must(t, err)
			inspectedManifest, complete, err := i.inspectPausedDeliveryProvider(ctx, spec, protocol.Hash(paused))
			testutil.Must(t, err)
			if !complete || protocol.Hash(inspectedManifest) != protocol.Hash(rotated) {
				t.Fatal("completed rotation inspection lost receipt")
			}
			if protocol.Hash(again) != protocol.Hash(rotated) || starts != services.starts || stops != services.stops {
				t.Fatal("completed rotation replayed service effects")
			}
			if rotated.Mode != "verification" || rotated.Identity != active.Identity || rotated.DeliveryOwnerHash != active.DeliveryOwnerHash || rotated.ConfigHash == paused.ConfigHash || protocol.Hash(c.Worker) != protocol.Hash(next.Worker) {
				t.Fatal("rotation changed delivery authority")
			}
			s, err = openManagedStore(rotated, next)
			testutil.Must(t, err)
			retained, err := s.Get(ctx, producer, msg.ID)
			testutil.Must(t, err)
			status, err := s.OwnershipStatus(producer, "one")
			testutil.Must(t, err)
			testutil.Must(t, s.Close())
			if !status.Active || protocol.Hash(retained) != protocol.Hash(queued) {
				t.Fatal("rotation changed queue or producer")
			}
			profile, err := InspectManagedReportProfile(spec.Root, active.Identity, producer, "ops")
			testutil.Must(t, err)
			for _, credential := range []string{c.Providers["telegram"].TokenFile, c.Providers["telegram"].ChatFile, next.Providers["telegram"].TokenFile, next.Providers["telegram"].ChatFile} {
				if !slices.Contains(profile.Credentials, credential) {
					t.Fatal("rotation dropped a reporting credential exclusion")
				}
			}
			review := qualifyPausedDirectDelivery(t, rotated)
			renewed, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(rotated), review.QualificationHash, false)
			testutil.Must(t, err)
			_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(renewed), protocol.Hash(active))
			testutil.Must(t, err)
			for _, captured := range []ManagedReportProfile{originalProfile, profile} {
				_, err = InspectCapturedReportProfile(ctx, spec.Root, protocol.Hash(renewed), captured)
				testutil.Must(t, err)
				captured.Credentials = append(slices.Clone(captured.Credentials), filepath.Join(filepath.Dir(spec.RuntimeFile), "forged-exclusion"))
				if _, err = InspectCapturedReportProfile(ctx, spec.Root, protocol.Hash(renewed), captured); err == nil {
					t.Fatal("forged historical report exclusion accepted")
				}
			}
			_, err = InspectCapturedDeliveryContinuation(ctx, spec.Root, protocol.Hash(renewed), protocol.Hash(rotated), protocol.Hash(active))
			testutil.Must(t, err)
			if _, err = InspectCapturedDeliveryContinuation(ctx, spec.Root, protocol.Hash(renewed), protocol.Hash(active), protocol.Hash(rotated)); err == nil {
				t.Fatal("reversed provider snapshot accepted")
			}
			// A later ordinary renewal retains the credential transition as ancestry.
			pausedAgain, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(renewed), "", false)
			testutil.Must(t, err)
			review = qualifyPausedDirectDelivery(t, pausedAgain)
			latest, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(pausedAgain), review.QualificationHash, false)
			testutil.Must(t, err)
			_, err = InspectCommandDeliveryContinuation(ctx, spec.Root, protocol.Hash(latest), protocol.Hash(active))
			testutil.Must(t, err)
			for _, captured := range []ManagedReportProfile{originalProfile, profile} {
				_, err = InspectCapturedReportProfile(ctx, spec.Root, protocol.Hash(latest), captured)
				testutil.Must(t, err)
			}
			if protocol.Hash(latest.FreshSignal) != protocol.Hash(active.FreshSignal) {
				t.Fatal("rotation replaced native service authority")
			}
			for path, body := range retainedNative {
				actual, err := os.ReadFile(path)
				testutil.Must(t, err)
				if string(actual) != string(body) {
					t.Fatal("rotation or renewal changed native account history", path)
				}
			}
		})
	}
}

func TestPausedDeliveryProviderRotationRejectsChangedAuthorityBeforeEffects(t *testing.T) {
	for _, damage := range []string{"destination", "producer-policy", "helper", "pause", "capability-service", "capability-helper"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			spec, i, services, _, before, q := deliveryFixture(t)
			active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(q), false)
			testutil.Must(t, err)
			operation := "pause-delivery"
			if damage == "pause" {
				operation = "deactivate-delivery"
			}
			paused, err := i.prepareDelivery(ctx, operation, spec.Root, protocol.Hash(active), "", false)
			testutil.Must(t, err)
			input := sameDestinationProviderInput(t, spec)
			switch damage {
			case "destination":
				p := input.Providers["telegram"]
				testutil.Must(t, os.WriteFile(p.ChatFile, []byte("999"), 0600))
				token, err := provider.ReadSecret(p.TokenFile)
				testutil.Must(t, err)
				p.Destination = provider.TelegramIdentity(token, "999")
				input.Providers["telegram"] = p
			case "producer-policy":
				input.Worker.Producers[0].MaxMessages++
			case "helper":
				spec.BinarySHA256 = protocol.Hash("another helper")
			case "capability-service", "capability-helper":
				calls := 0
				i.deliveryProvidersProbe = func(context.Context, *safefs.Directory, string, string) error {
					calls++
					if damage == "capability-service" || calls == 2 {
						return errors.New("old helper")
					}
					return nil
				}
			}
			raw, err := input.Bytes()
			testutil.Must(t, err)
			testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
			retained := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
			starts, stops := services.starts, services.stops
			if _, err = i.rotatePausedDeliveryProvider(ctx, spec, protocol.Hash(paused)); err == nil {
				t.Fatal("unsafe rotation accepted")
			}
			if retained != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) || starts != services.starts || stops != services.stops {
				t.Fatal("refusal retained operation or changed service")
			}
		})
	}
}

func TestDeliveryProviderReviewRejectsDriftAndOldHelpersWithoutEffects(t *testing.T) {
	for _, damage := range []string{"none", "destination", "policy", "unit", "service-capability", "helper-capability"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			spec, i, services, c, before, q := deliveryFixture(t)
			active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(q), false)
			testutil.Must(t, err)
			input := sameDestinationProviderInput(t, spec)
			switch damage {
			case "destination":
				p := input.Providers["telegram"]
				testutil.Must(t, os.WriteFile(p.ChatFile, []byte("999"), 0600))
				token, e := provider.ReadSecret(p.TokenFile)
				testutil.Must(t, e)
				p.Destination = provider.TelegramIdentity(token, "999")
				input.Providers["telegram"] = p
			case "policy":
				input.Worker.Producers[0].MaxMessages++
			case "unit":
				testutil.Must(t, os.WriteFile(filepath.Join(spec.UnitDirectory, managedUnit), []byte("foreign"), 0600))
			case "service-capability", "helper-capability":
				calls := 0
				i.deliveryProvidersProbe = func(context.Context, *safefs.Directory, string, string) error {
					calls++
					if damage == "service-capability" || calls == 2 {
						return errors.New("old helper")
					}
					return nil
				}
			}
			raw, err := input.Bytes()
			testutil.Must(t, err)
			tree := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
			starts, stops := services.starts, services.stops
			review, err := i.reviewDeliveryProviderRotation(ctx, spec.Root, protocol.Hash(active), raw)
			if (err == nil) != (damage == "none") {
				t.Fatal("review result", damage, err)
			}
			if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) || starts != services.starts || stops != services.stops {
				t.Fatal("review mutated installation")
			}
			if damage == "none" {
				if protocol.Hash(review.Before) != protocol.Hash(active) {
					t.Fatal("review changed predecessor")
				}
				for _, path := range []string{c.Providers["telegram"].TokenFile, c.Providers["telegram"].ChatFile, input.Providers["telegram"].TokenFile, input.Providers["telegram"].ChatFile} {
					if !slices.Contains(review.Credentials, path) {
						t.Fatal("review omitted a retained exclusion")
					}
				}
			}
		})
	}
}

func TestDeliveryProviderExclusionsSurviveDeactivationAndNewAdmission(t *testing.T) {
	ctx := context.Background()
	spec, i, _, _, before, q := deliveryFixture(t)
	active, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(before), protocol.Hash(q), false)
	testutil.Must(t, err)
	paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(active), "", false)
	testutil.Must(t, err)
	input := sameDestinationProviderInput(t, spec)
	raw, err := input.Bytes()
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
	rotated, err := i.rotatePausedDeliveryProvider(ctx, spec, protocol.Hash(paused))
	testutil.Must(t, err)
	review := qualifyPausedDirectDelivery(t, rotated)
	admitted, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(rotated), review.QualificationHash, false)
	testutil.Must(t, err)
	producer := input.Worker.Producers[0].Producer
	profile, err := InspectManagedReportProfile(spec.Root, admitted.Identity, producer, "ops")
	testutil.Must(t, err)
	deactivated, err := i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(admitted), "", false)
	testutil.Must(t, err)
	if deactivated.ReportCredentialHistory == nil || *deactivated.ReportCredentialHistory != *rotated.ReportCredentialHistory {
		t.Fatal("deactivation dropped exclusions")
	}
	observed, err := InspectManagedReportProfile(spec.Root, deactivated.Identity, producer, "ops")
	testutil.Must(t, err)
	if !slices.Equal(observed.Credentials, profile.Credentials) {
		t.Fatal("deactivation released retained credential files")
	}
	// This specialist activation starts a version-1 admission, outside the
	// ordinary pause/renewal lineage. Its manifest still anchors the old files.
	latest, err := i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(deactivated), review.QualificationHash, false)
	testutil.Must(t, err)
	observed, err = InspectManagedReportProfile(spec.Root, latest.Identity, producer, "ops")
	testutil.Must(t, err)
	if !slices.Equal(observed.Credentials, profile.Credentials) {
		t.Fatal("new admission released retained credentials")
	}
	_, err = i.prepareDelivery(ctx, "deactivate-delivery", spec.Root, protocol.Hash(latest), "", false)
	testutil.Must(t, err)
	testutil.Must(t, i.services.Stop(ctx))
	publication := publishManagedRecovery(t, spec)
	spec.RecoveryCommitHash = publication.Commit.CommitHash
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.recoveryProbe, i.deliveryRecoveryProbe = nop, nop
	latest, err = i.prepare(ctx, "adopt-recovery", spec)
	testutil.Must(t, err)
	if latest.ReportCredentialHistory == nil || *latest.ReportCredentialHistory != *rotated.ReportCredentialHistory {
		t.Fatal("specialist recovery dropped credential history")
	}
	observed, err = InspectManagedReportProfile(spec.Root, latest.Identity, producer, "ops")
	testutil.Must(t, err)
	if !slices.Equal(observed.Credentials, profile.Credentials) {
		t.Fatal("specialist recovery released retained credential files")
	}
	d, err := privateManagedDir(spec.Root, true)
	testutil.Must(t, err)
	defer d.Close()
	j, err := readManagedJournal(d)
	testutil.Must(t, err)
	j.Intent.After.ReportCredentialHistory = nil
	if j.validate() == nil {
		t.Fatal("non-rotation transition dropped credential history")
	}
	old := filepath.Join(spec.Root, "operations", rotated.ReportCredentialHistory.Operation, "intent.json")
	testutil.Must(t, os.Remove(old))
	if _, err = InspectManagedReportProfile(spec.Root, latest.Identity, producer, "ops"); err == nil {
		t.Fatal("missing credential history silently released exclusions")
	}
}
