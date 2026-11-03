package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
	"github.com/dryaf/deploy/internal/ui"
)

func signalServiceProperties(source, fragment string) string {
	return "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=42\nControlPID=0\nJob=\nFragmentPath=" + fragment + "\nSourcePath=" + source + "\nDropInPaths=\nUnitFileState=generated\nNames=" + backupSignalUnit + "\nNeedDaemonReload=no\n"
}
func TestSignalServiceRejectsAmbiguousManagerState(t *testing.T) {
	raw := signalServiceProperties("/source", "/fragment")
	if _, err := parseSignalService([]byte(raw)); err != nil {
		t.Fatal(err)
	}
	for _, change := range [][2]string{
		{"LoadState=loaded", "LoadState=not-found"}, {"DropInPaths=", "DropInPaths=/override"},
		{"Names=" + backupSignalUnit, "Names=" + backupSignalUnit + " alias.service"},
		{"UnitFileState=generated", "UnitFileState=enabled"}, {"NeedDaemonReload=no", "NeedDaemonReload=yes"},
		{"Job=", "Job=123"}, {"ControlPID=0", "ControlPID=23"}, {"MainPID=42", "MainPID=0"},
		{"ActiveState=active", "ActiveState=activating"}, {"SubState=running", "SubState=dead"},
	} {
		if _, err := parseSignalService([]byte(strings.Replace(raw, change[0], change[1], 1))); err == nil {
			t.Fatal("accepted", change)
		}
	}
	for _, bad := range []string{raw + "MainPID=42\n", raw + "Foreign=x\n", strings.Replace(raw, "Job=\n", "", 1)} {
		if _, err := parseSignalService([]byte(bad)); err == nil {
			t.Fatal("accepted malformed state")
		}
	}
	inactive := strings.NewReplacer("ActiveState=active", "ActiveState=inactive", "SubState=running", "SubState=dead", "MainPID=42", "MainPID=0").Replace(raw)
	if _, err := parseSignalService([]byte(inactive)); err != nil {
		t.Fatal(err)
	}
	if _, err := parseSignalService([]byte(strings.Replace(inactive, "MainPID=0", "MainPID=42", 1))); err == nil {
		t.Fatal("inactive live process accepted")
	}
}

