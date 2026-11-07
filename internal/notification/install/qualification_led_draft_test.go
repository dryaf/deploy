package install

import (
	"context"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestGuidedLEDQualificationRequiresEnvironmentAndPreservesWindow(t *testing.T) {
	spec, _, _, report := qualificationFixture(t, func(c *Runtime) { c.LEDSocket = "/private/fixture-led.sock" })
	ctx := context.Background()
	pin := protocol.Hash(report.Binding.Manifest)
	now := time.Now().UTC()
	call := func(action string, o QualificationObservation, at time.Time) QualificationSession {
		session, err := qualificationDraftOperation(ctx, spec.Root, pin, action, o, at)
		testutil.Must(t, err)
		return session
	}
	session := call("begin", QualificationObservation{}, now)
	if !session.LEDEnvironmentRequired {
		t.Fatal("missing LED environment was hidden")
	}
	for _, c := range session.Checks {
		call("observe", QualificationObservation{Session: session.ID, Scope: c.Scope, ID: c.ID, Result: "passed", EvidenceHash: protocol.Hash("synthetic observation")}, now.Add(time.Second))
	}
	incomplete := call("commit", QualificationObservation{Session: session.ID}, now.Add(2*time.Second))
	if !incomplete.Recorded || incomplete.EvidenceComplete || !incomplete.LEDEnvironmentRequired {
		t.Fatal("missing environment qualified LED", incomplete)
	}
	next := call("begin", QualificationObservation{}, now.Add(3*time.Second))
	if next.ID == session.ID || !next.CreatedAt.Equal(session.CreatedAt) {
		t.Fatal("resuming renewed the evidence window")
	}
	environment := QualificationObservation{Session: next.ID, LEDVersion: "synthetic-v1", LEDConfigHash: protocol.Hash("synthetic daemon configuration")}
	recorded := call("observe", environment, now.Add(4*time.Second))
	if recorded.LEDEnvironmentRequired || !recorded.EvidenceComplete || recorded.Recorded {
		t.Fatal(recorded)
	}
	tree := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	call("observe", environment, now.Add(5*time.Second))
	changed := environment
	changed.LEDVersion = "other-version"
	if _, err := qualificationDraftOperation(ctx, spec.Root, pin, "observe", changed, now.Add(6*time.Second)); err == nil {
		t.Fatal("rewrote environment")
	}
	if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
		t.Fatal("retry or rejection changed retained evidence")
	}
	complete := call("commit", QualificationObservation{Session: next.ID}, now.Add(7*time.Second))
	if !complete.Recorded || !complete.EvidenceComplete || complete.LEDEnvironmentRequired {
		t.Fatal(complete)
	}
	again := call("begin", QualificationObservation{}, now.Add(8*time.Second))
	if again.ID != next.ID || !again.Recorded {
		t.Fatal("complete report was replaced")
	}
	expired := call("begin", QualificationObservation{}, now.Add(qualificationWindow))
	if expired.ID == next.ID || !expired.LEDEnvironmentRequired || expired.EvidenceComplete {
		t.Fatal("reused stale environment", expired)
	}
	for _, check := range expired.Checks {
		if check.Result != "pending" {
			t.Fatal("renewed stale observation")
		}
	}
}

func TestGuidedLEDQualificationRejectsUnconfiguredAndMixedEnvironment(t *testing.T) {
	for _, led := range []bool{false, true} {
		spec, _, _, report := qualificationFixture(t, func(c *Runtime) {
			if led {
				c.LEDSocket = "/private/fixture-led.sock"
			}
		})
		pin := protocol.Hash(report.Binding.Manifest)
		ctx := context.Background()
		session, err := BeginQualification(ctx, spec.Root, pin)
		testutil.Must(t, err)
		valid := QualificationObservation{Session: session.ID, LEDVersion: "synthetic-v1", LEDConfigHash: protocol.Hash("synthetic config")}
		cases := []QualificationObservation{}
		if !led {
			cases = append(cases, valid)
		}
		missing := valid
		missing.LEDVersion = ""
		cases = append(cases, missing)
		missing = valid
		missing.LEDConfigHash = ""
		cases = append(cases, missing)
		mixed := valid
		mixed.Scope = "led"
		mixed.ID = "visual-patterns"
		mixed.Result = "passed"
		mixed.EvidenceHash = protocol.Hash("evidence")
		cases = append(cases, mixed)
		tree := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
		for _, o := range cases {
			if _, err = ObserveQualification(ctx, spec.Root, pin, o); err == nil {
				t.Fatal("invalid environment accepted", led)
			}
		}
		if _, err = qualificationDraftOperation(ctx, spec.Root, pin, "commit", valid, time.Now().UTC()); err == nil {
			t.Fatal("commit ignored environment")
		}
		if _, err = qualificationDraftOperation(ctx, spec.Root, pin, "begin", valid, time.Now().UTC()); err == nil {
			t.Fatal("begin ignored environment")
		}
		if tree != activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) {
			t.Fatal("refused environment changed draft")
		}
	}
}
