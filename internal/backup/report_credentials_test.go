//go:build linux

package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/testutil/configfixture"
)

type credentialReportServices struct {
	t                                   *testing.T
	paths                               WorkerUnitPaths
	loaded                              map[string]string
	stopped, active, foreign, undrained bool
	stops, reloads, starts              int
}

func (s *credentialReportServices) Inspect(_ context.Context, name, path string, stopped, enabled, pendingReload bool) error {
	if s.foreign || path != filepath.Join(s.paths.unitDirectory(), name) {
		return fmt.Errorf("foreign report unit")
	}
	if stopped && (!s.stopped || s.undrained) {
		return fmt.Errorf("report process not drained")
	}
	if enabled && !s.active {
		return fmt.Errorf("report trigger not active")
	}
	if !pendingReload {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != s.loaded[name] {
			return fmt.Errorf("report unit needs reload")
		}
	}
	return nil
}
func (s *credentialReportServices) Stop(context.Context) error {
	s.stops++
	s.stopped = true
	s.active = false
	return nil
}
func (s *credentialReportServices) Reload(context.Context) error {
	s.reloads++
	if !s.stopped || s.undrained {
		s.t.Fatal("reload before report drain")
	}
	for name := range s.loaded {
		raw, err := os.ReadFile(filepath.Join(s.paths.unitDirectory(), name))
		testutil.Must(s.t, err)
		s.loaded[name] = string(raw)
	}
	return nil
}
func (s *credentialReportServices) Start(context.Context) error {
	s.starts++
	s.active = true
	s.stopped = false
	return nil
}

func reportCredentialFixture(t *testing.T, active bool) (*safefs.Directory, WorkerInstallation, WorkerUnitPaths, setupProviderReports, *credentialReportServices) {
	t.Helper()
	home := t.TempDir()
	paths := WorkerUnitPaths{Home: home, Config: filepath.Join(home, "runtime.json"), State: filepath.Join(home, "state"), Keys: filepath.Join(home, "keys"), RelayState: filepath.Join(home, "signal"), Archive: filepath.Join(home, "archive")}
	c := WorkerInstallation{Policy: configfixture.WorkerPolicy(t)}
	identity := notify.Identity{Worker: testutil.ID(t, "w"), Recovery: testutil.ID(t, "r")}
	profile := install.ManagedReportProfile{ManifestHash: notify.Hash("before"), Identity: identity, UnitDirectory: paths.unitDirectory(), Credentials: []string{filepath.Join(home, "old-token"), filepath.Join(home, "old-chat")}}
	credentials := append(append([]string(nil), profile.Credentials...), filepath.Join(home, "new-token"), filepath.Join(home, "new-chat"))
	v, err := newProviderReports(c, paths, profile, credentials, active)
	testutil.Must(t, err)
	testutil.Must(t, os.MkdirAll(paths.unitDirectory(), 0700))
	loaded := map[string]string{}
	for name, body := range v.BeforeUnits {
		testutil.Must(t, os.WriteFile(filepath.Join(paths.unitDirectory(), name), []byte(body), 0600))
		loaded[name] = body
	}
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	t.Cleanup(func() { root.Close() })
	return root, c, paths, v, &credentialReportServices{t: t, paths: paths, loaded: loaded, active: active, stopped: !active}
}

func TestReportCredentialExclusionsResumePublicationBeforeRestart(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, point := range []string{"report-stopped", "report-files", "report-reloaded"} {
			t.Run(fmt.Sprintf("active-%t/%s", active, point), func(t *testing.T) {
				root, c, paths, v, services := reportCredentialFixture(t, active)
				ctx := context.Background()
				lost := errors.New("lost report publication reply")
				err := v.stage(ctx, root, c, paths, services, func(at string) error {
					if at == point {
						return lost
					}
					return nil
				})
				if !errors.Is(err, lost) {
					t.Fatal("missed publication boundary", err)
				}
				if services.active || services.starts != 0 {
					t.Fatal("reporting restarted before gateway and scope completion")
				}
				testutil.Must(t, v.stage(ctx, root, c, paths, services, func(string) error { return nil }))
				for _, path := range v.Credentials {
					if !strings.Contains(services.loaded["deploy-backup-report.service"], reportUnitPathQuote("-"+path)) {
						t.Fatal("credential became readable", path)
					}
				}
				// The enclosing coordinator calls this only after publishing its scope
				// and removing the installation gate. Retrying it is harmless.
				for range 2 {
					testutil.Must(t, v.resumeTriggers(ctx, root, paths, services))
				}
				if services.active != active || !active && services.starts != 0 {
					t.Fatal("changed original trigger disposition")
				}
				owned := v.ownedFiles(map[string]string{"unrelated": notify.Hash("unchanged")}, paths)
				if owned["unrelated"] != notify.Hash("unchanged") {
					t.Fatal("removed unrelated owned file")
				}
				for name, body := range v.AfterUnits {
					if owned[(InstallationPaths{Home: paths.Home}).OwnedPath(filepath.Join(paths.unitDirectory(), name))] != sharedSetupDigest([]byte(body)) {
						t.Fatal("lost report unit pin")
					}
				}
			})
		}
	}
}

func TestReportCredentialExclusionsRejectDriftBeforePublication(t *testing.T) {
	for _, damage := range []string{"unit", "foreign", "process", "drop-exclusion", "forged-unit", "required-path"} {
		t.Run(damage, func(t *testing.T) {
			root, c, paths, v, services := reportCredentialFixture(t, true)
			path := filepath.Join(paths.unitDirectory(), "deploy-backup-report.service")
			switch damage {
			case "unit":
				testutil.Must(t, os.WriteFile(path, []byte("foreign unit"), 0600))
			case "foreign":
				services.foreign = true
			case "process":
				services.undrained = true
			case "drop-exclusion":
				v.Credentials = v.Credentials[:1]
			case "forged-unit":
				v.AfterUnits["deploy-backup-report.service"] += "ExecStart=/foreign\n"
			case "required-path":
				v.Credentials = []string{paths.Home}
			}
			before, err := os.ReadFile(path)
			testutil.Must(t, err)
			if err = v.stage(context.Background(), root, c, paths, services, func(string) error { return nil }); err == nil {
				t.Fatal("unsafe exclusion migration accepted")
			}
			after, err := os.ReadFile(path)
			testutil.Must(t, err)
			if string(before) != string(after) || services.reloads != 0 || services.starts != 0 {
				t.Fatal("refusal published unit or restarted reporting")
			}
		})
	}
}
