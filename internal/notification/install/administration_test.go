package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func TestMessageAdministrationCLIReviewDecideAndStatus(t *testing.T) {
	_, config := runtimeFixture(t)
	c, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err = os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(state, c.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Worker.Producers[0].Producer
	for _, err := range []error{s.Stage(p, "one"), s.Activate(p, "one")} {
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	m := protocol.Message{Producer: p, Sequence: 1, ID: "cli-review", Generation: "one", Subject: "subject", Kind: "failure", Severity: "critical", ObservedAt: now, ExpiresAt: now.Add(time.Hour), Text: "Synthetic failure", Route: "ops"}
	m.Incidents = []protocol.Incident{{Subject: m.Subject, ID: "synthetic-incident", Revision: 1}}
	_, err = s.Enqueue(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	j, granted, err := s.Grant(context.Background(), p, m.ID)
	if err != nil || !granted {
		t.Fatal(err, granted)
	}
	if err = s.Finish(context.Background(), j, worker.Result{Ambiguous: true}); err != nil {
		t.Fatal(err)
	}
	args := []string{"message-review", "--local", "--config", config, "--state-dir", state, "--producer", p.ID, "--message", m.ID}
	var out bytes.Buffer
	if err = runMessageAdministrationCLI(args, &out); err == nil {
		t.Fatal("administration bypassed live worker lease")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = runMessageAdministrationCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	var review worker.MessageReview
	if err = json.Unmarshal(out.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "review.json")
	if err = os.WriteFile(path, out.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	decide := []string{"message-decide", "--local", "--config", config, "--state-dir", state, "--review", path, "--action", "retry", "--reason", "Reviewed uncertain result"}
	out.Reset()
	if err = runMessageAdministrationCLI(decide, &out); err == nil {
		t.Fatal("CLI retried without duplicate consent")
	}
	decide = append(decide, "--accept-duplicate-risk")
	old := ui.DryRun
	ui.DryRun = true
	err = runMessageAdministrationCLI(decide, &out)
	ui.DryRun = old
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(state, "administration")); !os.IsNotExist(err) {
		t.Fatal("dry-run persisted decision")
	}
	out.Reset()
	if err = runMessageAdministrationCLI(decide, &out); err != nil {
		t.Fatal(err)
	}
	var record worker.MessageDecisionRecord
	if err = json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Receipt.State != "queued" || record.Receipt.Acceptance != nil {
		t.Fatal(record)
	}
	out.Reset()
	if err = runMessageAdministrationCLI([]string{"message-decision-status", "--local", "--config", config, "--state-dir", state, "--decision", review.ID}, &out); err != nil {
		t.Fatal(err)
	}
	var history worker.MessageDecisionRecord
	if err = json.Unmarshal(out.Bytes(), &history); err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(history) != protocol.Hash(record) {
		t.Fatal("status changed decision")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = readMessageReview(path); err == nil {
		t.Fatal("public review accepted")
	}
	if err = runMessageAdministrationCLI(append(args, "--action", "cancel"), &out); err == nil {
		t.Fatal("review accepted mutation flags")
	}
}
