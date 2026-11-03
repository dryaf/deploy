package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

type ledEvidenceFixture struct {
	*ledRecoveryFixture
	inventory RecoveryLEDReview
	challenge RecoveryLEDChallenge
	report    RecoveryLEDReport
}

func newLEDEvidenceFixture(t *testing.T, policy bool, wildcard bool) *ledEvidenceFixture {
	return newLEDEvidenceFixtureWithRegistry(t, policy, wildcard, migrationSocket, nil)
}

func newLEDEvidenceFixtureWithRegistry(t *testing.T, policy bool, wildcard bool, socket string, api LEDAPI, configure ...func(*Config)) *ledEvidenceFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	c := configuration()
	c.Producers[0].PolicyHealth = policy
	selector := "backup"
	if wildcard {
		selector = "*"
	}
	c.LEDBindings = []LEDBinding{binding("selected", selector)}
	for _, adjust := range configure {
		adjust(&c)
	}
	path := t.TempDir()
	must(t, os.Chmod(path, 0700))
	s, err := Open(path, c, true)
	must(t, err)
	defer s.Close()
	s.now = func() time.Time { return now.Add(-time.Minute) }
	old := snapshot(s.now(), 1, 1)
	old.Subjects[0].Condition = "critical"
	if policy {
		old.Subjects[0].FaultRevision = 1
	}
	must(t, s.Replace(ctx, old))
	f := &ledRecoveryFixture{path: path, config: c, now: now, boot: "boot-one", uptime: 100}
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		var e error
		f.old, e = s.currentLEDState(tx, "selected", "critical")
		return e
	}))
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	old.Sequence = 2
	old.BasisRevision = 2
	if policy {
		old.Subjects[0].FaultRevision = 2
	}
	must(t, s.Replace(ctx, old))
	must(t, s.transaction(ctx, func(tx *sql.Tx) error { var e error; f.new, e = s.currentLEDState(tx, "selected", "warning"); return e }))
	must(t, s.Close())
	_, err = prepareSnapshotRecovery(ctx, path, artifact, c, nil)
	must(t, err)
	f.r, err = openRecoveryReconciler(ctx, path, c, nil)
	must(t, err)
	f.r.now = func() time.Time { return f.now }
	f.r.clock = func() (string, float64, error) { return f.boot, f.uptime, nil }
	t.Cleanup(func() { f.r.Close() })
	f.api = &inventoryLEDObserver{fakeLED: &fakeLED{states: map[string]led.State{
		f.old: {ID: f.old, LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true},
		f.new: {ID: f.new, LED: "green", Pattern: "blink-2", Priority: 60, Persistent: true},
	}}}
	if api == nil {
		api = f.api
	} else {
		for _, state := range f.api.states {
			must(t, api.Set(ctx, state))
		}
	}
	v, err := f.r.reviewLEDInventory(ctx, socket, api, false)
	must(t, err)
	ch, err := f.r.ChallengeLED(ctx, v.InventoryID, producer)
	must(t, err)
	report := RecoveryLEDReport{Snapshot: snapshot(f.now, 3, 3)}
	if policy {
		report.Snapshot.Subjects[0].FaultRevision = 2
		for i, rev := range []uint64{1, 2} {
			report.Transitions = append(report.Transitions, protocol.FaultTransition{ID: []string{"resolve-one", "resolve-two"}[i], Subject: "backup", FaultRevision: rev, Cause: "resolved", StatusSequence: 3, BasisRevision: 3, ObservedAt: f.now, ValidUntil: f.now.Add(time.Minute)})
		}
	}
	return &ledEvidenceFixture{f, v, ch, report}
}
func submitLEDEvidence(t *testing.T, f *ledEvidenceFixture) RecoveryLEDReview {
	t.Helper()
	_, err := f.r.SubmitLED(context.Background(), f.challenge, f.report)
	must(t, err)
	v, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, nil, false)
	must(t, err)
	return v
}
func TestRecoveryLEDEvidencePreparesExactFaultsWithoutMutation(t *testing.T) {
	for _, policy := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "policy"}[policy], func(t *testing.T) {
			f := newLEDEvidenceFixture(t, policy, false)
			before := protocol.Hash(f.api.states)
			v := submitLEDEvidence(t, f)
			result := v.Resolutions["selected"]
			if !result.DomainResolved || len(result.ProposedFaultIDs) != 2 || result.EvidenceHash == "" || result.ValidUntil.After(v.ValidUntil) {
				t.Fatal(result)
			}
			if v.ClearAllowed || v.ActivationAllowed || v.Resolutions[workerLEDBinding].DomainResolved || before != protocol.Hash(f.api.states) || len(f.api.cleared) > 0 {
				t.Fatal("proposal mutated or activated LEDs")
			}
			saved, err := f.r.LEDEvidence(context.Background(), v.InventoryID, producer)
			must(t, err)
			f.now = f.now.Add(time.Second)
			f.uptime++
			again, err := f.r.SubmitLED(context.Background(), f.challenge, f.report)
			must(t, err)
			if protocol.Hash(saved) != protocol.Hash(again) {
				t.Fatal("replay renewed evidence")
			}
			must(t, f.r.Close())
			r, err := openRecoveryReconciler(context.Background(), f.path, f.config, nil)
			must(t, err)
			defer r.Close()
			got, err := r.LEDEvidence(context.Background(), v.InventoryID, producer)
			must(t, err)
			if protocol.Hash(saved) != protocol.Hash(got) {
				t.Fatal("restart changed evidence")
			}
			if s, err := Open(f.path, f.config, false); err == nil {
				s.Close()
				t.Fatal("evidence activated worker")
			}
		})
	}
}
func TestRecoveryLEDEvidenceRejectsInvalidReports(t *testing.T) {
	for _, damage := range []string{"nonce", "epoch", "producer", "pre-challenge", "future", "old-sequence", "old-basis", "config", "fault-regression", "transition-old", "transition-revision", "transition-basis", "support-missing", "unsupported-policy", "oversized"} {
		t.Run(damage, func(t *testing.T) {
			policy := damage != "unsupported-policy"
			f := newLEDEvidenceFixture(t, policy, false)
			ch, v := f.challenge, f.report
			switch damage {
			case "nonce":
				ch.Nonce = "n00000000000000000000000000000000"
			case "epoch":
				ch.After.Recovery = "other"
			case "producer":
				v.Snapshot.Producer.Epoch = "other"
			case "pre-challenge":
				v.Snapshot.EvaluatedAt = f.now.Add(-time.Second)
			case "future":
				v.Snapshot.EvaluatedAt = f.now.Add(time.Second)
			case "old-sequence":
				v.Snapshot.Sequence = 2
			case "old-basis":
				v.Snapshot.BasisRevision = 1
			case "config":
				v.Snapshot.ConfigRevision = "other"
			case "fault-regression":
				v.Snapshot.Subjects[0].FaultRevision = 1
			case "transition-old":
				v.Transitions[0].ObservedAt = f.now.Add(-time.Second)
			case "transition-revision":
				v.Transitions[0].FaultRevision = 3
			case "transition-basis":
				v.Transitions[0].BasisRevision = 2
			case "support-missing":
				v.Transitions[0].Cause = "policy_accepted"
				v.Transitions[0].Support = []protocol.DecisionRef{{ID: "accept", Revision: 1}}
			case "unsupported-policy":
				v.Snapshot.Subjects[0].FaultRevision = 1
			case "oversized":
				for len(v.Transitions) <= 128 {
					v.Transitions = append(v.Transitions, v.Transitions[0])
				}
			}
			if _, err := f.r.SubmitLED(context.Background(), ch, v); err == nil {
				t.Fatal("invalid report admitted")
			}
			if _, err := f.r.LEDEvidence(context.Background(), f.inventory.InventoryID, producer); !errors.Is(err, protocol.ErrRecoveryIncomplete) {
				t.Fatal("rejected report persisted", err)
			}
		})
	}
}
func TestRecoveryLEDEvidenceResolutionRequiresCoverageAndTransitions(t *testing.T) {
	for _, damage := range []string{"missing-transition", "missing-subject", "paused", "expired-subject", "expired-support", "expired-transition", "retired", "source-drift", "domain-drift", "reboot", "clock-rollback", "expired-inventory", "monotonic-expiry"} {
		t.Run(damage, func(t *testing.T) {
			f := newLEDEvidenceFixture(t, true, true)
			switch damage {
			case "missing-transition":
				f.report.Transitions = f.report.Transitions[:1]
			case "missing-subject":
				f.report.Snapshot.Subjects = nil
				f.report.Transitions = nil
			case "paused":
				f.report.Snapshot.Subjects[0].Condition = "paused"
				f.report.Transitions = nil
			case "expired-subject":
				f.report.Snapshot.Subjects[0].ValidUntil = f.now.Add(time.Second)
			case "expired-support":
				f.report.Snapshot.Support = []protocol.AcceptanceSupport{{Subject: "backup", Decision: "accepted", Revision: 1, Occurrence: "occurrence", Assumptions: 1, ValidUntil: f.now.Add(time.Second)}}
			case "expired-transition":
				f.report.Transitions[0].ValidUntil = f.now.Add(time.Second)
			case "monotonic-expiry":
				f.report.Snapshot.Subjects[0].ValidUntil = f.now.Add(time.Second)
			case "retired":
				f.report.Snapshot.Subjects[0].Condition = "retired"
				for i := range f.report.Transitions {
					f.report.Transitions[i].Cause = "retired"
				}
			}
			// Snapshot validation requires a nonempty subject list: use an unrelated key.
			if damage == "missing-subject" {
				f.report.Snapshot.Subjects = []protocol.Subject{{Key: "unrelated", Condition: "ok", ValidUntil: f.now.Add(time.Hour)}}
			}
			_, err := f.r.SubmitLED(context.Background(), f.challenge, f.report)
			must(t, err)
			switch damage {
			case "expired-subject", "expired-support", "expired-transition":
				f.now = f.now.Add(2 * time.Second)
				f.uptime += 2
			case "expired-inventory":
				f.now = f.now.Add(31 * time.Second)
				f.uptime += 31
			case "monotonic-expiry":
				f.uptime += 2
			case "source-drift":
				_, err = f.r.db.Exec("UPDATE led_states SET obsolete=1")
				must(t, err)
			case "domain-drift":
				_, err = f.r.db.Exec("UPDATE subjects SET latched='warning'")
				must(t, err)
			case "reboot":
				f.boot = "other-boot"
			case "clock-rollback":
				f.now = f.now.Add(-time.Second)
			}
			v, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, nil, false)
			must(t, err)
			want := damage == "retired"
			if v.Resolutions["selected"].DomainResolved != want {
				t.Fatal(damage, v.Resolutions["selected"])
			}
			if !want && len(v.Resolutions["selected"].ProposedFaultIDs) > 0 {
				t.Fatal("blocked proposal exposes clear targets")
			}
		})
	}
}
func TestRecoveryLEDEvidenceRefreshCannotRegressPriorReports(t *testing.T) {
	f := newLEDEvidenceFixture(t, true, false)
	submitLEDEvidence(t, f)
	f.now = f.now.Add(time.Second)
	f.uptime++
	v, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, true)
	must(t, err)
	if _, err = f.r.SubmitLED(context.Background(), f.challenge, f.report); err == nil {
		t.Fatal("old inventory accepted new submission")
	}
	ch, err := f.r.ChallengeLED(context.Background(), v.InventoryID, producer)
	must(t, err)
	report := f.report
	report.Snapshot.EvaluatedAt = f.now
	for i := range report.Transitions {
		report.Transitions[i].ObservedAt = f.now
	}
	if _, err = f.r.SubmitLED(context.Background(), ch, report); err == nil {
		t.Fatal("refresh allowed reuse of status sequence")
	}
	report.Snapshot.Sequence++
	for i := range report.Transitions {
		report.Transitions[i].StatusSequence++
	}
	_, err = f.r.SubmitLED(context.Background(), ch, report)
	must(t, err)
	must(t, f.r.checkLEDEvidenceHistory(context.Background(), f.r.db))
}
func TestRecoveryLEDEvidencePolicyAcceptanceHasFiniteLease(t *testing.T) {
	f := newLEDEvidenceFixture(t, true, false)
	f.report.Snapshot.Support = []protocol.AcceptanceSupport{{Subject: "backup", Decision: "risk", Revision: 1, Occurrence: "dev-build", Assumptions: 1, ValidUntil: f.now.Add(3 * time.Second)}}
	for i := range f.report.Transitions {
		f.report.Transitions[i].Cause = "policy_accepted"
		f.report.Transitions[i].Support = []protocol.DecisionRef{{ID: "risk", Revision: 1}}
	}
	v := submitLEDEvidence(t, f)
	if !v.Resolutions["selected"].DomainResolved || !v.Resolutions["selected"].ValidUntil.Equal(f.now.Add(3*time.Second)) {
		t.Fatal(v.Resolutions)
	}
	f.now = f.now.Add(3 * time.Second)
	f.uptime += 3
	v, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, nil, false)
	must(t, err)
	if v.Resolutions["selected"].DomainResolved {
		t.Fatal("expired acceptance cleared policy fault")
	}
}
func TestRecoveryLEDEvidenceCorruptionBlocksReopen(t *testing.T) {
	for _, damage := range []string{"challenge-row", "report-row", "challenge-file", "report-file", "schema", "prior-report"} {
		t.Run(damage, func(t *testing.T) {
			f := newLEDEvidenceFixture(t, true, false)
			submitLEDEvidence(t, f)
			var err error
			switch damage {
			case "challenge-row":
				_, err = f.r.db.Exec("UPDATE reconciliation_led_evidence SET challenge_hash='bad'")
			case "report-row":
				_, err = f.r.db.Exec("UPDATE reconciliation_led_evidence SET reply_hash='bad'")
			case "challenge-file":
				err = f.r.stage.WriteJSON(ledEvidenceFile(f.inventory.InventoryID, producer, "challenge"), map[string]any{"bad": true}, true)
			case "report-file":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), ledEvidenceFile(f.inventory.InventoryID, producer, "report")))
			case "schema":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='led-evidence-version'")
			case "prior-report":
				v, e := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, true)
				must(t, e)
				_, e = f.r.ChallengeLED(context.Background(), v.InventoryID, producer)
				must(t, e)
				_, err = f.r.db.Exec("UPDATE reconciliation_led_evidence SET reply='',reply_hash='' WHERE inventory=?", f.inventory.InventoryID)
				must(t, err)
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), ledEvidenceFile(f.inventory.InventoryID, producer, "report")))
			}
			must(t, err)
			must(t, f.r.Close())
			if r, err := openRecoveryReconciler(context.Background(), f.path, f.config, nil); err == nil {
				r.Close()
				t.Fatal("corrupt evidence accepted")
			}
		})
	}
}

