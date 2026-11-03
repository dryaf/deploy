//go:build notificationintegration

package install

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

// Exercise the public command routing in the actual built executable against
// state left by the managed lifecycle. There is no dispatcher or provider I/O.
func testNativeMessageAdministration(t *testing.T, binary, config, state string, p protocol.Producer, event string) {
	t.Helper()
	run := func(command string, args ...string) []byte {
		t.Helper()
		base := []string{"notification-worker", command, "--local", "--config", config, "--state-dir", state}
		out, err := process.LimitedOutput(exec.Command(binary, append(base, args...)...), 20*time.Second, protocol.MaxResponse)
		if err != nil {
			t.Fatal(command, err, string(out))
		}
		return out
	}
	raw := run("message-review", "--producer", p.ID, "--message", event)
	var review worker.MessageReview
	if err := json.Unmarshal(raw, &review); err != nil {
		t.Fatal(err, string(raw))
	}
	file := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--review", file, "--action", "cancel", "--reason", "Synthetic operator cancellation"}
	raw = run("message-decide", args...)
	var original worker.MessageDecisionRecord
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err, string(raw))
	}
	if original.Receipt.State != "cancelled" || original.Receipt.Acceptance != nil || original.Decision.Review.ID != review.ID {
		t.Fatal("incorrect administrative completion", original)
	}
	for _, raw := range [][]byte{run("message-decide", args...), run("message-decision-status", "--decision", review.ID)} {
		var record worker.MessageDecisionRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err, string(raw))
		}
		if protocol.Hash(record) != protocol.Hash(original) {
			t.Fatal("executable retry/status changed original decision")
		}
	}
	var compact worker.ReceiptCompaction
	if err := json.Unmarshal(run("receipts-compact", "--producer", p.ID, "--apply"), &compact); err != nil {
		t.Fatal(err)
	}
	if compact.Applied || len(compact.Candidates) != 0 {
		t.Fatal("fresh unacknowledged event compacted")
	}
}
