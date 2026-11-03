package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func recoveryCLIFixture(t *testing.T) (string, string, worker.RecoveryMessageReview) {
	t.Helper()
	_, config := runtimeFixture(t)
	c, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	path := t.TempDir()
	if err = os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(path, c.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := c.Worker.Producers[0].Producer
	if err = s.Stage(p, "one"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	m := protocol.Message{Producer: p, ID: "uncertain", Sequence: 1, Generation: "one", Subject: "fixture", Kind: "test", Severity: "warning", ObservedAt: now, Text: "Private delivery text", Route: "ops"}
	if _, err = s.Enqueue(ctx, m); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	if _, err = s.Snapshot(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	old := s.Identity()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = worker.PrepareRecoveryQuarantine(ctx, path, artifact, c.Worker); err != nil {
		t.Fatal(err)
	}
	recovery, err := worker.OpenRecoverySession(ctx, path, c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close()
	ch, err := recovery.Challenge(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	manifest := protocol.RecoveryManifest{HighestIssued: 1, HighestAcknowledged: 1, ImportedReceipts: protocol.Cursor{Identity: old, Producer: p}}
	if err = recovery.Begin(ctx, ch, manifest); err != nil {
		t.Fatal(err)
	}
	if err = recovery.Import(ctx, ch, []protocol.RecoveryRecord{{Message: m}}); err != nil {
		t.Fatal(err)
	}
	status := protocol.Snapshot{Producer: p, Sequence: 1, BasisRevision: 1, ConfigRevision: "one", EvaluatedAt: time.Now().UTC(), Subjects: []protocol.Subject{{Key: "fixture", Condition: "unknown", Reason: "recovery", ValidUntil: now.Add(time.Minute)}}}
	if _, err = recovery.Finish(ctx, ch, status); err != nil {
		t.Fatal(err)
	}
	if err = recovery.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = runRecoveryCLI([]string{"recovery-review", "--local", "--config", config, "--state-dir", path, "--producer", p.ID, "--message", m.ID}, &out); err != nil {
		t.Fatal(err)
	}
	var v worker.RecoveryMessageReview
	if err = json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return config, path, v
}
func writeReview(t *testing.T, v worker.RecoveryMessageReview) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review.json")
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestRecoveryCLIReviewDecideReplay(t *testing.T) {
	for _, action := range []string{"retry", "cancel"} {
		t.Run(action, func(t *testing.T) {
			config, path, v := recoveryCLIFixture(t)
			if v.DecisionState != "none" || v.DeliveryEnabled || v.Message.Text != "Private delivery text" {
				t.Fatal(v)
			}
			review := writeReview(t, v)
			args := []string{"recovery-decide", "--local", "--config", config, "--state-dir", path, "--review", review, "--action", action, "--reason", "Reviewed exact payload"}
			if action == "retry" {
				args = append(args, "--accept-duplicate-risk")
			}
			var out bytes.Buffer
			if err := runRecoveryCLI(args, &out); err != nil {
				t.Fatal(err)
			}
			var record worker.RecoveryDispositionRecord
			if err := json.Unmarshal(out.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			if record.Decision.ID != v.RequestID || record.Decision.Actor != fmt.Sprintf("uid:%d", os.Geteuid()) || record.DeliveryEnabled {
				t.Fatal(record)
			}
			first := out.String()
			out.Reset()
			if err := runRecoveryCLI(args, &out); err != nil {
				t.Fatal(err)
			}
			if first != out.String() {
				t.Fatal("lost CLI response changed decision")
			}
			out.Reset()
			if err := runRecoveryCLI([]string{"recovery-review", "--local", "--config", config, "--state-dir", path, "--producer", v.Message.Producer.ID, "--message", v.Message.ID}, &out); err != nil {
				t.Fatal(err)
			}
			var after worker.RecoveryMessageReview
			if err := json.Unmarshal(out.Bytes(), &after); err != nil {
				t.Fatal(err)
			}
			if after.DecisionState != "recorded" || after.RequestID != v.RequestID {
				t.Fatal(after)
			}
			c, err := ReadRuntime(config)
			if err != nil {
				t.Fatal(err)
			}
			ownership, err := worker.OpenRecoveryOwnershipSession(context.Background(), path, c.Worker)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = ownership.PrepareRecoveryOwnership(context.Background(), v.Message.Producer); err != nil {
				t.Fatal(err)
			}
			if err = ownership.Close(); err != nil {
				t.Fatal(err)
			}
			application, err := worker.OpenRecoveryApplicationSession(context.Background(), path, c.Worker)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = application.ApplyRecoveryMessage(context.Background(), v.Message.Producer, v.Message.ID); err != nil {
				t.Fatal(err)
			}
			if err = application.Close(); err != nil {
				t.Fatal(err)
			}
			if action == "retry" {
				generation, err := worker.OpenRecoveryGenerationSession(context.Background(), path, c.Worker)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = generation.PrepareRecoveryGeneration(context.Background(), v.Message.Producer); err != nil {
					t.Fatal(err)
				}
				if _, err = generation.BindRecoveryRetry(context.Background(), v.Message.Producer, v.Message.ID); err != nil {
					t.Fatal(err)
				}
				if err = generation.Close(); err != nil {
					t.Fatal(err)
				}
			}
			out.Reset()
			if err = runRecoveryCLI([]string{"recovery-review", "--local", "--config", config, "--state-dir", path, "--producer", v.Message.Producer.ID, "--message", v.Message.ID}, &out); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(out.Bytes(), &after); err != nil {
				t.Fatal(err)
			}
			if after.ApplicationState != "applied" || after.Application == nil {
				t.Fatal(after)
			}
			if action == "retry" && (after.RetryBindingState != "bound" || after.RetryBinding == nil) {
				t.Fatal(after)
			}
			if _, err = readRecoveryReview(writeReview(t, after)); err != nil {
				t.Fatal(err)
			}
			// A saved pre-application review still replays the original decision.
			out.Reset()
			if err = runRecoveryCLI(args, &out); err != nil {
				t.Fatal(err)
			}
			if out.String() != first {
				t.Fatal("application changed original CLI decision")
			}
			if normal, err := worker.Open(path, c.Worker, false); err == nil {
				normal.Close()
				t.Fatal("CLI enabled ordinary worker")
			}
		})
	}
}
func TestRecoveryCLIRejectsUnsafeOrUnreviewedChoices(t *testing.T) {
	config, path, v := recoveryCLIFixture(t)
	review := writeReview(t, v)
	base := []string{"recovery-decide", "--local", "--config", config, "--state-dir", path, "--review", review, "--action", "retry", "--reason", "Reviewed"}
	if err := runRecoveryCLI(base, &bytes.Buffer{}); err == nil {
		t.Fatal("retry accepted without duplicate-risk consent")
	}
	base = append(base, "--accept-duplicate-risk")
	for _, extra := range [][]string{{"--local=false"}, {"--actor", "someone-else"}, {"--producer", "fixture"}, {"--action", "send"}, {"--accept-duplicate-risk=false"}} {
		if err := runRecoveryCLI(append(append([]string{}, base...), extra...), &bytes.Buffer{}); err == nil {
			t.Fatal("unsafe choice accepted", extra)
		}
	}
	for _, mode := range []string{"payload", "epoch", "summary", "world_readable", "symlink", "hardlink"} {
		t.Run(mode, func(t *testing.T) {
			changed := v
			switch mode {
			case "payload":
				changed.Message.Text = "Edited after review"
			case "epoch":
				changed.Identity.Recovery = "another-epoch"
			case "summary":
				changed.SummaryHash = protocol.Hash("changed")
			}
			p := writeReview(t, changed)
			switch mode {
			case "world_readable":
				if err := os.Chmod(p, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink", "hardlink":
				alias := filepath.Join(t.TempDir(), "alias.json")
				var err error
				if mode == "symlink" {
					err = os.Symlink(p, alias)
				} else {
					err = os.Link(p, alias)
				}
				if err != nil {
					t.Fatal(err)
				}
				p = alias
			}
			args := []string{"recovery-decide", "--local", "--config", config, "--state-dir", path, "--review", p, "--action", "cancel", "--reason", "Reviewed"}
			if err := runRecoveryCLI(args, &bytes.Buffer{}); err == nil {
				t.Fatal("invalid review accepted")
			}
		})
	}
}
func TestRecoveryCLIDryRunDoesNotOpenOrChangeState(t *testing.T) {
	config, path, v := recoveryCLIFixture(t)
	review := writeReview(t, v)
	c, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	held, err := worker.OpenRecoveryDispositionSession(context.Background(), path, c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	args := []string{"recovery-decide", "--local", "--config", config, "--state-dir", path, "--review", review, "--action", "cancel", "--reason", "Reviewed"}
	var out bytes.Buffer
	if err = runRecoveryCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "current recovery state not checked") {
		t.Fatal(out.String())
	}
	after, err := held.ReviewRecoveryMessage(context.Background(), v.Message.Producer, v.Message.ID)
	if err != nil || after.DecisionState != "none" {
		t.Fatal("dry run wrote choice", after, err)
	}
	ui.DryRun = false
	if err = runRecoveryCLI(args, &bytes.Buffer{}); err == nil {
		t.Fatal("exclusive recovery lease ignored")
	}
}
