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

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil/ledfixture"
)

func TestManagedLEDMappingNativeExecutable(t *testing.T) {
	socket := ledfixture.Start(t)
	spec, i, f, id := managedFixture(t)
	qualificationFixtureInputs(t, spec.RuntimeFile)
	short, err := os.MkdirTemp("", "managed-led-native-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	state := filepath.Join(short, "state")
	if err = os.Rename(spec.State, state); err != nil {
		t.Fatal(err)
	}
	spec.State = state
	before := rawManagedRuntime(t, spec.RuntimeFile)
	before.LEDSocket = socket
	endpoint, _, err := led.InspectEndpoint(context.Background(), socket)
	if err != nil {
		t.Fatal(err)
	}
	before.LEDEndpoint = &endpoint
	i.ledEndpointProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	writeRuntime(t, spec.RuntimeFile, before)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	old, err := i.prepare(ctx, "setup", spec)
	if err != nil {
		t.Fatal(err)
	}
	after := rawManagedRuntime(t, spec.RuntimeFile)
	after.Worker.LEDBindings = []worker.LEDBinding{{ID: "backup", Producer: after.Worker.Producers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
	writeRuntime(t, spec.RuntimeFile, after)
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(repo, "internal/notification/install/managed_cli.go")
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	replaced := strings.Replace(string(body), "capability.LEDMappingMigration = 2", "capability.LEDMappingMigration = 1", 1)
	if replaced == string(body) {
		t.Fatal("capability overlay did not change source")
	}
	overlaySource := filepath.Join(short, "no-led-capability.go")
	if err = os.WriteFile(overlaySource, []byte(replaced), 0600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: overlaySource}})
	if err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(short, "overlay.json")
	if err = os.WriteFile(overlayPath, overlay, 0600); err != nil {
		t.Fatal(err)
	}
	build := func(extra ...string) {
		t.Helper()
		args := append([]string{"build"}, extra...)
		args = append(args, "-o", spec.BinaryFile, "./cmd/deploy")
		cmd := exec.Command("go", args...)
		cmd.Dir = repo
		if out, err := process.LimitedOutput(cmd, time.Minute, 16384); err != nil {
			t.Fatal(err, string(out))
		}
		if err = os.Chmod(spec.BinaryFile, 0700); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(spec.BinaryFile)
		if err != nil {
			t.Fatal(err)
		}
		spec.BinarySHA256 = managedDigest(raw)
	}
	build("-overlay", overlayPath)
	i.probe = probeManagedBinary
	i.ledEndpointProbe = nil
	stops := f.stops
	if _, err = i.prepare(ctx, "migrate-led", spec); err == nil {
		t.Fatal("missing LED migration capability accepted")
	}
	if f.stops != stops {
		t.Fatal("unsupported executable stopped old service")
	}
	build()
	native := &executableManagedServices{fakeManagedServices: f, root: spec.Root}
	i.services = native
	t.Cleanup(func() {
		if native.cmd != nil {
			native.cmd.Process.Kill()
			<-native.done
			if native.log != nil {
				native.log.Close()
			}
		}
	})
	m, err := i.prepare(ctx, "migrate-led", spec)
	if err != nil {
		t.Fatal(err)
	}
	if m.Identity != id || m.ConfigHash == old.ConfigHash || m.HelperRevision != m.Revision {
		t.Fatal(m)
	}
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
	output, err := process.LimitedOutput(exec.Command(filepath.Join(spec.Root, managedHelperName(m)), "notification-worker", "installation-status", "--local", "--root", spec.Root), 15*time.Second, 32768)
	if err != nil {
		t.Fatal("retained helper", err, string(output))
	}
	m, err = i.prepare(ctx, "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	api := led.Client{Socket: socket, Budget: time.Second}
	foreign := led.State{ID: "foreign.client", LED: "green", Pattern: "blink-fast", Priority: 95, Persistent: true}
	if err = api.Set(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	disabled, err := i.prepareLEDOutput(ctx, spec.Root, protocol.Hash(m), true)
	if err != nil {
		t.Fatal("native disable", err)
	}
	entries, err := api.List(ctx)
	if err != nil || len(entries) != 1 || entries[0].State != foreign {
		t.Fatal("disable changed foreign state or retained worker output", entries, err)
	}
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal("native disabled resume", err)
	}
	enabled, err := i.prepareLEDOutput(ctx, spec.Root, protocol.Hash(disabled), false)
	if err != nil || enabled.LEDOutput == nil || enabled.LEDOutput.Disabled {
		t.Fatal("native re-enable", err)
	}
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
	entries, err = api.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.ID == foreign.ID {
			found = entry.State == foreign
			continue
		}
		if entry.Pattern == "heartbeat" {
			t.Fatal("re-enable invented healthy evidence", entry)
		}
	}
	if !found {
		t.Fatal("re-enable changed foreign output")
	}
	if _, err = i.uninstall(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
}
