package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func signalDeliveryCandidate(t *testing.T, spec ManagedSpec, native FreshSignalManifest, recipient string) (RuntimeInput, []byte) {
	t.Helper()
	raw, err := os.ReadFile(spec.RuntimeFile)
	testutil.Must(t, err)
	var input RuntimeInput
	testutil.Must(t, safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes))
	root := t.TempDir()
	accountPath, recipientPath := filepath.Join(root, "account"), filepath.Join(root, "recipient")
	original, err := os.ReadFile(native.Spec.AccountFile)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(accountPath, original, 0600))
	testutil.Must(t, os.WriteFile(recipientPath, []byte(recipient), 0600))
	refs, err := ReviewFreshSignalDelivery(context.Background(), native.Spec.Root, accountPath, recipientPath, managedDigest(original), managedDigest([]byte(recipient)))
	testutil.Must(t, err)
	p := input.Providers["signal"]
	p.SignalDelivery = refs
	a, r, err := refs.Values()
	testutil.Must(t, err)
	p.Destination = relay.Destination(a, r)
	input.Providers["signal"] = p
	raw, err = input.Bytes()
	testutil.Must(t, err)
	return input, raw
}

func TestFreshSignalRecipientChangesRetainNativeAuthorityAcrossInterruptions(t *testing.T) {
	for _, bound := range []string{"prepared", "stop-returned", "delivery-limits-migrated", "files", "published", "start-returned", "complete"} {
		t.Run(bound, func(t *testing.T) {
			ctx := context.Background()
			spec, i, _, native := freshSignalGatewayFixture(t, false)
			nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.enrolledLimitsProbe = nop
			i.signalDeliveryProbe, i.deliveryPauseProbe, i.deliveryLimitsProbe, i.deliveryProvidersProbe = nop, nop, nop, nop
			current, err := i.ensureVerification(ctx, spec)
			testutil.Must(t, err)
			q := qualifySignalDeliveryCandidate(t, current)
			current, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(current), q, false)
			testutil.Must(t, err)
			original := current
			nativeBefore := map[string][]byte{}
			for _, name := range []string{"intent.json", "ready.json", "lifecycle.json", "account", "recipient"} {
				b, e := os.ReadFile(filepath.Join(native.Spec.Root, name))
				testutil.Must(t, e)
				nativeBefore[name] = b
			}
			for cycle := range 2 {
				input, raw := signalDeliveryCandidate(t, spec, native, fmt.Sprint("+1000000000", cycle+1))
				_, err = i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(current), raw, "destination")
				testutil.Must(t, err)
				paused, err := i.prepareDelivery(ctx, "pause-delivery", spec.Root, protocol.Hash(current), "", false)
				testutil.Must(t, err)
				testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
				lost := errors.New("lost Signal migration reply")
				i.fault = func(at string) error {
					if at == bound {
						return lost
					}
					return nil
				}
				_, err = i.changePausedRuntime(ctx, spec, protocol.Hash(paused), "destination")
				if !errors.Is(err, lost) {
					t.Fatal("missed Signal boundary", err)
				}
				i.fault = nil
				current, err = i.changePausedRuntime(ctx, spec, protocol.Hash(paused), "destination")
				testutil.Must(t, err)
				observed, done, err := i.inspectPausedRuntimeChange(ctx, spec, protocol.Hash(paused), "destination")
				testutil.Must(t, err)
				if !done || protocol.Hash(observed) != protocol.Hash(current) || current.DeliveryOwnerHash != original.DeliveryOwnerHash || *current.FreshSignal != *original.FreshSignal {
					t.Fatal("native migration replaced owner")
				}
				p := input.Providers["signal"]
				resolved, err := freshSignalRecipient(native, p.SignalDelivery.Hash(), p.Destination)
				testutil.Must(t, err)
				if resolved != fmt.Sprint("+1000000000", cycle+1) {
					t.Fatal("wrong approved recipient")
				}
				_, ready, err := ReviewDeliveryAdmission(ctx, spec.Root, protocol.Hash(current))
				testutil.Must(t, err)
				if ready {
					t.Fatal("recipient edit reused old qualification")
				}
				q = qualifySignalDeliveryCandidate(t, current)
				current, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(current), q, false)
				testutil.Must(t, err)
			}
			for name, want := range nativeBefore {
				got, e := os.ReadFile(filepath.Join(native.Spec.Root, name))
				testutil.Must(t, e)
				if !reflect.DeepEqual(got, want) {
					t.Fatal("recipient change rewrote native authority", name)
				}
			}
		})
	}
}

