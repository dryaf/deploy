package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func quarantineCLIArguments(t *testing.T) (string, worker.Config, []string) {
	t.Helper()
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
	defer s.Close()
	from := filepath.Join(t.TempDir(), "snapshot")
	m, err := s.Snapshot(context.Background(), from)
	if err != nil {
		t.Fatal(err)
	}
	return state, c.Worker, []string{"--local", "--config", config, "--state-dir", state, "--from", from, "--snapshot-id", m.ID, "--database-hash", m.DatabaseHash}
}

func TestRecoveryQuarantineCLIAndRetry(t *testing.T) {
	state, c, args := quarantineCLIArguments(t)
	var first, replay bytes.Buffer
	if err := runRecoveryQuarantineCLI(args, &first); err != nil {
		t.Fatal(err)
	}
	if err := runRecoveryQuarantineCLI(args, &replay); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), replay.Bytes()) {
		t.Fatal("lost result cannot be retried")
	}
	var view worker.RecoveryQuarantineView
	if err := json.Unmarshal(first.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !safefs.ValidID(view.Operation, "n") || view.Phase != "quarantined" || view.DeliveryEnabled || !safefs.DigestPattern.MatchString(view.OriginalSourceHash) {
		t.Fatal(view)
	}
	if s, err := worker.Open(state, c, false); err == nil {
		s.Close()
		t.Fatal("preparation enabled runtime")
	}
}

func TestRecoveryQuarantineCLIFlagsAndDryRun(t *testing.T) {
	_, config := runtimeFixture(t)
	state, from := filepath.Join(t.TempDir(), "missing-state"), filepath.Join(t.TempDir(), "missing-snapshot")
	id, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--local", "--config", config, "--state-dir", state, "--from", from, "--snapshot-id", id, "--database-hash", strings.Repeat("a", 64)}
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	var out bytes.Buffer
	if err := runRecoveryQuarantineCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("no state or snapshot opened")) {
		t.Fatal(out.String())
	}
	if err := RunCLI(append([]string{"recovery-prepare"}, args...)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{{"--database-hash", ""}, {"--database-hash", "invalid"}, {"--local=false"}, {"--snapshot-id", ""}, {"--snapshot-id", "invalid"}, {"--from", "relative"}, {"--from", state}, {"--state-dir", from + "/nested"}, {"--activate"}, {"positional"}} {
		if err := runRecoveryQuarantineCLI(append(append([]string{}, args...), bad...), &out); err == nil {
			t.Fatal("unsafe scope accepted", bad)
		}
	}
	for _, path := range []string{state, from} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("dry run created state", err)
		}
	}
}
