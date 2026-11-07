package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
)

type importTestController func(ControlRequest) (ControlResult, error)

func (f importTestController) Apply(_ context.Context, _ config.ServerConfig, _ Artifact, r ControlRequest) (ControlResult, error) {
	return f(r)
}

func TestBackupImportPreservesLegacyMountAndRetentionPolicy(t *testing.T) {
	inv, err := config.ParseInventory("/project/server.yaml", []byte(backupOnlyInventory))
	if err != nil {
		t.Fatal(err)
	}
	facts := testFacts()
	b := *inv.Servers["manager"].Desired.Backup
	b.Storage.Mode, b.Storage.Path, b.Storage.RequireMount = "mount", "/mnt/backups/archive", "/mnt/backups"
	b.Retention = &config.RetentionConfig{KeepLast: 7, MaxAge: "720h"}
	p, _, err := CompileBackup(b, facts)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := backup.ResolveInstallationPaths(facts.Home, "", "", "", config.WorkerPaths{})
	if err != nil {
		t.Fatal(err)
	}
	legacy := p
	legacy.Storage.Mode = ""
	c := backup.WorkerInstallation{Policy: legacy, WorkerID: "do-not-export-writer", ArchiveID: "do-not-export-archive", Image: "registry.invalid/worker@sha256:" + strings.Repeat("a", 64)}
	review := backupImportPolicy(backup.SetupPolicyReview{Installation: c, Paths: layout}, config.WorkerArtifacts{})
	if review.Storage.Mode != "mount" || review.Paths.State != layout.State || review.Artifacts.Image != c.Image {
		t.Fatal("legacy semantics lost")
	}
	compiled, _, err := CompileBackup(review, facts)
	if err != nil || !reflect.DeepEqual(compiled, p) {
		t.Fatal("import changed effective backup policy", err)
	}
	raw, _ := json.Marshal(review)
	if bytes.Contains(raw, []byte(c.WorkerID)) || bytes.Contains(raw, []byte(c.ArchiveID)) {
		t.Fatal("generated archive authority exported")
	}
	inv.Servers["manager"] = config.ServerConfig{Desired: &config.DesiredWorkers{}}
	located := importLocator(inv, "backup-worker", "manager")
	if inv.Servers["manager"].Desired.Backup != nil || located.Servers["manager"].Desired.Backup == nil {
		t.Fatal("review locator changed original desired policy")
	}
}

func TestImportReviewCandidateNeverSavesOrReadsCredentials(t *testing.T) {
	for _, damage := range []string{"", "concurrent-edit", "wrong-kind"} {
		t.Run(damage, func(t *testing.T) {
			file := inventoryFixture(t)
			a := elfFixture(t, "arm64")
			e, err := OpenInventoryEdit(file)
			if err != nil {
				t.Fatal(err)
			}
			if err = e.Set("manager", []string{"notification_worker", "artifacts"}, config.WorkerArtifacts{Helper: a.File, HelperSHA256: a.SHA256}); err != nil {
				t.Fatal(err)
			}
			if err = e.Save(); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(file)
			if err = os.Chmod(file, 0400); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			inspector := &countingInspector{}
			calls := 0
			controller := importTestController(func(r ControlRequest) (ControlResult, error) {
				calls++
				if r.Action != "import" || len(r.Inputs) != 0 || r.InteractiveLink || r.LocalSignal != nil || r.LocalLED != nil {
					t.Fatal("import prepared mutation inputs")
				}
				n := *r.Desired.Notification
				n.Providers = config.ProviderDeclarations{"telegram": {TokenFile: "/target/private/token", ChatIDFile: "/target/private/chat"}}
				n.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "telegram"}}
				review := ImportReview{Policy: config.DesiredWorkers{Notification: &n}}
				if damage == "wrong-kind" {
					review.Policy.Backup = &config.BackupDeclaration{}
				}
				if damage == "concurrent-edit" {
					if err := os.Chmod(file, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(file, append(before, []byte("# concurrent edit\n")...), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return ControlResult{Version: 1, State: "candidate", Code: "review_required", Import: &review}, nil
			})
			cli := CLI{Out: &out, Inspector: inspector, Controller: controller}
			if err = cli.Run(context.Background(), "notification-worker", []string{"import", "manager", "--dry-run"}, Options{Inventory: file}); err != nil {
				t.Fatal(err)
			}
			if calls != 0 || inspector.Calls != 0 {
				t.Fatal("dry-run contacted target")
			}
			out.Reset()
			err = cli.Run(context.Background(), "notification-worker", []string{"import", "manager", "--json", "--non-interactive"}, Options{Inventory: file})
			var result struct{ State, Code, Candidate string }
			if e := json.Unmarshal(out.Bytes(), &result); e != nil {
				t.Fatal(e, out.String())
			}
			if damage != "" {
				if err == nil || result.Candidate != "" {
					t.Fatal("invalid review emitted candidate", err)
				}
				return
			}
			if err != nil || result.State != "candidate" || !strings.Contains(result.Candidate, "# keep this comment") || !strings.Contains(result.Candidate, "token_file: /target/private/token") {
				t.Fatal(err, result)
			}
			if _, err = config.ParseInventory(file, []byte(result.Candidate)); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(file)
			if !bytes.Equal(before, after) || calls != 1 {
				t.Fatal("review wrote inventory or repeated remote work")
			}
		})
	}
}

func TestNotificationImportDropsRuntimeIdentitiesButKeepsPolicy(t *testing.T) {
	r, paths := bootstrapControlFixture(t)
	input, err := CompileUnboundNotification(r.Desired, paths, r.Facts.Home, r.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	p := protocol.Producer{ID: "backup-worker", Epoch: "must-not-export"}
	input.Bootstrap = false
	input.Worker.Producers = []worker.Enrollment{{Producer: p, Required: true, Routes: []string{"alerts"}, MaxSilenceSeconds: 120, MaxEvidenceSeconds: 86400, MaxMessages: 100}}
	input.LEDSocket = "/run/fixture-led.sock"
	input.Worker.LEDBindings = []worker.LEDBinding{{ID: "backup", Producer: p, Subjects: []string{"*"}, LED: "red", AllowPower: true, Styles: map[string]worker.LEDStyle{"failure": {Pattern: "blink-fast", Priority: 70}}}}
	v := install.PolicyReview{Manifest: install.ManagedManifest{Root: paths.Installation, UnitDirectory: paths.UnitDirectory, Identity: protocol.Identity{Worker: "private-identity", Recovery: "private-recovery"}, Paths: install.ServicePaths{State: paths.State}}, Input: input}
	review, err := notificationImportPolicy(v, config.WorkerArtifacts{}, paths)
	if err != nil {
		t.Fatal(err)
	}
	n := review.Policy.Notification
	if n.Limits.MaxMessages != input.Worker.MaxMessages || n.StatusLEDs.Bindings["backup"].LED != "power" || !n.StatusLEDs.PowerLED || !*n.Producers["backup-worker"].Required {
		t.Fatal("lost policy", n)
	}
	raw, _ := json.Marshal(review)
	for _, value := range []string{"must-not-export", "private-identity", "private-recovery", input.Providers["telegram"].Destination, string(r.Inputs["token"])} {
		if value != "" && bytes.Contains(raw, []byte(value)) {
			t.Fatal("runtime identity or secret exported")
		}
	}
}