func TestFreshSignalReferenceChangeAndLostApprovalRefuse(t *testing.T) {
	ctx := context.Background()
	spec, i, _, native := freshSignalGatewayFixture(t, true)
	nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.signalDeliveryProbe, i.bootstrapProvidersProbe, i.bootstrapLimitsProbe = nop, nop, nop
	m, err := i.ensureVerification(ctx, spec)
	testutil.Must(t, err)
	recipient, err := provider.ReadSecret(native.Spec.RecipientFile)
	testutil.Must(t, err)
	input, raw := signalDeliveryCandidate(t, spec, native, recipient)
	testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
	m, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(m), "bootstrap-providers")
	testutil.Must(t, err)
	p := input.Providers["signal"]
	testutil.Must(t, InspectFreshSignalDeliveryReferences(ctx, *m.FreshSignal, *p.SignalDelivery, p.Destination))
	first := p
	input, raw = signalDeliveryCandidate(t, spec, native, "+10000000009")
	testutil.Must(t, os.WriteFile(spec.RuntimeFile, raw, 0600))
	m, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(m), "bootstrap-providers")
	testutil.Must(t, err)
	next, err := input.Resolve()
	testutil.Must(t, err)
	d, err := safefs.Open(spec.Root)
	testutil.Must(t, err)
	j, err := readManagedJournal(d)
	testutil.Must(t, err)
	missingAnchor := m
	missingAnchor.ReportCredentialHistory = nil
	if _, err = verifyManagedArtifacts(d, missingAnchor); err == nil {
		t.Fatal("native recipient accepted a removed history anchor")
	}
	paths, err := retainedDeliveryCredentials(d, j, next)
	d.Close()
	testutil.Must(t, err)
	for _, want := range []string{native.Spec.AccountFile, native.Spec.RecipientFile, first.SignalDelivery.AccountFile, first.SignalDelivery.RecipientFile, input.Providers["signal"].SignalDelivery.AccountFile, input.Providers["signal"].SignalDelivery.RecipientFile} {
		if !slices.Contains(paths, want) {
			t.Fatal("staged history released private reference", want)
		}
	}
	path := filepath.Join(native.Spec.Root, "delivery", first.SignalDelivery.Hash()+".json")
	testutil.Must(t, os.Remove(path))
	if _, err = i.ensureVerification(ctx, spec); err == nil {
		t.Fatal("lost approval was recreated")
	}
	if _, err = InspectRetainedInstallation(ctx, spec.Root); err == nil {
		t.Fatal("status accepted lost historical approval")
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("lost approval repaired", err)
	}
}

func qualifySignalDeliveryCandidate(t *testing.T, m ManagedManifest) string {
	t.Helper()
	ctx := context.Background()
	q, err := BeginQualification(ctx, m.Root, protocol.Hash(m))
	testutil.Must(t, err)
	for _, c := range q.Checks {
		_, err = ObserveQualification(ctx, m.Root, protocol.Hash(m), QualificationObservation{Session: q.ID, Scope: c.Scope, ID: c.ID, Result: "passed", EvidenceHash: protocol.Hash("synthetic offline recipient observation")})
		testutil.Must(t, err)
	}
	_, err = CommitQualification(ctx, m.Root, protocol.Hash(m), q.ID)
	testutil.Must(t, err)
	review, ready, err := ReviewDeliveryAdmission(ctx, m.Root, protocol.Hash(m))
	testutil.Must(t, err)
	if !ready {
		t.Fatal("qualification incomplete")
	}
	return review.QualificationHash
}

func TestSignalDeliveryReviewRefusesAccountAndHelperChangesBeforeEffects(t *testing.T) {
	for _, damage := range []string{"account", "recipient-bytes", "gateway-capability", "helper-capability", "native-capability"} {
		t.Run(damage, func(t *testing.T) {
			ctx := context.Background()
			spec, i, _, native := freshSignalGatewayFixture(t, false)
			nop := func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.deliveryPauseProbe, i.deliveryLimitsProbe = nop, nop
			m, err := i.ensureVerification(ctx, spec)
			testutil.Must(t, err)
			q := qualifySignalDeliveryCandidate(t, m)
			m, err = i.prepareDelivery(ctx, "activate-delivery", spec.Root, protocol.Hash(m), q, false)
			testutil.Must(t, err)
			input, raw := signalDeliveryCandidate(t, spec, native, "+10000000009")
			p := input.Providers["signal"]
			calls := 0
			i.signalDeliveryProbe = func(context.Context, *safefs.Directory, string, string) error {
				calls++
				if damage == "gateway-capability" && calls == 1 || damage == "helper-capability" && calls == 2 || damage == "native-capability" && calls == 3 {
					return errors.New("old executable")
				}
				return nil
			}
			if damage == "account" {
				testutil.Must(t, os.WriteFile(p.SignalDelivery.AccountFile, []byte("+19999999999"), 0600))
				copy := *p.SignalDelivery
				copy.AccountHash = managedDigest([]byte("+19999999999"))
				p.SignalDelivery = &copy
				p.Destination = relay.Destination("+19999999999", "+10000000009")
				input.Providers["signal"] = p
				raw, err = input.Bytes()
				testutil.Must(t, err)
			}
			if damage == "recipient-bytes" {
				testutil.Must(t, os.WriteFile(p.SignalDelivery.RecipientFile, []byte("+10000000008"), 0600))
			}
			before, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
			testutil.Must(t, err)
			if _, err = i.reviewPausedRuntimeChange(ctx, spec.Root, protocol.Hash(m), raw, "destination"); err == nil {
				t.Fatal("unsafe Signal proposal accepted")
			}
			after, err := os.ReadFile(filepath.Join(spec.Root, "lifecycle.json"))
			testutil.Must(t, err)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("failed review changed gateway intent")
			}
			if _, err = os.Stat(filepath.Join(native.Spec.Root, "delivery")); !os.IsNotExist(err) {
				t.Fatal("failed review approved recipient", err)
			}
		})
	}
}