func TestRecoveryLEDEvidenceReadDomainValidatesStatusHash(t *testing.T) {
	f := newLEDEvidenceFixture(t, false, false)
	_, err := f.r.db.Exec("UPDATE status SET hash='bad'")
	must(t, err)
	if _, err = f.r.SubmitLED(context.Background(), f.challenge, f.report); err == nil {
		t.Fatal("corrupt source status admitted")
	}
}

func TestRecoveryLEDEvidenceRefreshCannotExtendMonotonicSupportLease(t *testing.T) {
	f := newLEDEvidenceFixture(t, true, false)
	f.report.Snapshot.Support = []protocol.AcceptanceSupport{{Subject: "backup", Decision: "risk", Revision: 1, Occurrence: "dev-build", Assumptions: 1, ValidUntil: f.now.Add(3 * time.Second)}}
	for i := range f.report.Transitions {
		f.report.Transitions[i].Cause = "policy_accepted"
		f.report.Transitions[i].Support = []protocol.DecisionRef{{ID: "risk", Revision: 1}}
	}
	submitLEDEvidence(t, f)
	f.now = f.now.Add(time.Second)
	f.uptime += 4
	inventory, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, true)
	must(t, err)
	f.challenge, err = f.r.ChallengeLED(context.Background(), inventory.InventoryID, producer)
	must(t, err)
	f.report.Snapshot.Sequence++
	f.report.Snapshot.EvaluatedAt = f.now
	for i := range f.report.Transitions {
		f.report.Transitions[i].StatusSequence++
		f.report.Transitions[i].ObservedAt = f.now
	}
	v := submitLEDEvidence(t, f)
	if v.Resolutions["selected"].DomainResolved || v.Resolutions["selected"].Reason != "acceptance_support_expired" {
		t.Fatal(v.Resolutions)
	}
}

