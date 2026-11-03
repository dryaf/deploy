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

func publicationVerifyArguments(t *testing.T) (string, []string, []byte) {
	t.Helper()
	state, args := publicationCLIArguments(t)
	var out bytes.Buffer
	if err := runRecoveryPublicationCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	var v worker.RecoveryPublicationImage
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return state, append(args, "--image-hash", v.ImageHash), out.Bytes()
}

func TestRecoveryPublicationVerifyCLI(t *testing.T) {
	state, args, expected := publicationVerifyArguments(t)
	var out bytes.Buffer
	if err := runRecoveryPublicationVerifyCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(expected, out.Bytes()) {
		t.Fatal("verification changed the prepared image result")
	}
	for _, kind := range []string{"missing-local", "missing-image", "wrong-image", "relative", "positional", "unknown-flag"} {
		t.Run(kind, func(t *testing.T) {
			a := append([]string(nil), args...)
			switch kind {
			case "missing-local":
				a = a[1:]
			case "missing-image":
				a = a[:len(a)-2]
			case "wrong-image":
				a[len(a)-1] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			case "relative":
				a[4] = "relative"
			case "positional":
				a = append(a, "unexpected")
			case "unknown-flag":
				a = append(a, "--activate")
			}
			out.Reset()
			if err := runRecoveryPublicationVerifyCLI(a, &out); err == nil || out.Len() != 0 {
				t.Fatal("invalid verification emitted success", err, out.String())
			}
		})
	}
	saved := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = saved }()
	args[4] = filepath.Join(state, "absent")
	out.Reset()
	if err := runRecoveryPublicationVerifyCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("[DRY]")) {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(args[4]); !os.IsNotExist(err) {
		t.Fatal("dry run created state")
	}
}
