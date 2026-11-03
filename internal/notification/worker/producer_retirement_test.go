package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
)

func retirementFixture(t *testing.T) (*Store, *time.Time, string) {
	t.Helper()
	s, now, path := fixture(t)
	v := snapshot(*now, 1, 1)
	v.Subjects[0].Condition = "retired"
	must(t, s.Replace(context.Background(), v))
	must(t, s.Revoke(context.Background(), producer, "generation-one"))
	return s, now, path
}

func TestProducerRetirementRequiresStableLeaseAndIdentity(t *testing.T) {
	for _, change := range []string{"lease", "identity"} {
		t.Run(change, func(t *testing.T) {
			s, _, path := retirementFixture(t)
			before := readOwner(t, s)
			if change == "lease" {
				must(t, os.Rename(filepath.Join(path, "worker.lock"), filepath.Join(path, "worker.lock.old")))
				must(t, os.WriteFile(filepath.Join(path, "worker.lock"), nil, 0600))
			} else {
				must(t, os.WriteFile(filepath.Join(path, "identity.json"), []byte("{}\n"), 0600))
			}
			for _, apply := range []bool{false, true} {
				if _, err := s.RetireProducer(context.Background(), producer, "operator", "Decommissioned", apply); err == nil {
					t.Fatal("retirement accepted replaced authority", change, apply)
				}
			}
			if !sameOwnership(before, readOwner(t, s)) {
				t.Fatal("refused retirement changed ownership")
			}
		})
	}
}

func TestProducerRetirementPreservesHistoryAndFencesAdmission(t *testing.T) {
	s, now, path := retirementFixture(t)
	ctx := context.Background()
	before := readOwner(t, s)
	preview, err := s.RetireProducer(ctx, producer, "operator", "Source decommissioned", false)
	must(t, err)
	if !preview.Eligible || preview.Applied || preview.Retirement != nil || !sameOwnership(before, readOwner(t, s)) {
		t.Fatal(preview)
	}
	v, err := s.RetireProducer(ctx, producer, "operator", "Source decommissioned", true)
	must(t, err)
	if !v.Applied || v.Retirement == nil || v.Retirement.EvidenceHash != preview.EvidenceHash {
		t.Fatal(v)
	}
	want := readOwner(t, s)
	if want.Revision != before.Revision+1 || want.Generations["generation-one"] != "revoked" {
		t.Fatal(want)
	}
	must(t, s.verifyOwnershipPrefix(want))
	again, err := s.RetireProducer(ctx, producer, "operator", "Source decommissioned", true)
	must(t, err)
	if *again.Retirement != *v.Retirement || !sameOwnership(want, readOwner(t, s)) {
		t.Fatal("retry changed retirement")
	}
	if _, err = s.RetireProducer(ctx, producer, "operator", "Changed decision", true); err == nil {
		t.Fatal("conflicting decision accepted")
	}
	if err = s.Stage(producer, "new-generation"); err == nil {
		t.Fatal("retired epoch staged")
	}
	if err = s.Activate(producer, "generation-one"); err == nil {
		t.Fatal("retired epoch activated")
	}
	if err = s.Replace(ctx, snapshot(*now, 2, 2)); err == nil {
		t.Fatal("retired publisher renewed status")
	}
	if _, err = s.Enqueue(ctx, report(*now, "late", 1, 1, 1)); err == nil {
		t.Fatal("retired publisher enqueued work")
	}
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "retired" || len(h.RetiredProducers) != 1 || len(h.Subjects) != 0 {
		t.Fatal(h)
	}
	c := s.config
	must(t, s.Close())
	r, err := Open(path, c, false)
	must(t, err)
	defer r.Close()
	if !sameOwnership(want, readOwner(t, r)) {
		t.Fatal("reopen changed retirement")
	}
	if err = r.Stage(producer, "resurrected"); err == nil {
		t.Fatal("reopen lost retirement")
	}
}

