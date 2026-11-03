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

func checkpointVerifyArguments(t *testing.T) (string, []string) {
	t.Helper()
	state, args := recoveryCheckpointArguments(t)
	var out bytes.Buffer
	if err := runRecoveryCheckpointCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	var cp worker.RecoveryCheckpoint
	if err := json.Unmarshal(out.Bytes(), &cp); err != nil {
		t.Fatal(err)
	}
	return state, append(args, "--checkpoint-hash", cp.CheckpointHash)
}

func TestRecoveryCheckpointVerifyCLI(t *testing.T) {
	_, args := checkpointVerifyArguments(t)
	var first, again bytes.Buffer
	if err := runRecoveryCheckpointVerifyCLI(args, &first); err != nil {
		t.Fatal(err)
	}
	if err := runRecoveryCheckpointVerifyCLI(args, &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), again.Bytes()) {
		t.Fatal("verification replay changed")
	}
	var proof worker.RecoveryCheckpointProof
	if err := json.Unmarshal(first.Bytes(), &proof); err != nil {
		t.Fatal(err)
	}
	if proof.ProofHash == "" || proof.EvidenceArchiveHash == "" || proof.PublicationAllowed || proof.DeliveryEnabled || proof.ExecutionEnabled {
		t.Fatal(proof)
	}
}

func TestRecoveryCheckpointVerifyCLIFlagsAndDryRun(t *testing.T) {
	state, args := checkpointVerifyArguments(t)
	for _, kind := range []string{"missing-local", "missing-checkpoint", "wrong-checkpoint", "relative-path", "positional", "unknown-flag"} {
		t.Run(kind, func(t *testing.T) {
			a := append([]string(nil), args...)
			switch kind {
			case "missing-local":
				a = a[1:]
			case "missing-checkpoint":
				a = a[:len(a)-2]
			case "wrong-checkpoint":
				a[len(a)-1] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "relative-path":
				a[4] = "relative"
			case "positional":
				a = append(a, "unexpected")
			case "unknown-flag":
				a = append(a, "--publish")
			}
			var out bytes.Buffer
			if err := runRecoveryCheckpointVerifyCLI(a, &out); err == nil {
				t.Fatal("invalid verification accepted")
			}
			if out.Len() != 0 {
				t.Fatal("failed verification emitted success proof")
			}
		})
	}
	saved := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = saved }()
	args[4] = filepath.Join(state, "absent")
	var out bytes.Buffer
	if err := runRecoveryCheckpointVerifyCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("[DRY]")) {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(args[4]); !os.IsNotExist(err) {
		t.Fatal("dry-run created state", err)
	}
}
