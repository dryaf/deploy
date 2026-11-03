//go:build notificationintegration

package install

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
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

func TestRecoveryLEDPreservationAndAdoptionNativeExecutable(t *testing.T) {
	ctx := context.Background()
	socket := ledfixture.Start(t)
	api := led.Client{Socket: socket, Budget: time.Second}
	if err := api.Set(ctx, led.State{ID: "other.application", LED: "green", Pattern: "blink-fast", Priority: 90, Persistent: true}); err != nil {
		t.Fatal(err)
	}
	c, config := runtimeFixture(t)
	c.LEDSocket = socket
	writeRuntime(t, config, c)
	runtime, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	if err = os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(state, runtime.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	// Produce a real owned unknown fault before taking the snapshot, using the
	// normal reconciler against the pinned daemon's mock driver.
	reconciler := worker.LEDReconciler{Store: s, API: api, Budget: time.Second}
	if err = reconciler.Step(ctx); err != nil {
		s.Close()
		t.Fatal("seed owned fault", err)
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	if _, err = s.Snapshot(ctx, artifact); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = worker.PrepareRecoveryQuarantine(ctx, state, artifact, runtime.Worker); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal("build", err, string(out))
	}
	run := func(operation string, extra ...string) []byte {
		args := []string{"notification-worker", operation, "--local", "--config", config, "--state-dir", state}
		args = append(args, extra...)
		out, err := process.LimitedOutput(exec.Command(binary, args...), 15*time.Second, 32768)
		if err != nil {
			t.Fatal(operation, err, string(out))
		}
		return out
	}
	var review worker.RecoveryLEDReview
	if err = json.Unmarshal(run("recovery-led-review"), &review); err != nil {
		t.Fatal(err)
	}
	before, err := api.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var view worker.RecoveryLEDPreservationView
	options := []string{"--inventory", review.InventoryID, "--inventory-hash", review.InventoryHash}
	if err = json.Unmarshal(run("recovery-led-preserve", options...), &view); err != nil {
		t.Fatal(err)
	}
	if view.State != "committed" || view.Faults != 1 || view.UnownedStates != 1 || view.ActivationAllowed || view.ClearAllowed {
		t.Fatal(view)
	}
	var repeated worker.RecoveryLEDPreservationView
	if err = json.Unmarshal(run("recovery-led-preserve", options...), &repeated); err != nil || repeated != view {
		t.Fatal(repeated, err)
	}
	if err = json.Unmarshal(run("recovery-led-review"), &review); err != nil || review.Preservation == nil || *review.Preservation != view {
		t.Fatal(review.Preservation, err)
	}
	var adopted, adoptionRetry worker.RecoveryLEDAdoptionView
	adoptionOptions := append(append([]string{}, options...), "--preservation-hash", view.RecordHash)
	if err = json.Unmarshal(run("recovery-led-adopt", adoptionOptions...), &adopted); err != nil {
		t.Fatal(err)
	}
	if adopted.State != "committed" || adopted.Faults != 1 || !adopted.Quarantined || adopted.ActivationAllowed || adopted.ClearAllowed {
		t.Fatal(adopted)
	}
	if err = json.Unmarshal(run("recovery-led-adopt", adoptionOptions...), &adoptionRetry); err != nil || adopted != adoptionRetry {
		t.Fatal(adoptionRetry, err)
	}
	if err = json.Unmarshal(run("recovery-led-review"), &review); err != nil || review.Adoption == nil || *review.Adoption != adopted {
		t.Fatal(review.Adoption, err)
	}
	var epoch, epochRetry worker.RecoveryLEDEpochView
	epochOptions := []string{"--adoption-hash", adopted.RecordHash}
	if err = json.Unmarshal(run("recovery-led-handoff", epochOptions...), &epoch); err != nil {
		t.Fatal(err)
	}
	if epoch.State != "committed" || epoch.Faults != 1 || !epoch.Quarantined || epoch.ActivationAllowed || epoch.ClearAllowed {
		t.Fatal(epoch)
	}
	if err = json.Unmarshal(run("recovery-led-handoff", epochOptions...), &epochRetry); err != nil || epoch != epochRetry {
		t.Fatal(epochRetry, err)
	}
	if err = json.Unmarshal(run("recovery-led-review"), &review); err != nil || review.EpochHandoff == nil || *review.EpochHandoff != epoch || !review.SourcesMatch {
		t.Fatal(review.EpochHandoff, err)
	}
	if err = json.Unmarshal(run("recovery-led-adopt", adoptionOptions...), &adoptionRetry); err != nil || adopted != adoptionRetry {
		t.Fatal(adoptionRetry, err)
	}
	after, err := api.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(before) != protocol.Hash(after) {
		t.Fatal("native preservation mutated registry")
	}
	if active, err := worker.Open(state, runtime.Worker, false); err == nil {
		active.Close()
		t.Fatal("native preservation activated worker")
	}
}
