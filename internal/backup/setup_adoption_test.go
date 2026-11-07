package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func setupAdoptionFixture(t *testing.T, aliases ...string) (string, WorkerSetupRequest) {
	t.Helper()
	home, state, j := setupObservationFixture(t, aliases...)
	completePolicyReviewSources(t, home, &j)
	testutil.Must(t, state.Close())
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var c WorkerInstallation
	testutil.Must(t, root.ReadJSON(workerRuntimeConfig, &c))
	c.Image = "registry.invalid/backup@sha256:" + strings.Repeat("a", 64)
	body, err := json.Marshal(c)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(home, workerRuntimeConfig), body, 0600))
	j.OwnedFiles[workerRuntimeConfig] = sharedSetupDigest(body)
	j.HomeFilesystem, err = homeArchiveFilesystem(c.Policy.Storage.Path)
	testutil.Must(t, err)
	helper := j.Declaration.HelperSHA256
	j.Declaration = nil
	testutil.Must(t, root.WriteJSON(backupSetupFile, j, true))
	p := c.Policy
	d := config.DesiredWorkers{Backup: &config.BackupDeclaration{Sources: p.Sources, Storage: config.DesiredStorage{Mode: p.Storage.Mode, Path: p.Storage.Path, RequireMount: p.Storage.RequireMount, MinFreeBytes: p.Storage.MinFreeBytes}, Schedule: p.Schedule, Timezone: p.Timezone, Freshness: p.Freshness, MaxRunTime: p.MaxRunTime, MaxSnapshotBytes: p.MaxSnapshotBytes, Timeouts: p.Timeouts, Retention: p.Retention, Notifications: config.DesiredBackupNotifications{WorkerNotifications: p.Notifications}, Monitoring: p.Monitoring, RestoreChecks: p.RestoreChecks}}
	hash, err := d.Fingerprint()
	testutil.Must(t, err)
	r := WorkerSetupRequest{Schema: 1, Operation: "adopt", Policy: p, Image: c.Image, RelayImage: c.RelayImage, Declaration: &SetupDeclaration{Version: 1, Desired: &d, DesiredHash: hash, HelperSHA256: helper, Sources: map[string]string{}, UID: os.Getuid(), User: "fixture", Home: home, Architecture: runtime.GOARCH}}
	for _, s := range c.Sources {
		known, err := os.ReadFile(s.KnownHosts)
		testutil.Must(t, err)
		r.Sources = append(r.Sources, SetupSource{Alias: s.Alias, Host: s.Host, User: s.User, Port: s.Port, KnownHosts: string(known)})
		r.Declaration.Sources[s.Alias] = strings.Repeat("a", 64)
	}
	return home, r
}

func TestBackupAdoptionTransportNeverEnrollsSources(t *testing.T) {
	for _, stage := range []string{"complete", "prepared"} {
		t.Run(stage, func(t *testing.T) {
			request := WorkerSetupRequest{Schema: 1, Operation: "adopt", Sources: []SetupSource{{Alias: "source"}}}
			response := WorkerSetupResponse{Schema: 1, Stage: stage, WorkerID: testutil.ID(t, "w"), ArchiveID: testutil.ID(t, "a"), Unchanged: true}
			var replies, sent bytes.Buffer
			raw, err := json.Marshal(response)
			testutil.Must(t, err)
			testutil.Must(t, protocol.WriteBackupFrame(&replies, raw))
			calls := 0
			got, err := exchangeWorkerSetup(request, &replies, &sent, func(SetupSource, string, string) (source.SourceEnrollmentResult, error) {
				calls++
				return source.SourceEnrollmentResult{}, nil
			})
			if calls != 0 || (err == nil) != (stage == "complete") {
				t.Fatal("adoption accepted setup/source effects", got, err, calls)
			}
		})
	}
}

func runSetupAdoptionFixture(home string, r WorkerSetupRequest, boundary func(string) error) (WorkerSetupResponse, error) {
	root, err := safefs.Open(home)
	if err != nil {
		return WorkerSetupResponse{}, err
	}
	defer root.Close()
	lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
	if err != nil {
		return WorkerSetupResponse{}, err
	}
	defer lock.Close()
	return adoptWorkerSetupLocked(context.Background(), root, home, r, boundary)
}

func TestStandaloneBackupAdoptionPreservesRuntimeAndResumes(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless adoption")
	}
	for _, at := range []string{"backup-adoption-intent", "backup-adoption-published"} {
		t.Run(at, func(t *testing.T) {
			home, r := setupAdoptionFixture(t)
			before := policyReviewTree(t, home)
			stop := errors.New("lost backup adoption response")
			_, err := runSetupAdoptionFixture(home, r, func(boundary string) error {
				if boundary == at {
					return stop
				}
				return nil
			})
			if !errors.Is(err, stop) {
				t.Fatal("boundary not reached", err)
			}
			if at == "backup-adoption-intent" {
				o, err := InspectSetup(context.Background(), home)
				testutil.Must(t, err)
				if o.Stage != "adoption_pending" {
					t.Fatal("pending adoption hidden", o)
				}
			}
			r.Operation = "apply"
			var got WorkerSetupResponse
			for range 2 {
				got, err = runSetupAdoptionFixture(home, r, nil)
				testutil.Must(t, err)
				if got.Stage != "complete" || !got.Unchanged {
					t.Fatal(got)
				}
			}
			after := policyReviewTree(t, home)
			delete(before, filepath.Join(home, backupSetupFile))
			delete(after, filepath.Join(home, backupSetupFile))
			delete(after, filepath.Join(home, setupAdoptionFile))
			if !reflect.DeepEqual(before, after) {
				t.Fatal("adoption changed runtime, archive, database or source files")
			}
			root, err := safefs.Open(home)
			testutil.Must(t, err)
			defer root.Close()
			var j WorkerSetupJournal
			testutil.Must(t, root.ReadJSON(backupSetupFile, &j))
			if j.Adoption == nil || j.Declaration.DesiredHash != r.Declaration.DesiredHash || got.WorkerID != j.WorkerID || got.ArchiveID != j.ArchiveID {
				t.Fatal("ownership receipt lost")
			}
			if pending, err := inspectSetupAdoption(root, j); err != nil || pending {
				t.Fatal(pending, err)
			}
			var c WorkerInstallation
			testutil.Must(t, readInstalledWorkerJSON(root, &c))
			r.Policy.Schedule = "daily"
			testutil.Must(t, validateDeclarativeBackupUpdate(c, j.Declaration, r))
			o, policy, err := InspectSetupPolicy(context.Background(), home)
			testutil.Must(t, err)
			if o.Stage != "complete" || policy == nil || o.WorkerID != got.WorkerID {
				t.Fatal("adopted policy not inspectable", o)
			}
		})
	}
}