func TestRecoveryLEDEvidenceAcceptanceDependencyCannotDisappearOnRefresh(t *testing.T) {
	for _, resolve := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated-acceptance", true: "explicit-resolution"}[resolve], func(t *testing.T) {
			f := newLEDEvidenceFixture(t, true, false)
			f.report.Snapshot.Support = []protocol.AcceptanceSupport{{Subject: "backup", Decision: "risk", Revision: 1, Occurrence: "dev-build", Assumptions: 1, ValidUntil: f.now.Add(time.Minute)}}
			for i := range f.report.Transitions {
				f.report.Transitions[i].Cause = "policy_accepted"
				f.report.Transitions[i].Support = []protocol.DecisionRef{{ID: "risk", Revision: 1}}
			}
			submitLEDEvidence(t, f)
			f.now = f.now.Add(time.Second)
			f.uptime++
			inventory, err := f.r.reviewLEDInventory(context.Background(), migrationSocket, f.api, true)
			must(t, err)
			f.challenge, err = f.r.ChallengeLED(context.Background(), inventory.InventoryID, producer)
			must(t, err)
			f.report.Snapshot.Sequence++
			f.report.Snapshot.EvaluatedAt = f.now
			if resolve {
				f.report.Snapshot.Support = nil
			} else {
				f.report.Snapshot.Support[0].Decision = "other-risk"
			}
			for i := range f.report.Transitions {
				tr := &f.report.Transitions[i]
				tr.StatusSequence++
				tr.ObservedAt = f.now
				if resolve {
					tr.Cause = "resolved"
					tr.Support = nil
				} else {
					tr.Support = []protocol.DecisionRef{{ID: "other-risk", Revision: 1}}
				}
			}
			v := submitLEDEvidence(t, f)
			if v.Resolutions["selected"].DomainResolved != resolve {
				t.Fatal(v.Resolutions)
			}
		})
	}
}

