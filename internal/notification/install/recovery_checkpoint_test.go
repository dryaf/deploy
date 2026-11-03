package install

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func recoveryCheckpointArguments(t *testing.T) (string, []string) {
	t.Helper()
	state, args := recoveryStatusArguments(t)
	var out bytes.Buffer
	if err := runRecoveryStatusCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	var review worker.RecoveryStatus
	if err := json.Unmarshal(out.Bytes(), &review); err != nil {
		t.Fatal(err)
	}
	return state, append(args, "--review-hash", review.ReviewHash)
}

func TestRecoveryCheckpointCLI(t *testing.T) {
	_, args := recoveryCheckpointArguments(t)
	var first, replay bytes.Buffer
	if err := runRecoveryCheckpointCLI(args, &first); err != nil {
		t.Fatal(err)
	}
	if err := runRecoveryCheckpointCLI(args, &replay); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), replay.Bytes()) {
		t.Fatal("checkpoint replay changed")
	}
	var v worker.RecoveryCheckpoint
	if err := json.Unmarshal(first.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.PublicationAllowed || v.ExecutionEnabled || v.DeliveryEnabled || v.DatabaseBytes <= 0 || v.CheckpointHash == "" {
		t.Fatal(v)
	}
}

func TestRecoveryCheckpointCLIFlagsAndDryRun(t *testing.T) {
	state, args := recoveryCheckpointArguments(t)
	for _, kind := range []string{"missing-local", "missing-review", "wrong-review", "relative-state", "positional", "unknown-flag"} {
		t.Run(kind, func(t *testing.T) {
			a := append([]string(nil), args...)
			switch kind {
			case "missing-local":
				a = a[1:]
			case "missing-review":
				a = a[:len(a)-2]
			case "wrong-review":
				a[len(a)-1] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "relative-state":
				a[4] = "relative"
			case "positional":
				a = append(a, "unexpected")
			case "unknown-flag":
				a = append(a, "--publish")
			}
			var out bytes.Buffer
			if err := runRecoveryCheckpointCLI(a, &out); err == nil {
				t.Fatal("invalid arguments accepted")
			}
			if out.Len() != 0 {
				t.Fatal("failure emitted success JSON")
			}
		})
	}
	saved := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = saved }()
	args[4] = filepath.Join(state, "absent")
	var out bytes.Buffer
	if err := runRecoveryCheckpointCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(args[4]); !os.IsNotExist(err) {
		t.Fatal("dry-run created state", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("[DRY]")) {
		t.Fatal(out.String())
	}
}
