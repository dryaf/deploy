package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/ui"
)

func TestInstallationRetirementCLIPrivatePinsAndDryRun(t *testing.T) {
	i, spec, m, target := installationRetirementFixture(t)
	raw, _ := json.Marshal(target.Runtime)
	config := filepath.Join(filepath.Dir(target.State), "runtime.json")
	os.WriteFile(config, raw, 0600)
	var out bytes.Buffer
	if err := runReplacementTargetCLI([]string{"replacement-target", "--local", "--config", config, "--root", target.Root, "--state-dir", target.State, "--uid", strconv.Itoa(target.UID), "--reason", target.Reason}, &out); err != nil {
		t.Fatal(err)
	}
	var reviewed ReplacementTarget
	if err := json.Unmarshal(out.Bytes(), &reviewed); err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(reviewed) != protocol.Hash(target) {
		t.Fatal("review changed raw route labels")
	}
	targetFile := filepath.Join(filepath.Dir(target.State), "target.json")
	os.WriteFile(targetFile, out.Bytes(), 0600)
	ui.DryRun = true
	defer func() { ui.DryRun = false }()
	if err := runInstallationRetirementCLI([]string{"installation-retire", "--local", "--root", spec.Root, "--manifest-hash", protocol.Hash(m), "--target", targetFile}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(spec.Root, "retirement-target.json")); !os.IsNotExist(err) {
		t.Fatal("dry run published fence", err)
	}
	ui.DryRun = false
	v, err := i.retireInstallation(context.Background(), spec.Root, protocol.Hash(m), target)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(v)
	certificate := filepath.Join(filepath.Dir(target.State), "retirement.json")
	os.WriteFile(certificate, raw, 0600)
	args := []string{"replacement-init", "--local", "--certificate", certificate, "--certificate-hash", protocol.Hash(v)}
	ui.DryRun = true
	if err = runInstallationRetirementCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(target.State, "state.db")); !os.IsNotExist(err) {
		t.Fatal("dry run initialized state", err)
	}
	os.Chmod(certificate, 0644)
	if err = runInstallationRetirementCLI(args, &out); err == nil {
		t.Fatal("public certificate accepted")
	}
	os.Chmod(certificate, 0600)
	ui.DryRun = false
	out.Reset()
	if err = runInstallationRetirementCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	var id protocol.Identity
	if err = json.Unmarshal(out.Bytes(), &id); err != nil || !id.Valid() {
		t.Fatal("invalid result", err)
	}
}
