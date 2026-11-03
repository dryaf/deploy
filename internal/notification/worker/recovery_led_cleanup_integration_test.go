//go:build notificationintegration

package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

// The real pinned daemon supplies UID-scoped registry semantics; hardware stays
// mock. Test crash boundaries separately with deterministic injected transports.
func TestRecoveryLEDCleanupPinnedDaemon(t *testing.T) {
	ctx := context.Background()
	socket := ledfixture.Start(t)
	api := led.Client{Socket: socket, Budget: time.Second}
	must(t, api.Set(ctx, led.State{ID: "another.application", LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true, Message: "preserve this"}))
	f := newLEDEvidenceFixtureWithRegistry(t, true, false, socket, api, func(c *Config) {
		// LED-only enrollment needs no messaging credentials or route resolution.
		c.Routes = nil
		c.Producers[0].Routes = nil
	})
	// Publication preparation now pins a standalone original-source archive.
	// The real daemon workflow must keep all older evidence valid across it.
	_, err := f.r.preserveOriginal(ctx)
	must(t, err)
	_, err = f.r.SubmitLED(ctx, f.challenge, f.report)
	must(t, err)
	view, err := f.r.reviewLEDInventory(ctx, socket, nil, false)
	must(t, err)
	proposal := view.Resolutions["selected"]
	if !proposal.DomainResolved {
		t.Fatal(proposal)
	}
	preserved, err := f.r.preserveLED(ctx, socket, f.inventory.InventoryID, f.inventory.InventoryHash, api)
	must(t, err)
	adopted, err := f.r.adoptLED(ctx, socket, RecoveryLEDAdoptionRequest{f.inventory.InventoryID, f.inventory.InventoryHash, preserved.RecordHash}, api)
	must(t, err)
	epoch, err := f.r.handoffLED(ctx, socket, adopted.RecordHash)
	must(t, err)
	result, err := f.r.clearLEDFault(ctx, socket, f.inventory.InventoryID, "selected", f.old, proposal.EvidenceHash, api, acceptLEDDomain)
	must(t, err)
	if result.Outcome != "acknowledged" {
		t.Fatal(result)
	}
	entries, err := api.List(ctx)
	must(t, err)
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.ID] = true
	}
	if seen[f.old] || !seen[f.new] || !seen["another.application"] {
		t.Fatal(entries)
	}
	// A fresh read reports actual historical cleanup, not a visual assertion.
	view, err = f.r.reviewLEDInventory(ctx, socket, nil, false)
	must(t, err)
	if view.CleanupAttempts != 1 || view.CleanupUnconfirmed != 0 || view.VisualVerified || view.ActivationAllowed {
		t.Fatal(view)
	}
	writes := protocol.Hash(entries)
	_, err = f.r.clearLEDFault(ctx, socket, f.inventory.InventoryID, "selected", f.old, proposal.EvidenceHash, api, acceptLEDDomain)
	must(t, err)
	entries, err = api.List(ctx)
	must(t, err)
	if protocol.Hash(entries) != writes {
		t.Fatal("retry mutated daemon")
	}
	view, err = f.r.reviewLEDInventory(ctx, socket, nil, false)
	must(t, err)
	if len(view.ImportableCleanup) != 1 || view.ImportableCleanup[0].ResultHash != protocol.Hash(result) {
		t.Fatal("completed clear not offered for exact import", view.ImportableCleanup)
	}
	must(t, f.r.Close())
	dir := t.TempDir()
	must(t, os.Chmod(dir, 0700))
	configuration := filepath.Join(dir, "runtime.json")
	raw, err := json.Marshal(struct {
		Version   int    `json:"version"`
		Worker    Config `json:"worker"`
		LEDSocket string `json:"led_socket"`
	}{1, f.config, socket})
	must(t, err)
	must(t, os.WriteFile(configuration, raw, 0600))
	binary := filepath.Join(dir, "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal("build", err, string(out))
	}
	cmd := exec.Command(binary, "notification-worker", "recovery-led-import-cleanup", "--local", "--config", configuration, "--state-dir", f.path, "--epoch-hash", epoch.RecordHash, "--result-hash", protocol.Hash(result))
	out, err := process.LimitedOutput(cmd, 15*time.Second, 16384)
	if err != nil {
		t.Fatal("native import", err, string(out))
	}
	var imported RecoveryLEDImportView
	must(t, json.Unmarshal(out, &imported))
	if imported.State != "committed" || imported.FaultID != f.old || imported.ActivationAllowed || imported.ClearAllowed {
		t.Fatal(imported)
	}
	retried, err := ImportRecoveryLEDCleanup(ctx, f.path, f.config, socket, epoch.RecordHash, protocol.Hash(result))
	must(t, err)
	if retried != imported {
		t.Fatal("public import retry changed receipt")
	}
	entries, err = api.List(ctx)
	must(t, err)
	if protocol.Hash(entries) != writes {
		t.Fatal("import changed pinned daemon registry")
	}
}
