package install

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type managedTelegramTransport struct {
	calls  int
	before func(*http.Request) error
}

func (p *managedTelegramTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.calls++
	if p.before != nil {
		if err := p.before(r); err != nil {
			return nil, err
		}
	}
	return (telegramCheckTransport{}).RoundTrip(r)
}

func managedTelegramFixture(t *testing.T, configure func(*ManagedSpec, *managedInstaller, *fakeManagedServices)) (ManagedSpec, managedInstaller, *fakeManagedServices, *managedTelegramTransport) {
	return managedTelegramFixtureConfigured(t, configure, nil)
}
func managedTelegramFixtureConfigured(t *testing.T, configure func(*ManagedSpec, *managedInstaller, *fakeManagedServices), runtime func(*Runtime)) (ManagedSpec, managedInstaller, *fakeManagedServices, *managedTelegramTransport) {
	t.Helper()
	var spec ManagedSpec
	var i managedInstaller
	var f *fakeManagedServices
	p := &managedTelegramTransport{}
	args, _ := telegramCheckCLIArgumentsConfigured(t, func(state *string, config string) {
		base := t.TempDir()
		if err := os.Chmod(base, 0700); err != nil {
			t.Fatal(err)
		}
		spec = ManagedSpec{Version: 1, Root: filepath.Join(base, "installation"), State: *state, UnitDirectory: filepath.Join(base, "units"), RuntimeFile: config, BinaryFile: filepath.Join(base, "deploy")}
		if err := os.Mkdir(spec.UnitDirectory, 0700); err != nil {
			t.Fatal(err)
		}
		binary, err := os.ReadFile("/bin/true")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(spec.BinaryFile, binary, 0700); err != nil {
			t.Fatal(err)
		}
		spec.BinarySHA256 = managedDigest(binary)
		f = &fakeManagedServices{t: t, units: spec.UnitDirectory, state: managedServiceState{Load: "not-found", Active: "inactive"}}
		i = managedInstaller{services: f, probe: func(context.Context, *safefs.Directory, string, string, bool) error { return nil }, recoveryProbe: func(context.Context, *safefs.Directory, string, string) error { return nil }, telegramProbe: func(context.Context, *safefs.Directory, string, string) error { return nil }, telegramTransport: p}
		if configure != nil {
			configure(&spec, &i, f)
		}
		*state = spec.State
		if _, err = i.prepare(context.Background(), "setup", spec); err != nil {
			t.Fatal(err)
		}
		if err = i.services.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}, runtime)
	spec.RecoveryCommitHash = args[6]
	if _, err := i.prepare(context.Background(), "adopt-recovery", spec); err != nil {
		t.Fatal(err)
	}
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(c.Telegram.TokenFile, []byte("123:fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	return spec, i, f, p
}

func managedTelegramReceiptForTest(t *testing.T, m ManagedManifest) managedTelegramReceipt {
	t.Helper()
	d, err := privateManagedDir(m.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	v, err := readManagedTelegramReceipt(d, m)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestManagedTelegramRenewalLifecycle(t *testing.T) {
	spec, i, f, p := managedTelegramFixture(t, nil)
	ctx := context.Background()
	beforeStarts := f.starts
	p.before = func(*http.Request) error {
		if f.state.Active != "inactive" {
			t.Fatal("provider called with active managed service")
		}
		d, err := privateManagedDir(spec.Root, true)
		if err != nil {
			return err
		}
		defer d.Close()
		for _, name := range []string{"operation.lock", "installation.lock"} {
			if l, e := d.Lock(name, true, false); e == nil {
				l.Close()
				t.Fatalf("%s released during renewal", name)
			}
		}
		return nil
	}
	m, err := i.prepareTelegramRenewal(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if m.TelegramRenewalID == "" || m.Mode != "verification" || f.starts != beforeStarts+1 || p.calls != 3 {
		t.Fatal(m, f.starts, p.calls)
	}
	r := managedTelegramReceiptForTest(t, m)
	if r.Renewal.IntakeEnabled || r.Renewal.Binding.ID != m.TelegramRenewalID {
		t.Fatal(r)
	}
	p.before = func(*http.Request) error { t.Fatal("completed resume contacted provider"); return nil }
	if _, err = i.resume(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
	spec.RecoveryCommitHash = ""
	binary, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	binary = append(binary, []byte("second executable revision")...)
	if err = os.WriteFile(spec.BinaryFile, binary, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(binary)
	updated, err := i.prepare(ctx, "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	if updated.TelegramRenewalID != m.TelegramRenewalID {
		t.Fatal("update lost renewal")
	}
	rolled, err := i.rollback(ctx, spec.Root, m.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.TelegramRenewalID != m.TelegramRenewalID || rolled.BinaryHash != m.BinaryHash || rolled.Revision == updated.Revision {
		t.Fatal("binary rollback lost renewal")
	}
	removed, err := i.uninstall(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if removed.TelegramRenewalID != m.TelegramRenewalID || removed.Mode != "uninstalled" {
		t.Fatal(removed)
	}
}

func TestManagedTelegramRenewalInterrupted(t *testing.T) {
	for _, at := range []string{"prepared", "stop-returned", "telegram-request", "telegram-renewed", "telegram-receipt", "stopped", "published", "reloaded", "start-returned"} {
		t.Run(at, func(t *testing.T) {
			spec, i, _, p := managedTelegramFixture(t, nil)
			stop := errors.New("interrupted")
			i.fault = func(point string) error {
				if point == at {
					return stop
				}
				return nil
			}
			if _, err := i.prepareTelegramRenewal(context.Background(), spec.Root); !errors.Is(err, stop) {
				t.Fatal(err)
			}
			calls := p.calls
			i.fault = nil
			m, err := i.resume(context.Background(), spec.Root)
			if err != nil {
				t.Fatal(err)
			}
			managedTelegramReceiptForTest(t, m)
			if calls > 0 && p.calls != calls {
				t.Fatal("completed provider operation repeated", calls, p.calls)
			}
		})
	}
}

func TestManagedTelegramRenewalRejectsAuthorityDrift(t *testing.T) {
	for _, kind := range []string{"service", "unit", "lease", "runtime", "request-loss", "receipt-loss", "capability"} {
		t.Run(kind, func(t *testing.T) {
			spec, i, f, p := managedTelegramFixture(t, nil)
			starts := f.starts
			switch kind {
			case "capability":
				i.telegramProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old binary") }
			case "request-loss":
				i.fault = func(at string) error {
					if at != "telegram-renewed" {
						return nil
					}
					d, e := privateManagedDir(spec.Root, true)
					if e != nil {
						return e
					}
					defer d.Close()
					j, e := readManagedJournal(d)
					if e != nil {
						return e
					}
					if e = os.Remove(filepath.Join(spec.Root, "operations", j.Intent.ID, "telegram-request.json")); e != nil {
						return e
					}
					return errors.New("lost request")
				}
			case "receipt-loss":
				i.fault = func(at string) error {
					if at != "published" {
						return nil
					}
					d, e := privateManagedDir(spec.Root, true)
					if e != nil {
						return e
					}
					defer d.Close()
					j, e := readManagedJournal(d)
					if e != nil {
						return e
					}
					if e = os.Remove(filepath.Join(spec.Root, "operations", j.Intent.ID, "telegram-receipt.json")); e != nil {
						return e
					}
					return errors.New("lost receipt")
				}
			default:
				p.before = func(*http.Request) error {
					switch kind {
					case "service":
						f.state.Active = "active"
					case "unit":
						return os.WriteFile(filepath.Join(spec.UnitDirectory, managedUnit), []byte("foreign unit"), 0600)
					case "lease":
						name := filepath.Join(spec.Root, "installation.lock")
						if e := os.Rename(name, name+".old"); e != nil {
							return e
						}
						return os.WriteFile(name, nil, 0600)
					case "runtime":
						d, e := privateManagedDir(spec.Root, true)
						if e != nil {
							return e
						}
						defer d.Close()
						j, e := readManagedJournal(d)
						if e != nil {
							return e
						}
						return os.WriteFile(j.Intent.After.Paths.Config, []byte("{}"), 0600)
					}
					return nil
				}
			}
			if _, err := i.prepareTelegramRenewal(context.Background(), spec.Root); err == nil {
				t.Fatal("changed authority accepted")
			}
			if f.starts != starts {
				t.Fatal("failed renewal restarted service")
			}
			if kind == "capability" && p.calls != 0 {
				t.Fatal("unsupported binary contacted provider")
			}
			if kind == "request-loss" || kind == "receipt-loss" {
				i.fault = nil
				calls := p.calls
				if _, err := i.resume(context.Background(), spec.Root); err == nil {
					t.Fatal("lost authority reconstructed")
				}
				if p.calls != calls {
					t.Fatal("missing evidence contacted provider")
				}
			}
		})
	}
}

func TestManagedTelegramRenewalCLIDryRun(t *testing.T) {
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	root := filepath.Join(t.TempDir(), "absent")
	if err := RunCLI([]string{"renew-telegram", "--local", "--root", root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("dry run created state")
	}
	for _, args := range [][]string{{"renew-telegram", "--root", root}, {"renew-telegram", "--local", "--root", "relative"}, {"renew-telegram", "--local", "--root", root, "--spec", "other"}, {"renew-telegram", "--local", "--root", root, "--activate"}} {
		if err := RunCLI(args); err == nil {
			t.Fatal("invalid flags accepted", args)
		}
	}
}

func TestManagedTelegramRenewalSuccessor(t *testing.T) {
	spec, i, _, p := managedTelegramFixture(t, nil)
	ctx := context.Background()
	first, err := i.prepareTelegramRenewal(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	previous := managedTelegramReceiptForTest(t, first)
	second, err := i.prepareTelegramRenewal(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	next := managedTelegramReceiptForTest(t, second)
	if next.Renewal.Binding.PreviousID != first.TelegramRenewalID || next.Renewal.Binding.PreviousHash != previous.Renewal.RenewalHash || next.Renewal.Binding.Generation != 2 || p.calls != 6 {
		t.Fatal(next, p.calls)
	}
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = worker.AuditTelegramHandoffRenewal(ctx, spec.State, c.Worker, spec.RecoveryCommitHash, c.Telegram.Account, previous.Renewal.RenewalHash); err == nil {
		t.Fatal("superseded renewal audited as current")
	}
	if _, err = worker.AuditTelegramHandoffRenewal(ctx, spec.State, c.Worker, spec.RecoveryCommitHash, c.Telegram.Account, next.Renewal.RenewalHash); err != nil {
		t.Fatal(err)
	}
}

func TestManagedTelegramHelperUpgrade(t *testing.T) {
	spec, i, f, _ := managedTelegramFixture(t, nil)
	ctx := context.Background()
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	j, err := readManagedJournal(d)
	if err != nil {
		t.Fatal(err)
	}
	before := j.Intent.After
	oldHelper := managedHelperName(before)
	i.telegramProbe = func(_ context.Context, _ *safefs.Directory, name, _ string) error {
		if name == oldHelper {
			return errors.New("old helper cannot resume renewal")
		}
		return nil
	}
	i.helperProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	starts := f.starts
	if _, err = i.prepareTelegramRenewal(ctx, spec.Root); err == nil || f.starts != starts {
		t.Fatal("unsupported renewal changed service", err)
	}
	spec.RecoveryCommitHash = ""
	// An ordinary update retains the old helper and therefore cannot unlock renewal.
	updated, err := i.prepare(ctx, "update", spec)
	if err != nil {
		t.Fatal(err)
	}
	if updated.HelperRevision != before.HelperRevision {
		t.Fatal("ordinary update replaced helper")
	}
	if _, err = i.prepareTelegramRenewal(ctx, spec.Root); err == nil {
		t.Fatal("old helper accepted")
	}
	i.helperProbe = func(context.Context, *safefs.Directory, string, string) error {
		return errors.New("helper cannot resume upgrade")
	}
	starts = f.starts
	if _, err = i.prepare(ctx, "upgrade-helper", spec); err == nil || f.starts != starts {
		t.Fatal("unsupported helper upgrade changed service", err)
	}
	unchanged, err := readManagedJournal(d)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Phase != "complete" || protocol.Hash(unchanged.Intent.After) != protocol.Hash(updated) {
		t.Fatal("unsupported helper upgrade changed intent")
	}
	i.helperProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	upgraded, err := i.prepare(ctx, "upgrade-helper", spec)
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.HelperRevision != upgraded.Revision || upgraded.HelperRevision == before.HelperRevision || upgraded.Schema != before.Schema || upgraded.Identity != before.Identity || upgraded.ConfigHash != before.ConfigHash || upgraded.RecoveryCommitHash != before.RecoveryCommitHash {
		t.Fatal(upgraded)
	}
	if _, err = ownedManagedFile(d, oldHelper, maxManagedBinary); err != nil {
		t.Fatal("old helper lost", err)
	}
	if _, _, _, err = reviewManagedRollback(spec.Root, before.Revision); err == nil {
		t.Fatal("rollback crossed helper boundary")
	}
	renewed, err := i.prepareTelegramRenewal(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.TelegramRenewalID == "" {
		t.Fatal("renewal not published")
	}
	// Helper promotion also retains completed renewal evidence.
	after, err := i.prepare(ctx, "upgrade-helper", spec)
	if err != nil {
		t.Fatal(err)
	}
	if after.TelegramRenewalID != renewed.TelegramRenewalID {
		t.Fatal("helper upgrade lost renewal")
	}
	managedTelegramReceiptForTest(t, after)
	// The frozen operation is self-validating, including helper/schema constraints.
	j, err = readManagedJournal(d)
	if err != nil {
		t.Fatal(err)
	}
	altered := j
	altered.Intent.After.HelperHash = protocol.Hash("foreign")
	if altered.validate() == nil {
		t.Fatal("foreign helper accepted")
	}
}