func TestRecoveryLEDEvidenceCannotRegressCompletedProducerReconciliation(t *testing.T) {
	f := newLEDEvidenceFixture(t, true, false)
	ctx := context.Background()
	ch, err := f.r.Challenge(ctx, producer)
	must(t, err)
	must(t, f.r.Begin(ctx, ch, protocol.RecoveryManifest{ImportedReceipts: protocol.Cursor{Identity: f.r.journal.Before, Producer: producer}}))
	snap := f.report.Snapshot
	snap.Sequence = 10
	snap.BasisRevision = 10
	_, err = f.r.Finish(ctx, ch, snap)
	must(t, err)
	if _, err = f.r.SubmitLED(ctx, f.challenge, f.report); err == nil {
		t.Fatal("domain reconciliation changed without invalidating challenge")
	}
	inventory, err := f.r.reviewLEDInventory(ctx, migrationSocket, f.api, true)
	must(t, err)
	f.challenge, err = f.r.ChallengeLED(ctx, inventory.InventoryID, producer)
	must(t, err)
	if _, err = f.r.SubmitLED(ctx, f.challenge, f.report); err == nil {
		t.Fatal("LED report regressed completed reconciliation status")
	}
	f.report.Snapshot.Sequence = 11
	f.report.Snapshot.BasisRevision = 11
	for i := range f.report.Transitions {
		f.report.Transitions[i].StatusSequence = 11
		f.report.Transitions[i].BasisRevision = 11
	}
	v := submitLEDEvidence(t, f)
	if !v.Resolutions["selected"].DomainResolved {
		t.Fatal(v.Resolutions)
	}
}
