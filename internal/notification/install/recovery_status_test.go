package install

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func recoveryStatusArguments(t *testing.T) (string, []string) {
	t.Helper()
	state, _, flags := quarantineCLIArguments(t)
	var prepared bytes.Buffer
	if err := runRecoveryQuarantineCLI(flags, &prepared); err != nil {
		t.Fatal(err)
	}
	var view worker.RecoveryQuarantineView
	if err := json.Unmarshal(prepared.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	var config string
	for i, v := range flags {
		if v == "--config" {
			config = flags[i+1]
		}
	}
	return state, []string{"--local", "--config", config, "--state-dir", state, "--operation", view.Operation}
}
func TestRecoveryStatusCLI(t *testing.T) {
	_, args := recoveryStatusArguments(t)
	var first, again bytes.Buffer
	if err := runRecoveryStatusCLI(args, &first); err != nil {
		t.Fatal(err)
	}
	if err := runRecoveryStatusCLI(args, &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), again.Bytes()) {
		t.Fatal("review changed without evidence changes")
	}
	var v worker.RecoveryStatus
	if err := json.Unmarshal(first.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.PublicationAllowed || v.DeliveryEnabled || v.ExecutionEnabled || !safefs.DigestPattern.MatchString(v.ReviewHash) || v.PendingStages == 0 {
		t.Fatal(v)
	}
}
func TestRecoveryStatusCLIFlagsAndDryRun(t *testing.T) {
	state, args := recoveryStatusArguments(t)
	for _, kind := range []string{"missing-local", "relative-path", "missing-operation", "foreign-operation", "positional", "unknown-flag"} {
		t.Run(kind, func(t *testing.T) {
			a := append([]string(nil), args...)
			switch kind {
			case "missing-local":
				a = a[1:]
			case "relative-path":
				a[4] = "relative"
			case "missing-operation":
				a = a[:len(a)-2]
			case "foreign-operation":
				a[len(a)-1], _ = safefs.NewID("n")
			case "positional":
				a = append(a, "unexpected")
			case "unknown-flag":
				a = append(a, "--publish")
			}
			var out bytes.Buffer
			if err := runRecoveryStatusCLI(a, &out); err == nil {
				t.Fatal("invalid input accepted")
			}
			if out.Len() != 0 {
				t.Fatal("invalid input emitted a success report")
			}
		})
	}
	saved := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = saved }()
	missing := filepath.Join(state, "absent")
	args[4] = missing
	var out bytes.Buffer
	if err := runRecoveryStatusCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("dry-run created state", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("[DRY]")) {
		t.Fatal(out.String())
	}
}
