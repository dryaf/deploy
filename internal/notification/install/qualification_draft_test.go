package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestGuidedQualificationResumesAndPreservesObservationWindow(t *testing.T) {
	spec, _, _, template := qualificationFixture(t)
	ctx := context.Background()
	pin := protocol.Hash(template.Binding.Manifest)
	now := time.Now().UTC()
	begin := func(at time.Time) QualificationSession {
		s, err := qualificationDraftOperation(ctx, spec.Root, pin, "begin", QualificationObservation{}, at)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := begin(now)
	first := s.Checks[0]
	observation := QualificationObservation{Session: s.ID, Scope: first.Scope, ID: first.ID, Result: "passed", EvidenceHash: protocol.Hash("operator test artifact")}
	if _, err := qualificationDraftOperation(ctx, spec.Root, pin, "observe", observation, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := qualificationDraftOperation(ctx, spec.Root, pin, "observe", observation, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var draft qualificationDraft
	if err = d.ReadJSON("qualification-draft.json", &draft); err != nil || !draft.Report.Checks[0].ObservedAt.Equal(now.Add(time.Second)) {
		t.Fatal("retry renewed observation time", draft, err)
	}
	s = begin(now.Add(3 * time.Second))
	if s.ID != observation.Session || s.Checks[0].Result != "passed" || s.Recorded || s.EvidenceComplete {
		t.Fatal(s)
	}
	changed := observation
	changed.Result = "failed"
	if _, err = qualificationDraftOperation(ctx, spec.Root, pin, "observe", changed, now.Add(4*time.Second)); err == nil {
		t.Fatal("rewrote observation")
	}
	record, err := qualificationDraftOperation(ctx, spec.Root, pin, "commit", QualificationObservation{Session: s.ID}, now.Add(5*time.Second))
	if err != nil || !record.Recorded || record.EvidenceComplete {
		t.Fatal(record, err)
	}
	next := begin(now.Add(6 * time.Second))
	if next.ID == s.ID || next.Checks[0].Result != "passed" || !next.CreatedAt.Equal(s.CreatedAt) {
		t.Fatal("lost observation or renewed evidence window", next)
	}
	for _, check := range next.Checks {
		if check.Result == "passed" {
			continue
		}
		_, err = qualificationDraftOperation(ctx, spec.Root, pin, "observe", QualificationObservation{Session: next.ID, Scope: check.Scope, ID: check.ID, Result: "passed", EvidenceHash: protocol.Hash("synthetic fixture observation")}, now.Add(7*time.Second))
		if err != nil {
			t.Fatal(err)
		}
	}
	record, err = qualificationDraftOperation(ctx, spec.Root, pin, "commit", QualificationObservation{Session: next.ID}, now.Add(8*time.Second))
	if err != nil || !record.Recorded || !record.EvidenceComplete {
		t.Fatal(record, err)
	}
	again := begin(now.Add(9 * time.Second))
	if again.ID != next.ID || !again.Recorded || !again.EvidenceComplete {
		t.Fatal(again)
	}
	stale := begin(now.Add(qualificationWindow))
	if stale.ID == next.ID || stale.EvidenceComplete {
		t.Fatal("renewed stale qualification", stale)
	}
	for _, check := range stale.Checks {
		if check.Result != "pending" {
			t.Fatal("reused stale observation", check)
		}
	}
	raw, _ := json.Marshal(record)
	for _, secret := range []string{pin, spec.Root, template.Binding.Inputs["provider/telegram/token"], observation.EvidenceHash} {
		if secret != "" && strings.Contains(string(raw), secret) {
			t.Fatal("session exposed private binding")
		}
	}
}

func TestGuidedQualificationPendingAuditAndInputDrift(t *testing.T) {
	spec, _, c, template := qualificationFixture(t)
	ctx := context.Background()
	pin := protocol.Hash(template.Binding.Manifest)
	s, err := BeginQualification(ctx, spec.Root, pin)
	if err != nil {
		t.Fatal(err)
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var draft qualificationDraft
	if err = d.ReadJSON("qualification-draft.json", &draft); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("lost audit acknowledgement")
	_, err = appendQualification(d, draft.Report, time.Now().UTC(), func(at string) error {
		if at == "qualification_record" {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatal("did not interrupt audit", err)
	}
	resumed, err := BeginQualification(ctx, spec.Root, pin)
	if err != nil || resumed.ID != s.ID || !resumed.AcknowledgementPending {
		t.Fatal(resumed, err)
	}
	recorded, err := CommitQualification(ctx, spec.Root, pin, resumed.ID)
	if err != nil || !recorded.Recorded || recorded.EvidenceComplete {
		t.Fatal(recorded, err)
	}
	current, err := BeginQualification(ctx, spec.Root, pin)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.Providers["telegram"].TokenFile, []byte("123:rotated-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = CommitQualification(ctx, spec.Root, pin, current.ID); err == nil {
		t.Fatal("committed observations against changed credentials")
	}
	changed, err := BeginQualification(ctx, spec.Root, pin)
	if err != nil || changed.ID == current.ID {
		t.Fatal(changed, err)
	}
	if _, err = os.Stat(filepath.Join(spec.Root, "qualification-drafts", current.ID+".json")); err != nil {
		t.Fatal("lost stale draft", err)
	}
	if _, err = ObserveQualification(ctx, spec.Root, pin, QualificationObservation{Session: current.ID, Scope: "installation", ID: "native-runtime", Result: "passed", EvidenceHash: protocol.Hash("old")}); err == nil {
		t.Fatal("retargeted old wizard")
	}
	if _, err = BeginQualification(ctx, spec.Root, protocol.Hash("changed installation")); err == nil {
		t.Fatal("mixed installation revisions")
	}
}

func TestGuidedQualificationCannotOverrideNewerFailedEvidence(t *testing.T) {
	spec, _, _, template := qualificationFixture(t)
	ctx := context.Background()
	pin := protocol.Hash(template.Binding.Manifest)
	session, err := BeginQualification(ctx, spec.Root, pin)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range session.Checks {
		if _, err = ObserveQualification(ctx, spec.Root, pin, QualificationObservation{Session: session.ID, Scope: check.Scope, ID: check.ID, Result: "passed", EvidenceHash: protocol.Hash("synthetic observation")}); err != nil {
			t.Fatal(err)
		}
	}
	recorded, err := CommitQualification(ctx, spec.Root, pin, session.ID)
	if err != nil || !recorded.EvidenceComplete {
		t.Fatal(recorded, err)
	}
	qualificationPass(&template)
	template.Checks[0].Result = "failed"
	if status, e := qualifyFixture(t, spec.Root, template); e != nil || status.EvidenceComplete {
		t.Fatal(status, e)
	}
	old, err := CommitQualification(ctx, spec.Root, pin, session.ID)
	if err != nil || old.EvidenceComplete {
		t.Fatal("historical retry hid newer failed evidence", old, err)
	}
	newSession, err := BeginQualification(ctx, spec.Root, pin)
	if err != nil || newSession.ID == session.ID || newSession.EvidenceComplete || newSession.Checks[0].Result != "pending" {
		t.Fatal(newSession, err)
	}
	if !newSession.CreatedAt.Equal(template.CreatedAt) {
		t.Fatal("renewed report window while retaining passing observations")
	}
}