func TestStandaloneBackupAdoptionRefusesChangedOrMissingEvidence(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless adoption")
	}
	for _, damage := range []string{"implicit", "policy", "helper", "source", "trust", "key", "shared", "signal", "paths", "disabled", "journal", "lost-intent", "pending-policy", "busy", "wal"} {
		t.Run(damage, func(t *testing.T) {
			home, r := setupAdoptionFixture(t)
			switch damage {
			case "implicit":
				r.Operation = "apply"
			case "policy":
				r.Policy.Schedule = "weekly"
			case "helper":
				r.Declaration.HelperSHA256 = strings.Repeat("b", 64)
			case "source":
				r.Sources[0].Host = "other.invalid"
			case "trust":
				r.Sources[0].KnownHosts = "changed trust"
			case "key":
				testutil.Must(t, os.Remove(filepath.Join(home, workerTrustDirectory, r.Sources[0].Alias+".key")))
			case "shared":
				r.Policy.Notifications.Mode = "shared"
			case "signal":
				r.Declaration.NotificationRoot = "/retained/signal"
			case "paths":
				r.Declaration.Desired.Backup.Paths.State = filepath.Join(home, "relocated")
				r.Declaration.DesiredHash, _ = r.Declaration.Desired.Fingerprint()
			case "disabled":
				disabled := false
				r.Declaration.Desired.Backup.Enabled = &disabled
				r.Declaration.DesiredHash, _ = r.Declaration.Desired.Fingerprint()
			case "journal":
				testutil.Must(t, os.Remove(filepath.Join(home, backupSetupFile)))
			case "lost-intent":
				_, err := runSetupAdoptionFixture(home, r, nil)
				testutil.Must(t, err)
				testutil.Must(t, os.Remove(filepath.Join(home, setupAdoptionFile)))
			case "pending-policy":
				if _, err := runSetupAdoptionFixture(home, r, func(string) error { return errors.New("stop") }); err == nil {
					t.Fatal("did not stop")
				}
				r.Declaration.Desired.Backup.Schedule = "daily"
				r.Declaration.DesiredHash, _ = r.Declaration.Desired.Fingerprint()
			case "busy":
				root, err := safefs.Open(home)
				testutil.Must(t, err)
				defer root.Close()
				lock, err := root.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
				testutil.Must(t, err)
				defer lock.Close()
			case "wal":
				testutil.Must(t, os.WriteFile(filepath.Join(home, ".local/state/deploy/backup-worker/state/worker.db-wal"), []byte("pending WAL"), 0600))
			}
			before := policyReviewTree(t, home)
			if _, err := runSetupAdoptionFixture(home, r, nil); err == nil {
				t.Fatal("accepted drift or implicit adoption")
			}
			if after := policyReviewTree(t, home); !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed retained files")
			}
		})
	}
}

func TestStandaloneBackupAdoptionProcessDeath(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless adoption")
	}
	if path := os.Getenv("DEPLOY_BACKUP_ADOPTION_INPUT"); path != "" {
		raw, err := os.ReadFile(path)
		testutil.Must(t, err)
		var r WorkerSetupRequest
		testutil.Must(t, json.Unmarshal(raw, &r))
		_, err = runSetupAdoptionFixture(r.Declaration.Home, r, func(at string) error {
			if at == os.Getenv("DEPLOY_BACKUP_ADOPTION_AT") {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
			}
			return nil
		})
		t.Fatal("crash boundary missed", err)
	}
	for _, at := range []string{"backup-adoption-intent", "backup-adoption-published"} {
		t.Run(at, func(t *testing.T) {
			home, r := setupAdoptionFixture(t)
			raw, err := json.Marshal(r)
			testutil.Must(t, err)
			file := filepath.Join(home, "crash-request.json")
			testutil.Must(t, os.WriteFile(file, raw, 0600))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStandaloneBackupAdoptionProcessDeath$")
			cmd.Env = append(os.Environ(), "DEPLOY_BACKUP_ADOPTION_INPUT="+file, "DEPLOY_BACKUP_ADOPTION_AT="+at)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatalf("not killed: %v %s", err, out)
			}
			for range 2 {
				_, err = runSetupAdoptionFixture(home, r, nil)
				testutil.Must(t, err)
			}
		})
	}
}
