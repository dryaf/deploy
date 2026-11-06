package install

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func TestRecoveryIntakeCLIDryRunAndExplicitScope(t *testing.T) {
	spec, _, _, _, _ := managedCommandsFixture(t)
	operation, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	state := filepath.Join(t.TempDir(), "missing-state")
	for _, test := range []struct {
		name  string
		extra []string
	}{
		{"recovery-telegram-history", []string{"--from", filepath.Join(t.TempDir(), "missing-history"), "--snapshot-id", operation, "--database-hash", protocol.Hash("snapshot")}},
		{"recovery-telegram-drain", []string{"--receivers-stopped"}},
		{"recovery-requests-seal", []string{"--reconciliation-complete"}},
		{"recovery-telegram-cursor", nil},
	} {
		base := []string{test.name, "--local", "--config", spec.RuntimeFile, "--state-dir", state, "--operation", operation}
		var out bytes.Buffer
		if err := runRecoveryIntakeCLIWith(append(base, test.extra...), &out, nil); err != nil {
			t.Fatal(test.name, err)
		}
		if !bytes.Contains(out.Bytes(), []byte("[DRY]")) {
			t.Fatal(out.String())
		}
		if _, err := os.Stat(state); !os.IsNotExist(err) {
			t.Fatal("dry run touched recovery state", err)
		}
		if err := runRecoveryIntakeCLIWith(append(append([]string{}, base...), "--unknown"), &out, nil); err == nil {
			t.Fatal("unknown flag accepted")
		}
		if test.name != "recovery-telegram-cursor" && runRecoveryIntakeCLIWith(base, &out, nil) == nil {
			t.Fatal("missing explicit scope accepted", test.name)
		}
	}
}
