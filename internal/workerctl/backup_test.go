package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
)

const backupOnlyInventory = `version: 3
servers:
  manager:
    host: raspberrypi.local
    user: deploy
    backup_worker:
      sources: [source]
  source: {host: example.com, user: app}
`

func TestCompileStandaloneBackupDefaultsAndPaths(t *testing.T) {
	inv, err := config.ParseInventory("server.yaml", []byte(backupOnlyInventory))
	if err != nil {
		t.Fatal(err)
	}
	b := *inv.Servers["manager"].Desired.Backup
	p, paths, err := CompileBackup(b, testFacts())
	if err != nil {
		t.Fatal(err)
	}
	if p.Notifications.Mode != "disabled" || p.Storage.Mode != "home" || p.Storage.Path != paths.Archive || p.Storage.RequireMount != "" || p.Timezone != "UTC" || p.Retention != nil {
		t.Fatal("unexpected backup defaults", p)
	}
	b.Storage.Path = "~/backups"
	p, _, err = CompileBackup(b, testFacts())
	if err != nil || p.Storage.Path != "/home/remote/backups" {
		t.Fatal(p, err)
	}
	for _, unsafe := range []string{"~/", "~/.config/deploy/backup-worker/archive", "~/.local/state/deploy/backup-worker/state/archive", "~/.local/lib/deploy/backup-worker"} {
		b.Storage.Path = unsafe
		if _, _, err = CompileBackup(b, testFacts()); err == nil {
			t.Fatal("unsafe archive accepted", unsafe)
		}
	}
	b.Storage.Path = ""
	f := testFacts()
	f.ConfigHome = "/custom/config"
	if _, paths, err = CompileBackup(b, f); err != nil || paths.Config != "/custom/config/deploy/backup-worker" {
		t.Fatal("XDG override lost", paths, err)
	}
	b.Paths.State = "~/custom/state"
	if _, paths, err = CompileBackup(b, testFacts()); err != nil || paths.State != "/home/remote/custom/state" {
		t.Fatal("custom state lost", paths, err)
	}
}

