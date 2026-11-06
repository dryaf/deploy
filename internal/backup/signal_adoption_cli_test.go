package backup

import (
	"bytes"
	"context"
	"encoding/json"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/ui"
)

func TestLegacyBackupSignalAdoptionDryRun(t *testing.T) {
	spec, svc, _ := legacySignalAdoptionFixture(t)
	directory := filepath.Join(spec.Home, ".config/deploy/backup-worker")
	testutil.Must(t, os.MkdirAll(directory, 0700))
	raw, err := json.Marshal(svc.installation)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(directory, "runtime.json"), raw, 0600))
	raw, err = json.Marshal(map[string]string{"worker": svc.image, "relay": svc.image})
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(directory, "images.json"), raw, 0600))
	path := filepath.Join(t.TempDir(), "adoption.json")
	raw, err = json.Marshal(spec)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(path, raw, 0600))
	before := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = before }()
	for _, args := range [][]string{{"signal-adopt", "--local", "--spec", path}, {"signal-adoption-review", "--local", "--home", spec.Home}} {
		var out bytes.Buffer
		testutil.Must(t, runBackupSignalAdoptionCLI(args, &out))
		if !strings.Contains(out.String(), "[DRY]") {
			t.Fatal(out.String())
		}
	}
	if _, err = os.Stat(filepath.Join(spec.Inbox, "legacy-bootstrap-intent.json")); !os.IsNotExist(err) {
		t.Fatal("dry-run opened inbox", err)
	}
	for _, args := range [][]string{{"signal-adopt", "--spec", path}, {"signal-adopt", "--local", "--spec", path, "unexpected"}, {"signal-adoption-review", "--local", "--home", spec.Home, "--spec", path}} {
		if runBackupSignalAdoptionCLI(args, &bytes.Buffer{}) == nil {
			t.Fatal("invalid scope accepted", args)
		}
	}
	testutil.Must(t, os.Chmod(path, 0644))
	if runBackupSignalAdoptionCLI([]string{"signal-adopt", "--local", "--spec", path}, &bytes.Buffer{}) == nil {
		t.Fatal("nonprivate adoption spec accepted")
	}
}
func TestLegacySignalAdoptionImageProbeIsIsolated(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "arguments")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SIGNAL_ADOPTION_PROBE_LOG", log)
	svc := &signalService{image: "example.test/relay@sha256:" + strings.Repeat("a", 64)}
	for _, response := range []string{"backup-signal-adoption-v1", "old-runtime"} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SIGNAL_ADOPTION_PROBE_LOG\"\nprintf '%s\\n' '" + response + "'\n"
		testutil.Must(t, os.WriteFile(filepath.Join(bin, "podman"), []byte(script), 0700))
		err := svc.VerifyNextRecoverySupport(context.Background())
		if (err == nil) != (response == "backup-signal-adoption-v1") {
			t.Fatal(response, err)
		}
		raw, err := os.ReadFile(log)
		testutil.Must(t, err)
		for _, required := range []string{"--network=none\n", "--pull=never\n", "--read-only\n", "__signal-adoption-probe\n"} {
			if !strings.Contains(string(raw), required) {
				t.Fatal("probe isolation missing", string(raw))
			}
		}
		if strings.Contains(string(raw), "--volume") || strings.Contains(string(raw), "--mount") {
			t.Fatal("probe accessed account state")
		}
	}
}

func TestLegacyBackupSignalCLIWorkflow(t *testing.T) {
	spec, svc, _ := legacySignalAdoptionFixture(t)
	_, state, _, _, _ := archiveFixture(t)
	var control ArchiveControl
	testutil.Must(t, state.get("control", &control))
	svc.installation.WorkerID = control.WriterID
	svc.installation.ArchiveID = control.ArchiveID
	svc.installation.ConfigHash = control.ConfigHash
	paths := backupCutoverPaths(spec.Home, svc.installation)
	testutil.Must(t, state.Close())
	testutil.Must(t, os.MkdirAll(filepath.Dir(paths.State), 0700))
	testutil.Must(t, os.Rename(state.directory, paths.State))
	svc.configuration = paths.Config
	images := map[string]string{"worker": svc.image, "relay": svc.image}
	raw, err := json.Marshal(images)
	testutil.Must(t, err)
	svc.imagesHash = notify.Hash(raw)
	testutil.Must(t, os.WriteFile(filepath.Join(spec.Home, ".config/deploy/backup-worker/images.json"), raw, 0600))
	raw, err = json.Marshal(svc.installation)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(paths.Config, raw, 0600))
	units, err := renderWorkerUnits(svc.installation, paths, svc.image, svc.image)
	testutil.Must(t, err)
	svc.unitText = units["deploy-backup-signal.container"]
	testutil.Must(t, os.WriteFile(filepath.Join(spec.Home, ".config/containers/systemd/deploy-backup-signal.container"), []byte(svc.unitText), 0600))
	root, err := safefs.Open(spec.Home)
	testutil.Must(t, err)
	testutil.Must(t, root.Mkdir(".local/state/deploy/locks"))
	lease, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, true)
	testutil.Must(t, err)
	lease.Close()
	root.Close()
	view, err := svc.Inspect(context.Background())
	testutil.Must(t, err)
	svc.pinned = view.Binding
	spec.ServiceBinding = view.Binding
	spec.ConfigHash = notify.Hash(svc.installation)
	prepare := func(s backupSignalAdoptionSpec) (*signalService, error) {
		actual, err := prepareBackupSignalAdoption(s)
		if err != nil {
			return nil, err
		}
		actual.run = svc.run
		actual.adoptionProbe = svc.adoptionProbe
		return actual, nil
	}
	var review bytes.Buffer
	testutil.Must(t, runBackupSignalAdoptionCLIWith([]string{"signal-adoption-review", "--local", "--home", spec.Home}, &review, prepare))
	if !strings.Contains(review.String(), spec.ServiceBinding) {
		t.Fatal("review did not pin actual installed service")
	}
	path := filepath.Join(t.TempDir(), "spec.json")
	raw, err = json.Marshal(spec)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(path, raw, 0600))
	var out bytes.Buffer
	testutil.Must(t, runBackupSignalAdoptionCLIWith([]string{"signal-adopt", "--local", "--spec", path}, &out, prepare))
	var adoption backupSignalAdoption
	testutil.Must(t, json.Unmarshal(out.Bytes(), &adoption))
	testutil.Must(t, adoption.validate())
	handoff, _ := legacyAdoptionGateway(t, adoption)
	raw, err = json.Marshal(handoff)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(path, raw, 0600))
	out.Reset()
	testutil.Must(t, runBackupSignalAdoptionCLIWith([]string{"signal-adoption-handoff", "--local", "--spec", path}, &out, prepare))
	if !strings.Contains(out.String(), "legacy_bootstrap") {
		t.Fatal("CLI dropped bootstrap provenance")
	}
}
