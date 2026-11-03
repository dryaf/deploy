package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func TestReceiptCompactionCLIRequiresStoppedStateAndExplicitApply(t *testing.T) {
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
	args := []string{"receipts-compact", "--local", "--config", config, "--state-dir", state, "--producer", c.Worker.Producers[0].Producer.ID}
	var out bytes.Buffer
	if err = runReceiptCompactionCLI(args, &out); err == nil {
		t.Fatal("compaction bypassed live worker lease")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{nil, {"--apply"}} {
		out.Reset()
		if err = runReceiptCompactionCLI(append(append([]string{}, args...), extra...), &out); err != nil {
			t.Fatal(err)
		}
		var result worker.ReceiptCompaction
		if err = json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Applied || len(result.Candidates) != 0 || result.RetentionDays != 90 {
			t.Fatal(result)
		}
	}
	if err = runReceiptCompactionCLI(append(args, "--limit", "1025"), &out); err == nil {
		t.Fatal("unbounded pass accepted")
	}
	if err = runReceiptCompactionCLI(append(args, "--action", "cancel"), &out); err == nil {
		t.Fatal("unrelated option accepted")
	}
	// Dry-run needs no state and must not create a replacement for a missing DB.
	missing := filepath.Join(t.TempDir(), "missing")
	old := ui.DryRun
	ui.DryRun = true
	err = runReceiptCompactionCLI([]string{"receipts-compact", "--local", "--config", config, "--state-dir", missing, "--producer", c.Worker.Producers[0].Producer.ID, "--apply"}, &out)
	ui.DryRun = old
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("dry-run created state", err)
	}
	// The operation is local administration, never a producer API action.
	s, err = worker.Open(state, c.Worker, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, err = s.CompactReceipts(context.Background(), c.Worker.Producers[0].Producer, 0, true)
	if err == nil {
		t.Fatal("invalid internal range accepted")
	}
}