func TestProducerRetirementRefusesOutstandingObligations(t *testing.T) {
	for _, issue := range []string{"active", "staged", "message", "receipt", "subject", "latch", "policy", "transition", "command", "result", "reply", "led", "clock"} {
		t.Run(issue, func(t *testing.T) {
			s, now, _ := retirementFixture(t)
			ctx := context.Background()
			var err error
			switch issue {
			case "active", "staged":
				must(t, s.Stage(producer, "another"))
				if issue == "active" {
					must(t, s.Activate(producer, "another"))
				}
			case "message":
				_, err = s.Enqueue(ctx, report(*now, "pending", 1, 1, 1))
			case "receipt":
				_, err = s.db.Exec("INSERT INTO receipts(producer,body) VALUES(?,'{}')", producer.ID)
			case "subject":
				_, err = s.db.Exec("UPDATE subjects SET body=json_set(body,'$.condition','ok')")
			case "latch":
				_, err = s.db.Exec("UPDATE subjects SET latched='critical'")
			case "policy":
				_, err = s.db.Exec("INSERT INTO policy_faults VALUES(?,'source',1,'critical',1)", producer.ID)
			case "transition":
				_, err = s.db.Exec("INSERT INTO fault_transitions VALUES(?,'tr','hash','{}','pending')", producer.ID)
			case "command", "result", "reply":
				_, err = s.db.Exec("INSERT INTO commands(id,request_key,input_hash,producer,body,state,created) VALUES('C-test','key','hash',?,'{}','succeeded',1)", producer.ID)
				must(t, err)
				if issue == "command" {
					_, err = s.db.Exec("UPDATE commands SET state='outcome_unknown'")
				}
				if issue == "result" {
					_, err = s.db.Exec("INSERT INTO command_results VALUES('C-test',1,'hash','{}','{}',0)")
				}
				if issue == "reply" {
					_, err = s.db.Exec("INSERT INTO command_replies(command_id,kind,revision,body) VALUES('C-test','result',1,'{}')")
				}
			case "led":
				_, err = s.db.Exec("INSERT INTO led_states(id,condition) VALUES('old-fault','critical')")
			case "clock":
				s.clockUncertain = true
			}
			must(t, err)
			before := readOwner(t, s)
			if _, err = s.RetireProducer(ctx, producer, "operator", "Decommission", true); err == nil {
				t.Fatal("outstanding obligation ignored")
			}
			if !sameOwnership(before, readOwner(t, s)) {
				t.Fatal("refusal changed ownership")
			}
		})
	}
}

