package install

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func activationCheck(t *testing.T, r ActivationReview, scope, stage, state string) worker.AdmissionCheck {
	t.Helper()
	if r.ActivationAllowed || r.DeliveryEnabled || r.IntakeEnabled || r.ExecutionEnabled || !r.RequiresActivationRecheck {
		t.Fatal("review granted authority", r)
	}
	for _, c := range r.Checks {
		if c.Scope == scope && c.Stage == stage {
			if c.State != state {
				t.Fatalf("%s/%s = %+v; want %s", scope, stage, c, state)
			}
			return c
		}
	}
	t.Fatalf("missing %s/%s: %+v", scope, stage, r.Checks)
	return worker.AdmissionCheck{}
}

func activationTreeHash(t *testing.T, roots ...string) string {
	t.Helper()
	entries := map[string]string{}
	for _, root := range roots {
		if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			entries[path] = info.Mode().String()
			if info.Mode().IsRegular() {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				entries[path] += managedDigest(data)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return protocol.Hash(entries)
}

func TestActivationReviewQualificationAndCredentials(t *testing.T) {
	spec, i, c, report := qualificationFixture(t)
	f := i.services.(*fakeManagedServices)
	qualificationPass(&report)
	if status, err := qualifyFixture(t, spec.Root, report); err != nil || !status.EvidenceComplete {
		t.Fatal(status, err)
	}
	if err := f.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory)
	starts, stops := f.starts, f.stops
	read := func() ActivationReview {
		t.Helper()
		r, err := reviewActivation(context.Background(), spec.Root, f, &fakeSignalServiceManager{}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := read()
	activationCheck(t, r, "qualification", "evidence", "satisfied")
	activationCheck(t, r, "provider/telegram", "binding", "satisfied")
	activationCheck(t, r, "runtime", "state", "satisfied")
	activationCheck(t, r, "installation", "activation", "satisfied")
	if r.ManifestHash != protocol.Hash(report.Binding.Manifest) || r.Revision != report.Binding.Manifest.Revision || r.CheckedAt.IsZero() {
		t.Fatal("review lost revision binding", r)
	}
	if activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory) != before || starts != f.starts || stops != f.stops {
		t.Fatal("review modified installation or service")
	}
	// A later failed report supersedes success, even with unchanged inputs.
	report.ID, _ = safefs.NewID("n")
	report.Checks[0].Result = "failed"
	if _, err := qualifyFixture(t, spec.Root, report); err != nil {
		t.Fatal(err)
	}
	activationCheck(t, read(), "qualification", "evidence", "blocked")
	if err := os.WriteFile(c.Providers["telegram"].ChatFile, []byte("changed-chat"), 0600); err != nil {
		t.Fatal(err)
	}
	r = read()
	activationCheck(t, r, "provider/telegram", "binding", "blocked")
	activationCheck(t, r, "qualification", "evidence", "blocked")
	if err := os.Remove(c.Providers["telegram"].TokenFile); err != nil {
		t.Fatal(err)
	}
	r = read()
	activationCheck(t, r, "provider/telegram", "binding", "blocked")
	activationCheck(t, r, "qualification", "evidence", "blocked")
	data, _ := json.Marshal(r)
	if bytes.Contains(data, []byte("123:secret")) || bytes.Contains(data, []byte("changed-chat")) {
		t.Fatal("review leaked credentials")
	}
}

func TestActivationReviewDrainAndLeaseBoundaries(t *testing.T) {
	spec, i, _, _ := qualificationFixture(t)
	f := i.services.(*fakeManagedServices)
	read := func() ActivationReview {
		t.Helper()
		r, err := reviewActivation(context.Background(), spec.Root, f, &fakeSignalServiceManager{}, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	activationCheck(t, read(), "runtime", "drain", "blocked")
	if err := f.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lease, err := d.Lock("installation.lock", false, false)
	if err != nil {
		t.Fatal(err)
	}
	activationCheck(t, read(), "runtime", "drain", "blocked")
	lease.Close()
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(spec.State, c.Worker, false)
	if err != nil {
		t.Fatal(err)
	}
	r := read()
	activationCheck(t, r, "runtime", "drain", "satisfied")
	activationCheck(t, r, "runtime", "state", "blocked")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(spec.Root, "installation.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewActivation(context.Background(), spec.Root, f, &fakeSignalServiceManager{}, time.Now); err == nil {
		t.Fatal("review recreated a missing installation lease")
	}
}

func TestActivationReviewManagedSignalAndTelegram(t *testing.T) {
	spec, i, f, p, _ := managedSignalFixtureProviders(t, true)
	ctx := context.Background()
	s := signalServiceSpecFor(t, p, spec.UnitDirectory, spec.BinaryFile)
	sm := &fakeSignalServiceManager{units: s.UnitDirectory}
	installer := signalServiceInstaller{manager: sm, probe: func(context.Context, *safefs.Directory, SignalServiceManifest) error { return nil }}
	service, err := installer.setup(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	spec.SignalVerification.ServiceRoot, spec.SignalVerification.ServiceHash = s.Root, service.ManifestHash
	i.signalCheck = func(ctx context.Context, _ ManagedManifest) error { return sm.Verify(ctx, service) }
	m, err := i.prepare(ctx, "adopt-signal", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// Exercise the real clock contract: a completed renewal is still unusable
	// during its five-second settling interval.
	receipt := managedTelegramReceiptForTest(t, m)
	if wait := time.Until(receipt.Renewal.Cursor.NotBefore) + 20*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}
	before := activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory, p.Spec.Root, s.Root, p.Spec.AccountState, p.Installation.Source, p.Installation.Destination)
	polls := i.telegramTransport.(*managedTelegramTransport).calls
	r, err := reviewActivation(ctx, spec.Root, f, sm, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	activationCheck(t, r, "runtime", "state", "satisfied")
	activationCheck(t, r, "signal", "supervision", "satisfied")
	activationCheck(t, r, "signal", "gateway_cursor", "satisfied")
	activationCheck(t, r, "signal", "activation", "blocked")
	activationCheck(t, r, "commands", "activation", "blocked")
	activationCheck(t, r, "intake/telegram/123", "handoff", "satisfied")
	for name, profile := range mustRuntime(t, m.Paths.Config).Providers {
		if profile.Kind == "signal" {
			activationCheck(t, r, "provider/"+name, "binding", "blocked")
		}
	}
	if activationTreeHash(t, spec.Root, spec.State, spec.UnitDirectory, p.Spec.Root, s.Root, p.Spec.AccountState, p.Installation.Source, p.Installation.Destination) != before || polls != i.telegramTransport.(*managedTelegramTransport).calls {
		t.Fatal("review changed recovery evidence or polled Telegram")
	}
	if err := sm.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	r, err = reviewActivation(ctx, spec.Root, f, sm, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	activationCheck(t, r, "signal", "supervision", "blocked")
}

func mustRuntime(t *testing.T, path string) Runtime {
	t.Helper()
	c, err := ReadRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestActivationReviewCLIValidationAndDryRun(t *testing.T) {
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	root := filepath.Join(t.TempDir(), "absent")
	var out bytes.Buffer
	if err := runActivationReviewCLI([]string{"--local", "--root", root}, &out); err != nil || !strings.Contains(out.String(), "checks deferred") {
		t.Fatal(out.String(), err)
	}
	if err := RunCLI([]string{"activation-review", "--local", "--root", root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("dry-run created installation")
	}
	for _, args := range [][]string{{"--root", root}, {"--local", "--root", "relative"}, {"--local", "--root", root, "extra"}, {"--local", "--root", root, "--apply"}, {"--local", "--root", root, "--config", root}} {
		out.Reset()
		if err := runActivationReviewCLI(args, &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid flags accepted or emitted review", args, out.String(), err)
		}
	}
}

func TestActivationReviewSignalOutboundPins(t *testing.T) {
	for _, kind := range []string{"old-socket", "old-identity", "legacy", "aligned"} {
		t.Run(kind, func(t *testing.T) {
			spec, i, f, _ := managedFixture(t)
			signal, _ := signalRuntimeCLIConfiguration(t)
			var c Runtime
			raw, err := os.ReadFile(spec.RuntimeFile)
			if err != nil || json.Unmarshal(raw, &c) != nil {
				t.Fatal("fixture runtime", err)
			}
			c.Signal, c.Worker.Interaction = signal.Signal, signal.Worker.Interaction
			destination := c.Worker.Interaction.Destinations[0]
			profile := provider.Profile{Kind: "signal", Conversation: &destination, Destination: destination.Fingerprint(), Socket: c.Signal.Socket, RelayIdentity: c.Signal.InboxIdentity}
			switch kind {
			case "old-socket":
				profile.Socket = "/old/relay.sock"
			case "old-identity":
				profile.RelayIdentity.Recovery = "old-epoch"
			case "legacy":
				profile.Conversation = nil
			}
			c.Providers["signal"] = profile
			writeRuntime(t, spec.RuntimeFile, c)
			if _, err := i.prepare(context.Background(), "setup", spec); err != nil {
				t.Fatal(err)
			}
			if err := f.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			r, err := reviewActivation(context.Background(), spec.Root, f, &fakeSignalServiceManager{}, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			state := "blocked"
			if kind == "aligned" {
				state = "satisfied"
			}
			activationCheck(t, r, "provider/signal", "binding", state)
			activationCheck(t, r, "signal", "endpoint", "blocked")
			activationCheck(t, r, "signal", "activation", "blocked")
		})
	}
}

type activationInspectServices struct {
	managedServices
	inspect func(context.Context) (managedServiceState, error)
}

func (s activationInspectServices) Inspect(ctx context.Context) (managedServiceState, error) {
	return s.inspect(ctx)
}

func TestActivationReviewRefusesAuthorityLoss(t *testing.T) {
	for _, kind := range []string{"restart", "unit", "installation-lease", "operation-lease", "root"} {
		t.Run(kind, func(t *testing.T) {
			spec, i, _, _ := qualificationFixture(t)
			f := i.services.(*fakeManagedServices)
			if err := f.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			inspections := 0
			services := activationInspectServices{managedServices: f, inspect: func(ctx context.Context) (managedServiceState, error) {
				inspections++
				if inspections == 2 {
					switch kind {
					case "restart":
						f.state.Active = "active"
					case "unit":
						if err := os.WriteFile(filepath.Join(spec.UnitDirectory, managedUnit), []byte("foreign"), 0600); err != nil {
							t.Fatal(err)
						}
					case "root":
						if err := os.Rename(spec.Root, spec.Root+"-retained"); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(spec.Root, 0700); err != nil {
							t.Fatal(err)
						}
					default:
						name := "operation.lock"
						if kind == "installation-lease" {
							name = "installation.lock"
						}
						path := filepath.Join(spec.Root, name)
						if err := os.Rename(path, path+".old"); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, nil, 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				return f.Inspect(ctx)
			}}
			if _, err := reviewActivation(context.Background(), spec.Root, services, &fakeSignalServiceManager{}, time.Now); err == nil {
				t.Fatal("authority loss returned a review")
			}
		})
	}
}