func signalServiceFixture(t *testing.T) (*signalService, map[string]string, *[]string) {
	t.Helper()
	home := t.TempDir()
	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	policy := configfixture.WorkerPolicy(t)
	policy.Notifications.Signal.AccountFile = filepath.Join(home, "account")
	policy.Notifications.Signal.RecipientFile = filepath.Join(home, "recipient")
	c := WorkerInstallation{Schema: 1, WorkerID: testutil.ID(t, "w"), ArchiveID: testutil.ID(t, "a"), ConfigHash: strings.Repeat("a", 64), Policy: policy}
	for _, alias := range c.Policy.Sources {
		c.Sources = append(c.Sources, source.WorkerSource{Alias: alias, Host: "localhost", User: "fixture", Port: 22, KeyFile: "/tmp/key", KnownHosts: "/tmp/known", SourceID: testutil.ID(t, "s")})
	}
	paths := map[string]string{"config": filepath.Join(home, "runtime.json"), "images": filepath.Join(home, ".config/deploy/backup-worker/images.json"), "unit": filepath.Join(home, ".config/containers/systemd/deploy-backup-signal.container"), "fragment": filepath.Join(runtime, "systemd/generator", backupSignalUnit), "account": policy.Notifications.Signal.AccountFile, "recipient": policy.Notifications.Signal.RecipientFile}
	image := "example.test/relay@sha256:" + strings.Repeat("b", 64)
	raw, _ := json.Marshal(c)
	bodies := map[string]string{"config": string(raw), "images": "{\"relay\":\"" + image + "\"}", "unit": "[Container]\nImage=" + image + "\n", "fragment": "ExecStart=podman run " + image + " backup-worker __signal_relay\n", "account": "synthetic-account", "recipient": "synthetic-recipient"}
	for k, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(bodies[k]), 0600); err != nil {
			t.Fatal(err)
		}
	}
	calls := []string{}
	s := &signalService{home: home, configuration: paths["config"], image: image, unitText: bodies["unit"], imagesHash: protocol.Hash([]byte(bodies["images"])), installation: c}
	s.run = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "show" {
			return []byte(signalServiceProperties(paths["unit"], paths["fragment"])), nil
		}
		return nil, nil
	}
	view, err := s.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.pinned = view.Binding
	return s, paths, &calls
}
func TestSignalServicePinsConfigurationBeforeMutation(t *testing.T) {
	for _, damage := range []string{"config", "images", "unit", "fragment", "account", "recipient", "source", "outside-fragment", "drop-in"} {
		t.Run(damage, func(t *testing.T) {
			s, paths, calls := signalServiceFixture(t)
			if p, ok := paths[damage]; ok {
				f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = fmt.Fprint(f, "changed")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			} else {
				original := s.run
				s.run = func(ctx context.Context, args ...string) ([]byte, error) {
					raw, err := original(ctx, args...)
					if args[0] == "show" {
						switch damage {
						case "source":
							raw = []byte(strings.Replace(string(raw), "SourcePath="+paths["unit"], "SourcePath=/other", 1))
						case "outside-fragment":
							raw = []byte(strings.Replace(string(raw), "FragmentPath="+paths["fragment"], "FragmentPath=/other", 1))
						case "drop-in":
							raw = []byte(strings.Replace(string(raw), "DropInPaths=", "DropInPaths=/other", 1))
						}
					}
					return raw, err
				}
			}
			for _, action := range []func(context.Context) error{s.Stop, s.Start} {
				if err := action(context.Background()); err == nil {
					t.Fatal("changed binding accepted")
				}
			}
			for _, call := range *calls {
				if !strings.HasPrefix(call, "show ") {
					t.Fatal("manager mutated", call)
				}
			}
		})
	}
}
func TestSignalServiceChangesOnlyRelayActivity(t *testing.T) {
	s, _, calls := signalServiceFixture(t)
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var changes []string
	for _, call := range *calls {
		if !strings.HasPrefix(call, "show ") {
			changes = append(changes, call)
		}
	}
	if strings.Join(changes, ";") != "stop "+backupSignalUnit+";start "+backupSignalUnit {
		t.Fatal(changes)
	}
}

func TestSignalServiceProbeRequiresCompatibleImageWithoutAccountAccess(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "arguments")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("SIGNAL_PROBE_FIXTURE_LOG", log)
	for _, v := range []struct {
		body string
		want bool
	}{{`{"version":2}`, true}, {`{"version":1}`, false}, {`{"version":2,"unexpected":true}`, false}, {`not-json`, false}} {
		script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SIGNAL_PROBE_FIXTURE_LOG\"\nprintf '%s\\n' '" + v.body + "'\n"
		if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		err := signalAccountProbe(context.Background(), "example.test/relay@sha256:"+strings.Repeat("a", 64))
		if (err == nil) != v.want {
			t.Fatal(v, err)
		}
		raw, e := os.ReadFile(log)
		if e != nil {
			t.Fatal(e)
		}
		for _, required := range []string{"--network=none\n", "--pull=never\n", "--read-only\n", "__signal-account-probe\n"} {
			if !strings.Contains(string(raw), required) {
				t.Fatal("probe isolation missing", string(raw))
			}
		}
		if strings.Contains(string(raw), "--volume") || strings.Contains(string(raw), "--mount") {
			t.Fatal("probe mounted account", string(raw))
		}
	}
}

func TestSignalServiceRejectsAlternateWorkerState(t *testing.T) {
	home := t.TempDir()
	configuration := filepath.Join(home, ".config/deploy/backup-worker/runtime.json")
	state := filepath.Join(home, ".local/state/deploy/backup-worker/state")
	for _, pair := range [][2]string{{configuration, filepath.Join(home, "foreign-state")}, {filepath.Join(home, "foreign-config"), state}} {
		if err := signalLinkWorker(home, pair[0], pair[1], "", false); err == nil || !strings.Contains(err.Error(), "installed worker configuration and state paths") {
			t.Fatal("alternate installation authority accepted", err)
		}
	}
}

type interruptedSignalService struct{}

func (interruptedSignalService) Inspect(context.Context) (signalaccount.ServiceState, error) {
	return signalaccount.ServiceState{Binding: protocol.Hash("fixture"), Active: true}, nil
}
func (interruptedSignalService) Stop(context.Context) error  { return errors.New("interrupted stop") }
func (interruptedSignalService) Start(context.Context) error { return errors.New("unexpected start") }

