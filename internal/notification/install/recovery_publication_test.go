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

func publicationCLIArguments(t *testing.T) (string, []string) {
	t.Helper()
	state, args := checkpointVerifyArguments(t)
	var out bytes.Buffer
	if err := runRecoveryCheckpointVerifyCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	var proof worker.RecoveryCheckpointProof
	if err := json.Unmarshal(out.Bytes(), &proof); err != nil {
		t.Fatal(err)
	}
	return state, append(args, "--proof-hash", proof.ProofHash)
}

func TestRecoveryPublicationCLI(t *testing.T) {
	state, args := publicationCLIArguments(t)
	before, err := os.ReadFile(filepath.Join(state, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	var one, two bytes.Buffer
	if err = runRecoveryPublicationCLI(args, &one); err != nil {
		t.Fatal(err)
	}
	if err = runRecoveryPublicationCLI(args, &two); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one.Bytes(), two.Bytes()) {
		t.Fatal("preparation replay changed")
	}
	var image worker.RecoveryPublicationImage
	if err = json.Unmarshal(one.Bytes(), &image); err != nil {
		t.Fatal(err)
	}
	if image.ImageHash == "" || image.AnchorHash == "" || image.PublicationAllowed || image.ExecutionEnabled || image.DeliveryEnabled {
		t.Fatal(image)
	}
	after, err := os.ReadFile(filepath.Join(state, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("preparation replaced canonical state")
	}
}

func TestRecoveryPublicationCLIFlagsAndDryRun(t *testing.T) {
	state, args := publicationCLIArguments(t)
	for _, kind := range []string{"missing-local", "missing-proof", "wrong-proof", "relative-path", "positional", "unknown-flag"} {
		t.Run(kind, func(t *testing.T) {
			a := append([]string(nil), args...)
			switch kind {
			case "missing-local":
				a = a[1:]
			case "missing-proof":
				a = a[:len(a)-2]
			case "wrong-proof":
				a[len(a)-1] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "relative-path":
				a[4] = "relative"
			case "positional":
				a = append(a, "unexpected")
			case "unknown-flag":
				a = append(a, "--activate")
			}
			var out bytes.Buffer
			if err := runRecoveryPublicationCLI(a, &out); err == nil {
				t.Fatal("invalid arguments accepted")
			}
			if out.Len() != 0 {
				t.Fatal("failure emitted image success")
			}
		})
	}
	saved := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = saved }()
	args[4] = filepath.Join(state, "absent")
	var out bytes.Buffer
	if err := runRecoveryPublicationCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("[DRY]")) {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(args[4]); !os.IsNotExist(err) {
		t.Fatal("dry-run created state")
	}
}
