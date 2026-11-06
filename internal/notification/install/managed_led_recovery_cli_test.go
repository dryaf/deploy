package install

import (
	"bytes"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/ui"
	"path/filepath"
	"testing"
)

func TestManagedLEDRecoveryCLIScopeAndDryRun(t *testing.T) {
	previous := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = previous }()
	base := []string{"--local", "--root", filepath.Join(t.TempDir(), "absent"), "--manifest-hash", protocol.Hash("manifest")}
	var out bytes.Buffer
	for _, operation := range []string{"led-recovery-review", "led-recovery-clear"} {
		args := append([]string{operation}, base...)
		if operation == "led-recovery-clear" {
			args = append(args, "--fault", "old-fault", "--review-hash", protocol.Hash("review"))
		}
		if err := runRuntimeLEDRecoveryCLI(args, &out); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"led-recovery-clear"}, {"led-recovery-review", "--local"}, append(append([]string{"led-recovery-review"}, base...), "--fault", "foreign"), append([]string{"led-recovery-clear"}, base...)} {
		if err := runRuntimeLEDRecoveryCLI(args, &out); err == nil {
			t.Fatal("invalid scope accepted", args)
		}
	}
}