func TestSignalServiceMaintenanceBlocksInstallationChanges(t *testing.T) {
	home := t.TempDir()
	if err := requireNoSignalMaintenance(home); err != nil {
		t.Fatal("absent Signal enrollment", err)
	}
	path := filepath.Join(home, ".local/share/deploy/backup-worker/signal")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := requireNoSignalMaintenance(home); err != nil {
		t.Fatal(err)
	}
	id, _ := safefs.NewID("n")
	request := signalaccount.ServiceRequest{Operation: id, Name: "fixture", Binding: protocol.Hash("fixture")}
	launch := func(context.Context) error { t.Fatal("link launched before stop"); return nil }
	if _, err := signalaccount.MaintainLink(context.Background(), path, request, interruptedSignalService{}, launch); err == nil {
		t.Fatal("missing interruption")
	}
	if err := requireNoSignalMaintenance(home); err == nil {
		t.Fatal("unfinished maintenance permitted installation drift")
	}
	if err := requireLegacyWorkerSetup(home, &WorkerSetupJournal{}); err == nil {
		t.Fatal("setup bypassed maintenance gate")
	}
}
func TestSignalServiceUsesExistingInstallationCoordinator(t *testing.T) {
	home := t.TempDir()
	if l, err := signalInstallationLease(home); err == nil {
		l.Close()
		t.Fatal("uninstalled worker accepted")
	}
	d, err := safefs.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Mkdir(".local/state/deploy/locks"); err != nil {
		t.Fatal(err)
	}
	lock, err := d.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := signalInstallationLease(home); err == nil {
		l.Close()
		t.Fatal("concurrent setup bypassed")
	}
	lock.Close()
	l, err := signalInstallationLease(home)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

func TestSignalServiceOperationSelectionPreservesCompletedRetry(t *testing.T) {
	id, _ := safefs.NewID("n")
	other, _ := safefs.NewID("n")
	completed := signalaccount.MaintenanceStatus{Operation: id, Phase: "complete"}
	for _, status := range []signalaccount.MaintenanceStatus{completed, {Operation: id, Phase: "stopped"}, {Operation: id, Phase: "complete", Pending: true}} {
		for _, explicit := range []string{"", id} {
			got, err := selectSignalOperation(status, explicit, false)
			if err != nil || got != id {
				t.Fatal("retry allocated new link", got, err)
			}
		}
		if status.Phase != "complete" || status.Pending {
			for _, explicit := range []string{"", other} {
				if _, err := selectSignalOperation(status, explicit, explicit == ""); err == nil {
					t.Fatal("unfinished operation bypassed")
				}
			}
		}
	}
	for _, status := range []signalaccount.MaintenanceStatus{{Phase: "none"}, completed} {
		got, err := selectSignalOperation(status, "", true)
		if err != nil || !safefs.ValidID(got, "n") || got == id {
			t.Fatal("new operation", got, err)
		}
		got, err = selectSignalOperation(status, other, false)
		if err != nil || got != other {
			t.Fatal("explicit operation", got, err)
		}
	}
	if _, err := selectSignalOperation(completed, id, true); err == nil {
		t.Fatal("ambiguous flags accepted")
	}
	if _, err := selectSignalOperation(completed, "invalid", false); err == nil {
		t.Fatal("invalid ID accepted")
	}
}

func TestSignalServiceCLIValidatesRetryFlagsBeforeDryRun(t *testing.T) {
	old := ui.DryRun
	ui.DryRun = true
	t.Cleanup(func() { ui.DryRun = old })
	id, _ := safefs.NewID("n")
	for _, args := range [][]string{{"signal-link", "--local"}, {"signal-link", "--local", "--new-link"}, {"signal-link", "--local", "--operation", id}} {
		if err := runWorkerOperation(args); err != nil {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{{"signal-link", "--local", "--new-link", "--operation", id}, {"signal-link", "--local", "--operation", "invalid"}, {"status", "--local", "--new-link"}, {"status", "--local", "--operation", id}} {
		if err := runWorkerOperation(args); err == nil {
			t.Fatal("invalid retry flags accepted", args)
		}
	}
}