func TestProducerRetirementInterruptedPublicationAndLostReply(t *testing.T) {
	for _, point := range []string{"intent", "history", "owner"} {
		t.Run(point, func(t *testing.T) {
			s, now, path := retirementFixture(t)
			s.ownershipFault = func(at string) error {
				if at == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := s.RetireProducer(context.Background(), producer, "operator", "Retired source", true); err == nil {
				t.Fatal("fault missing")
			}
			var intent ownershipIntent
			must(t, s.dir.ReadJSON(ownershipName(producer)+".journal.json", &intent))
			if intent.Transition.Retirement == nil {
				t.Fatal("retirement intent missing")
			}
			if err := s.Replace(context.Background(), snapshot(*now, 2, 2)); err == nil {
				t.Fatal("incomplete retirement allowed admission")
			}
			c := s.config
			must(t, s.Close())
			r, err := Open(path, c, false)
			must(t, err)
			defer r.Close()
			v, err := r.RetireProducer(context.Background(), producer, "operator", "Retired source", true)
			must(t, err)
			if *v.Retirement != *intent.Transition.Retirement {
				t.Fatal("resume manufactured new authority")
			}
		})
	}
}

func TestProducerRetirementRejectsRegressedDatabaseAndDamagedHistory(t *testing.T) {
	for _, issue := range []string{"status", "submitted", "receipt", "reason", "history"} {
		t.Run(issue, func(t *testing.T) {
			s, _, path := retirementFixture(t)
			_, err := s.RetireProducer(context.Background(), producer, "operator", "Retired source", true)
			must(t, err)
			owner := readOwner(t, s)
			switch issue {
			case "status":
				_, err = s.db.Exec("DELETE FROM status")
			case "submitted":
				_, err = s.db.Exec("UPDATE producers SET submitted=1")
			case "receipt":
				_, err = s.db.Exec("INSERT INTO receipts(producer,body) VALUES(?,'{}')", producer.ID)
			case "reason":
				owner.Retirement.Reason = "Changed"
				err = s.dir.WriteJSON(ownershipName(producer)+".json", owner, true)
			case "history":
				err = os.Remove(filepath.Join(path, ownershipHistoryPath(producer, owner.Revision-1)))
			}
			must(t, err)
			c := s.config
			must(t, s.Close())
			if r, err := Open(path, c, false); err == nil {
				r.Close()
				t.Fatal("damaged retirement opened")
			}
		})
	}
}

func TestProducerRetirementRemainsPermanentAcrossRecovery(t *testing.T) {
	s, _, _ := retirementFixture(t)
	_, err := s.RetireProducer(context.Background(), producer, "operator", "Retired source", true)
	must(t, err)
	before := readOwner(t, s)
	b := recoveryBinding{Operation: "n-11111111111111111111111111111111", Snapshot: "n-22222222222222222222222222222222", Before: before.Identity, After: before.Identity}
	b.After.Recovery = "n-33333333333333333333333333333333"
	event := ownershipTransition{Schema: 2, Identity: before.Identity, Producer: producer, Revision: before.Revision + 1, Previous: before.History, BeforeHash: protocol.Hash(before), Operation: "recover", Recovery: &ownershipRecovery{Binding: b, SummaryHash: protocol.Hash("summary")}}
	after, err := applyOwnershipTransition(before, event)
	must(t, err)
	if after.Retirement == nil || *after.Retirement != *before.Retirement {
		t.Fatal("recovery lost retirement")
	}
	stage := ownershipTransition{Schema: 1, Identity: after.Identity, Producer: producer, Revision: after.Revision + 1, Previous: after.History, BeforeHash: protocol.Hash(after), Operation: "stage", Generation: "new"}
	if _, err = applyOwnershipTransition(after, stage); err == nil {
		t.Fatal("recovery reopened retired generation")
	}
	// An absent producer may be fenced before its challenged summary arrives.
	// Completing that later summary must retain the independent retirement.
	event.Schema, event.Operation, event.Recovery.SummaryHash = 3, "recovery-fence", ""
	pending, err := applyOwnershipTransition(before, event)
	must(t, err)
	if pending.Retirement == nil || *pending.Retirement != *before.Retirement || pending.RecoveryPending == nil {
		t.Fatal("pending recovery lost retirement")
	}
	event.Identity, event.Revision, event.Previous, event.BeforeHash = pending.Identity, pending.Revision+1, pending.History, protocol.Hash(pending)
	event.Operation, event.Recovery.SummaryHash = "recover", protocol.Hash("later-summary")
	completed, err := applyOwnershipTransition(pending, event)
	must(t, err)
	if completed.Retirement == nil || *completed.Retirement != *before.Retirement || completed.RecoveryPending != nil {
		t.Fatal("completed recovery changed retirement")
	}
	stage.Identity, stage.Revision, stage.Previous, stage.BeforeHash = completed.Identity, completed.Revision+1, completed.History, protocol.Hash(completed)
	if _, err = applyOwnershipTransition(completed, stage); err == nil {
		t.Fatal("completed pending recovery reopened retired generation")
	}
}

func TestProducerRetirementLEDRemainsBaselineAfterSilence(t *testing.T) {
	s, now, _ := bindingFixture(t, binding("selected", "backup"))
	ctx := context.Background()
	api := &fakeLED{states: map[string]led.State{}}
	r := LEDReconciler{Store: s, API: api, Budget: time.Second}
	v := snapshot(*now, 1, 1)
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	v.Sequence, v.BasisRevision, v.Subjects[0].Condition = 2, 2, "retired"
	must(t, s.Replace(ctx, v))
	must(t, r.Step(ctx))
	must(t, s.Revoke(ctx, producer, "generation-one"))
	_, err := s.RetireProducer(ctx, producer, "operator", "Source and outputs decommissioned", true)
	must(t, err)
	*now = now.Add(24 * time.Hour)
	must(t, r.Step(ctx))
	h, err := s.Health(ctx)
	must(t, err)
	if h.Condition != "retired" || h.Bindings["selected"].Condition != "retired" {
		t.Fatal("retired producer required a heartbeat", h)
	}
	if len(api.states) != 2 {
		t.Fatal("retirement left unexpected daemon states", api.states)
	}
	for _, state := range api.states {
		if !strings.HasSuffix(state.ID, ".baseline") || !state.Persistent || state.TTL != nil || state.Pattern == "heartbeat" {
			t.Fatal("retired output acquired a healthy lease or fault", state)
		}
	}
}

func TestProducerRetirementDoesNotHideOtherProducerFaults(t *testing.T) {
	c := configuration()
	other := c.Producers[0]
	other.Producer.ID = "another"
	c.Producers = append(c.Producers, other)
	path := filepath.Join(t.TempDir(), "state")
	must(t, os.Mkdir(path, 0700))
	s, err := Open(path, c, true)
	must(t, err)
	defer s.Close()
	now := time.Now().UTC()
	s.now = func() time.Time { return now }
	for _, e := range c.Producers {
		must(t, s.Stage(e.Producer, "generation-one"))
		must(t, s.Activate(e.Producer, "generation-one"))
	}
	v := snapshot(now, 1, 1)
	v.Subjects[0].Condition = "retired"
	must(t, s.Replace(context.Background(), v))
	must(t, s.Revoke(context.Background(), producer, "generation-one"))
	_, err = s.RetireProducer(context.Background(), producer, "operator", "Decommission", true)
	must(t, err)
	v = snapshot(now, 1, 1)
	v.Producer = other.Producer
	must(t, s.Replace(context.Background(), v))
	h, err := s.Health(context.Background())
	must(t, err)
	if h.Condition != "ok" || len(h.RetiredProducers) != 1 {
		t.Fatal(h)
	}
	v.Sequence, v.BasisRevision, v.Subjects[0].Condition = 2, 2, "critical"
	must(t, s.Replace(context.Background(), v))
	api := &fakeLED{states: map[string]led.State{}}
	must(t, (&LEDReconciler{Store: s, API: api, Budget: time.Second}).Step(context.Background()))
	h, err = s.Health(context.Background())
	must(t, err)
	if h.Condition != "critical" {
		t.Fatal("retirement hid unrelated fault", h)
	}
	must(t, s.Close())
	r, err := Open(path, c, false)
	must(t, err)
	r.Close()
}

func TestProducerRetirementLegacyEncodingAndReasonBounds(t *testing.T) {
	s, _, _ := fixture(t)
	raw, err := json.Marshal(readOwner(t, s))
	must(t, err)
	if strings.Contains(string(raw), "retirement") {
		t.Fatal("legacy encoding changed")
	}
	for _, reason := range []string{"", " ", " leading", "two\nlines", strings.Repeat("x", 1025), "bad\x00reason"} {
		if ValidateProducerRetirementReason("operator", reason) == nil {
			t.Fatal("invalid reason accepted")
		}
	}
}

func TestProducerRetirementKeepsAcknowledgedHistoryThroughCompaction(t *testing.T) {
	s, now, path, m, _ := receiptCompactionFixture(t)
	ctx := context.Background()
	v := snapshot(*now, 2, 2)
	v.Subjects[0].Condition = "retired"
	must(t, s.Replace(ctx, v))
	must(t, s.Revoke(ctx, producer, "generation-one"))
	retired, err := s.RetireProducer(ctx, producer, "operator", "Report imported and source retired", true)
	must(t, err)
	job, err := s.Get(ctx, producer, m.ID)
	must(t, err)
	if job.State != "provider_accepted" {
		t.Fatal("retirement changed delivery outcome")
	}
	compacted, err := s.CompactReceipts(ctx, producer, 128, true)
	must(t, err)
	if !compacted.Applied {
		t.Fatal("fixture did not compact old intermediate receipts")
	}
	must(t, s.verifyRetirements(ctx))
	c := s.config
	must(t, s.Close())
	r, err := Open(path, c, false)
	must(t, err)
	defer r.Close()
	again, err := r.RetireProducer(ctx, producer, "operator", "Report imported and source retired", true)
	must(t, err)
	if *again.Retirement != *retired.Retirement {
		t.Fatal("compaction lost retirement")
	}
}

func TestProducerRetirementRejectsCommandsAndPolicyButPreservesHistoricalReads(t *testing.T) {
	s, _, _, in := commandFixture(t)
	ctx := context.Background()
	v, err := s.AdmitCommand(ctx, in)
	must(t, err)
	// A settled read command with a settled reply has no remaining obligation.
	_, err = s.db.Exec("UPDATE commands SET state='succeeded'; UPDATE command_replies SET state='provider_accepted'")
	must(t, err)
	must(t, s.Revoke(ctx, producer, "generation-one"))
	_, err = s.RetireProducer(ctx, producer, "operator", "Worker decommissioned", true)
	must(t, err)
	prior, err := s.AdmitCommand(ctx, in)
	must(t, err)
	if prior.Command.ID != v.Command.ID || prior.State != "succeeded" {
		t.Fatal("historical replay changed result")
	}
	in.NativeID = "new-command"
	if _, err = s.AdmitCommand(ctx, in); err == nil {
		t.Fatal("retired worker admitted new command")
	}
	if _, err = s.GrantCommand(ctx, producer, v.Command.ID); err == nil {
		t.Fatal("retired worker granted command")
	}
	in.Text = "command " + v.Command.ID
	handled, err := s.AdmitGatewayQuery(ctx, in)
	must(t, err)
	if !handled {
		t.Fatal("historical status query unavailable")
	}
	in.NativeID, in.Text = "retired-workers", "workers"
	if reply := queryReply(t, s, in); !strings.Contains(reply.Text, "backup/storage (retired)") {
		t.Fatal("worker listing did not explain retirement", reply.Text)
	}
	in.NativeID, in.Text = "retired-help", "backup/storage help"
	if reply := queryReply(t, s, in); !strings.Contains(reply.Text, "permanently retired") || strings.Contains(reply.Text, "backup/storage status:") {
		t.Fatal("help advertised retired worker commands", reply.Text)
	}
	s.config.Interaction.Permissions = nil
	in.NativeID, in.Text = "unauthorized-retired-help", "backup/storage help"
	if _, err = s.AdmitGatewayQuery(ctx, in); err == nil {
		t.Fatal("retirement help disclosed unauthorized worker")
	}
	if _, err = s.SubmitCommandResult(ctx, producer, interaction.Result{CommandID: v.Command.ID, CommandHash: protocol.Hash(v.Command), Phase: "execute", State: "succeeded", Revision: 1, Scope: v.Command.Scope}); err == nil {
		t.Fatal("retired worker changed final outcome")
	}
	must(t, s.verifyRetirements(ctx))
	p, now, _ := policyFixture(t)
	must(t, p.Revoke(ctx, producer, "generation-one"))
	_, err = p.RetireProducer(ctx, producer, "operator", "Policy worker decommissioned", true)
	must(t, err)
	batch := protocol.TransitionBatch{Producer: producer, Records: []protocol.FaultTransition{transition(*now, "late-transition", 1, 1)}}
	if _, err = p.EnqueueTransitions(ctx, batch); err == nil {
		t.Fatal("retired worker admitted transition")
	}
}