func TestCompileBackupProviderReferencesAreTargetLocal(t *testing.T) {
	data := strings.Replace(backupOnlyInventory, "sources: [source]", `sources: [source]
      notifications:
        primary: telegram
        telegram:
          token_file: ~/.config/private/token
          chat_id_file: ~/.config/private/chat`, 1)
	inv, err := config.ParseInventory("server.yaml", []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	b := inv.Servers["manager"].Desired.Backup
	p, _, err := CompileBackup(*b, testFacts())
	if err != nil || p.Notifications.Mode != "" || p.Notifications.Telegram.TokenFile != "/home/remote/.config/private/token" {
		t.Fatal(p, err)
	}
	if b.Notifications.Telegram.TokenFile != "~/.config/private/token" {
		t.Fatal("compiler changed declaration")
	}
}

type backupTestInspector struct{}

func (backupTestInspector) Inspect(_ context.Context, s config.ServerConfig) (TargetFacts, error) {
	f := testFacts()
	if s.Host == "example.com" {
		f.Arch, f.User, f.Home = "amd64", "app", "/home/app"
	}
	return f, nil
}

type backupTestInstaller struct {
	Plans           []backup.SetupPlan
	Worker, Archive string
}

func (i *backupTestInstaller) Setup(_ context.Context, p backup.SetupPlan) (backup.WorkerSetupResponse, error) {
	i.Plans = append(i.Plans, p)
	return backup.WorkerSetupResponse{Schema: 1, Stage: "complete", WorkerID: i.Worker, ArchiveID: i.Archive}, nil
}

func TestShortBackupPreflightsSourcesAndPinsArtifactsBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	catalog := Catalog{Version: 1, Release: "fixture", Linux: map[string]ArchitectureArtifacts{}}
	for _, arch := range []string{"arm64", "amd64"} {
		a := elfFixture(t, arch)
		body, err := os.ReadFile(a.File)
		if err != nil {
			t.Fatal(err)
		}
		name := "deploy-linux-" + arch
		if err = os.WriteFile(filepath.Join(dir, name), body, 0700); err != nil {
			t.Fatal(err)
		}
		a.File = name
		catalog.Linux[arch] = ArchitectureArtifacts{Helper: a, BackupImage: "example.test/backup@sha256:" + strings.Repeat("a", 64)}
	}
	raw, _ := json.Marshal(catalog)
	file := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	inventory := filepath.Join(dir, "server.yaml")
	if err := os.WriteFile(inventory, []byte(backupOnlyInventory), 0600); err != nil {
		t.Fatal(err)
	}
	i := &backupTestInstaller{}
	i.Worker, _ = safefs.NewID("w")
	i.Archive, _ = safefs.NewID("a")
	var out bytes.Buffer
	trustMissing := true
	c := CLI{Inspector: backupTestInspector{}, BackupInstaller: i, Out: &out, SourceTrust: func(context.Context, config.ServerConfig) (string, error) {
		if trustMissing {
			return "", fmt.Errorf("verified source host key missing")
		}
		return "fixture verified key\n", nil
	}}
	options := Options{Inventory: inventory, Catalog: file, NonInteractive: true, JSON: true}
	if err := c.Run(context.Background(), "backup-worker", []string{"manager"}, options); err == nil || len(i.Plans) != 0 {
		t.Fatal("mutated before source trust was established", err)
	}
	trustMissing = false
	for _, action := range []string{"apply", "resume"} {
		out.Reset()
		if err := c.Run(context.Background(), "backup-worker", []string{action, "manager"}, options); err != nil {
			t.Fatal(err)
		}
		var result BackupResult
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.State != "qualification_required" || result.WorkerID != i.Worker {
			t.Fatal(out.String(), err)
		}
	}
	if len(i.Plans) != 2 {
		t.Fatal("setup not reached")
	}
	first, second := i.Plans[0], i.Plans[1]
	if first.Target.Helper.Architecture != "arm64" || first.Sources["source"].Helper.Architecture != "amd64" || first.Request.Declaration.Sources["source"] != first.Sources["source"].Helper.SHA256 {
		t.Fatal("wrong per-host artifact pins")
	}
	a, _ := json.Marshal(first.Request)
	b, _ := json.Marshal(second.Request)
	if !bytes.Equal(a, b) {
		t.Fatal("resume regenerated request authority")
	}
	if first.Request.Policy.Notifications.Mode != "disabled" || first.Request.Sources[0].KnownHosts == "" {
		t.Fatal("desired backup policy was not compiled")
	}
	if first.Request.Declaration.Version != 1 || first.Request.Declaration.Paths != nil {
		t.Fatal("default installation contract changed")
	}
	custom := strings.Replace(backupOnlyInventory, "sources: [source]", `sources: [source]
      paths:
        config: ~/backup-config
        runtime: ~/backup-inputs/runtime.json
        state: /srv/backup/state
        data: /srv/backup/data
        installation: ~/backup-helper
        denied_paths: [~/private]`, 1)
	if err := os.WriteFile(inventory, []byte(custom), 0600); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"apply", "resume"} {
		out.Reset()
		if err := c.Run(context.Background(), "backup-worker", []string{action, "manager"}, options); err != nil {
			t.Fatal(err)
		}
	}
	if len(i.Plans) != 4 {
		t.Fatal("custom setup not reached")
	}
	declaration := i.Plans[2].Request.Declaration
	if declaration.Version != 2 || declaration.Paths == nil || declaration.Paths.Runtime != "/home/remote/backup-inputs/runtime.json" || declaration.Paths.Helper() != "/home/remote/backup-helper/deploy" || declaration.Paths.State != "/srv/backup/state" {
		t.Fatal("custom layout not pinned", declaration)
	}
	if err := declaration.Paths.MatchesDesired(declaration.Desired.Backup.Paths); err != nil {
		t.Fatal(err)
	}
	a, _ = json.Marshal(i.Plans[2].Request)
	b, _ = json.Marshal(i.Plans[3].Request)
	if !bytes.Equal(a, b) {
		t.Fatal("custom resume changed path authority")
	}
	if err := os.WriteFile(inventory, []byte(backupOnlyInventory), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	preview := options
	preview.DryRun = true
	if err := c.Run(context.Background(), "backup-worker", []string{"adopt", "manager"}, preview); err != nil || len(i.Plans) != 4 {
		t.Fatal("adoption preview mutated", err)
	}
	out.Reset()
	if err := c.Run(context.Background(), "backup-worker", []string{"adopt", "manager"}, options); err != nil {
		t.Fatal(err)
	}
	var adopted BackupResult
	if err := json.Unmarshal(out.Bytes(), &adopted); err != nil || adopted.State != "adopted" || adopted.WorkerID != i.Worker {
		t.Fatal("adoption result", out.String(), err)
	}
	if len(i.Plans) != 5 || i.Plans[4].Request.Operation != "adopt" || i.Plans[4].Request.Declaration.DesiredHash != first.Request.Declaration.DesiredHash || i.Plans[4].Request.Declaration.Sources["source"] != first.Request.Declaration.Sources["source"] {
		t.Fatal("adoption bypassed source/artifact pins")
	}
}
