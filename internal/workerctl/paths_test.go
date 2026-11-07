package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
)

func testFacts() TargetFacts {
	return TargetFacts{OS: "linux", Arch: "arm64", UID: 1000, User: "deploy", Home: "/home/remote", UserManager: true, Linger: true, Tools: map[string]bool{"systemctl": true, "busctl": true, "podman": true, "ssh": true, "ssh-keygen": true, "bwrap": true}}
}

func TestCustomBackupStatusKeepsHomeReservationLocator(t *testing.T) {
	f := testFacts()
	f.StateHome = "/srv/pi-state"
	p, err := ResolvePaths("backup-worker", config.WorkerPaths{State: "/srv/backup/database"}, f)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Paths map[string]string `json:"paths"`
	}
	if err := json.Unmarshal(statusRequest("backup-worker", p), &request); err != nil {
		t.Fatal(err)
	}
	if request.Paths["setup"] != "/home/remote/.local/state/deploy/backup-worker/setup.json" || request.Paths["state"] != "/srv/backup/database" {
		t.Fatal("status would lose retained setup authority", request.Paths)
	}
	if p.Installation != "/home/remote/.local/lib/deploy/backup-worker" {
		t.Fatal("default backup helper moved", p.Installation)
	}
}

func TestTargetPathsAreRemoteAndRejectOverlap(t *testing.T) {
	f := testFacts()
	t.Setenv("HOME", "/operator")
	p, err := ResolvePaths("backup-worker", config.WorkerPaths{}, f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Archive != "/home/remote/.local/share/deploy/backup-worker/archive" || strings.Contains(p.Config, "operator") {
		t.Fatal(p)
	}
	f.ConfigHome = "/remote/config"
	f.StateHome = "/remote/state"
	f.DataHome = "/remote/data"
	p, err = ResolvePaths("notification-worker", config.WorkerPaths{}, f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Config != "/remote/config/deploy/notification-worker" {
		t.Fatal(p)
	}
	for _, override := range []config.WorkerPaths{{State: p.Config}, {State: "~/../escape"}, {State: "/"}, {State: "~/" + strings.Repeat("x", 120)}, {Installation: p.UnitDirectory}, {Runtime: p.State + "/runtime.json"}} {
		if _, err = ResolvePaths("notification-worker", override, f); err == nil {
			t.Fatalf("accepted %+v", override)
		}
	}
	f.ConfigHome = "relative"
	if _, err = ResolvePaths("backup-worker", config.WorkerPaths{}, f); err == nil {
		t.Fatal("accepted relative XDG directory")
	}
}

type countingInspector struct{ Calls int }

func (i *countingInspector) Inspect(context.Context, config.ServerConfig) (TargetFacts, error) {
	i.Calls++
	return testFacts(), nil
}

const cliInventory = `# keep this comment
version: 3
servers:
  manager:
    host: raspberrypi.local
    user: deploy
    notification_worker:
      providers: [telegram] # keep provider comment
    backup_worker:
      sources: [source]
  source: {host: example.com, user: app}
`

func inventoryFixture(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(f, []byte(cliInventory), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestOfflinePlanAndUnattendedMissingInputsDoNotInspectOrWrite(t *testing.T) {
	f := inventoryFixture(t)
	before, _ := os.ReadFile(f)
	var out bytes.Buffer
	remote := &countingInspector{}
	c := CLI{Inspector: remote, Out: &out}
	for _, args := range [][]string{{"manager", "--dry-run", "--json"}, {"plan", "manager", "--dry-run"}, {"configure", "manager", "--dry-run"}, {"manager", "--non-interactive"}} {
		out.Reset()
		err := c.Run(context.Background(), "backup-worker", args, Options{Inventory: f})
		if args[len(args)-1] == "--non-interactive" && err == nil {
			t.Fatal("unattended missing inputs accepted")
		}
		if remote.Calls != 0 {
			t.Fatal("offline/missing-input command reached SSH")
		}
		if !strings.Contains(out.String(), "token_file") {
			t.Fatal(out.String())
		}
	}
	after, _ := os.ReadFile(f)
	if !bytes.Equal(before, after) {
		t.Fatal("offline plan edited inventory")
	}
}

func TestShortParserAndLegacyDispatch(t *testing.T) {
	for _, args := range [][]string{{"manager"}, {"apply", "status", "--json"}, {"--servers", "other.yml", "plan", "manager"}, {"configure", "manager", "--non-interactive"}} {
		if _, err := Parse(args, Options{}); err != nil {
			t.Fatal(args, err)
		}
		if !Matches(args) {
			t.Fatal("short form not recognized", args)
		}
	}
	for _, args := range [][]string{{"--local", "status"}, {"init", "--local"}, {"status", "--local"}, {"setup", "--spec", "input"}, {"notification-cutover", "--spec=x"}, {"__managed-probe"}, {"status", "--local=false"}, {"pin", "manager"}, {"pause", "manager"}, {"signal-link", "manager"}, {"reconcile", "manager"}} {
		if Matches(args) {
			t.Fatal("legacy operation captured", args)
		}
	}
	for _, args := range [][]string{{"status"}, {"manager", "--token=secret"}, {"apply", "manager", "extra"}, {"--servers"}} {
		if _, err := Parse(args, Options{}); err == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
}

func TestPolicyDiffDoesNotDiscloseValues(t *testing.T) {
	a := config.DesiredWorkers{Notification: &config.NotificationDeclaration{Providers: config.ProviderDeclarations{"telegram": {TokenFile: "/private/old"}}}}
	b := config.DesiredWorkers{Notification: &config.NotificationDeclaration{Providers: config.ProviderDeclarations{"telegram": {TokenFile: "/private/new"}}}}
	changes := PolicyChanges(a, b)
	if len(changes) != 1 || changes[0].Transition != "provider_review" || strings.Contains(changes[0].Field, "private") {
		t.Fatal(changes)
	}
	if len(PolicyChanges(a, a)) != 0 {
		t.Fatal("unchanged policy produces diff")
	}
}

func TestPolicyDiffRetainsIntegerPrecision(t *testing.T) {
	before := config.DesiredWorkers{Notification: &config.NotificationDeclaration{Limits: config.NotificationLimits{MaxDatabaseBytes: 1 << 53}}}
	after := config.DesiredWorkers{Notification: &config.NotificationDeclaration{Limits: config.NotificationLimits{MaxDatabaseBytes: 1<<53 + 1}}}
	changes := PolicyChanges(before, after)
	if len(changes) != 1 || changes[0].Field != "notification_worker.limits.max_database_bytes" {
		t.Fatal("integer change disappeared", changes)
	}
}

func TestPolicyDiffHasOnlyConcreteFields(t *testing.T) {
	changes := PolicyChanges(config.DesiredWorkers{}, config.DesiredWorkers{Notification: &config.NotificationDeclaration{}})
	for _, change := range changes {
		if change.Field == "" || change.Field == "notification_worker.paths" {
			t.Fatal("empty object produced a policy change", changes)
		}
	}
}
