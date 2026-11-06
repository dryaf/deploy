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

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
)

func TestInstallationRetirementNativeReplacement(t *testing.T) {
	i, spec, m, target := installationRetirementFixture(t)
	v, err := i.retireInstallation(context.Background(), spec.Root, protocol.Hash(m), target)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(v)
	file := filepath.Join(filepath.Dir(target.State), "retirement.json")
	os.WriteFile(file, raw, 0600)
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = "../../.."
	if output, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(output))
	}
	var original protocol.Identity
	for n := 0; n < 2; n++ {
		cmd := exec.Command(binary, "notification-worker", "replacement-init", "--local", "--certificate", file, "--certificate-hash", protocol.Hash(v))
		output, err := process.LimitedOutput(cmd, 30*time.Second, 16384)
		if err != nil {
			t.Fatal(err, string(output))
		}
		var id protocol.Identity
		if err = json.Unmarshal(output, &id); err != nil {
			t.Fatal(err, string(output))
		}
		if !id.Valid() || id.Worker == m.Identity.Worker || (n > 0 && id != original) {
			t.Fatal("native identity changed", id)
		}
		original = id
	}
}
