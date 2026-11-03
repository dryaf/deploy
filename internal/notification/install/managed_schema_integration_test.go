//go:build notificationintegration

package install

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestManagedSchemaNativeExecutable(t *testing.T) {
	spec, i, f, id := managedFixture(t)
	// Set up the historical manifest/store with the lifecycle fixture manager;
	// the migrated service is an actual native deploy process with a Unix API.
	short, e := os.MkdirTemp("", "schema-native-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(short)
	state := filepath.Join(short, "state")
	if e = os.Rename(spec.State, state); e != nil {
		t.Fatal(e)
	}
	spec.State = state
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, e = i.prepare(ctx, "setup", spec); e != nil {
		t.Fatal(e)
	}
	old := downgradeManagedSchemaFixture(t, spec)
	// A schema-compatible binary predating migration support must fail before
	// stopping the old service, even though its ordinary lifecycle probe passes.
	repo, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	source := filepath.Join(repo, "internal/notification/install/managed_cli.go")
	body, e := os.ReadFile(source)
	if e != nil {
		t.Fatal(e)
	}
	overlaySource := filepath.Join(short, "legacy-probe.go")
	replaced := strings.Replace(string(body), "capability.SchemaMigration = 1", "capability.SchemaMigration = 0", 1)
	if replaced == string(body) {
		t.Fatal("probe fixture did not change capability")
	}
	if e = os.WriteFile(overlaySource, []byte(replaced), 0600); e != nil {
		t.Fatal(e)
	}
	overlay, e := json.Marshal(map[string]any{"Replace": map[string]string{source: overlaySource}})
	if e != nil {
		t.Fatal(e)
	}
	overlayPath := filepath.Join(short, "overlay.json")
	if e = os.WriteFile(overlayPath, overlay, 0600); e != nil {
		t.Fatal(e)
	}
	incompatible := exec.Command("go", "build", "-overlay", overlayPath, "-o", spec.BinaryFile, "./cmd/deploy")
	incompatible.Dir = repo
	if out, e := process.LimitedOutput(incompatible, time.Minute, 16384); e != nil {
		t.Fatal(e, string(out))
	}
	if e = os.Chmod(spec.BinaryFile, 0700); e != nil {
		t.Fatal(e)
	}
	legacy, e := os.ReadFile(spec.BinaryFile)
	if e != nil {
		t.Fatal(e)
	}
	spec.BinarySHA256 = managedDigest(legacy)
	i.probe = probeManagedBinary
	stops := f.stops
	if _, e = i.prepare(ctx, "migrate-schema", spec); e == nil {
		t.Fatal("binary without migration capability accepted")
	}
	if f.stops != stops {
		t.Fatal("incompatible migration stopped old service")
	}
	build := exec.Command("go", "build", "-o", spec.BinaryFile, "./cmd/deploy")
	build.Dir = "../../.."
	if out, e := process.LimitedOutput(build, time.Minute, 16384); e != nil {
		t.Fatal(e, string(out))
	}
	if e = os.Chmod(spec.BinaryFile, 0700); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(spec.BinaryFile)
	if e != nil {
		t.Fatal(e)
	}
	// Keep the ordinary capability response readable by retained older helpers
	// whose JSON decoder rejects unknown fields.
	ordinary, e := process.LimitedOutput(exec.Command(spec.BinaryFile, "notification-worker", "__managed-probe"), 10*time.Second, 4096)
	if e != nil {
		t.Fatal(e, string(ordinary))
	}
	var oldCapability struct {
		Version int `json:"version"`
		Schema  int `json:"schema"`
	}
	if e = safefs.DecodeJSON(ordinary, &oldCapability, 4096); e != nil {
		t.Fatal("ordinary probe broke retained helpers", e)
	}
	spec.BinarySHA256 = managedDigest(raw)
	native := &executableManagedServices{fakeManagedServices: f, root: spec.Root}
	i.services = native
	i.probe = probeManagedBinary
	t.Cleanup(func() {
		if native.cmd != nil {
			native.cmd.Process.Kill()
			select {
			case <-native.done:
			case <-time.After(5 * time.Second):
			}
			if native.log != nil {
				native.log.Close()
			}
		}
	})
	migrated, e := i.prepare(ctx, "migrate-schema", spec)
	if e != nil {
		out, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
		t.Fatal(e, string(out))
	}
	if migrated.Identity != id || migrated.Schema != 10 || migrated.HelperRevision == "" || migrated.ConfigHash != old.ConfigHash {
		t.Fatal(migrated)
	}
	if _, e = i.resume(ctx, spec.Root); e != nil {
		t.Fatal(e)
	}
	// The retained helper must understand the migration journal and new pins.
	out, e := process.LimitedOutput(exec.Command(filepath.Join(spec.Root, managedHelperName(migrated)), "notification-worker", "installation-status", "--local", "--root", spec.Root), 15*time.Second, 32768)
	if e != nil {
		t.Fatal("retained helper cannot inspect migrated installation", e, string(out))
	}
	if _, e = i.prepare(ctx, "update", spec); e != nil {
		t.Fatal("subsequent native update", e)
	}
	if _, e = i.uninstall(ctx, spec.Root); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(spec.Root, managedHelperName(old))); e != nil {
		t.Fatal("old helper removed", e)
	}
}
