package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/safefs"
)

func providerUpdateFixture(t *testing.T) (ManagedSpec, managedInstaller, *fakeManagedServices, ManagedManifest) {
	t.Helper()
	spec, i, f, _ := managedFixture(t)
	i.bootstrapProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	spec.State = filepath.Join(filepath.Dir(spec.State), "unbound")
	raw, err := os.ReadFile(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	var input RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	input.Bootstrap, input.Worker.Producers = true, nil
	p := input.Providers["telegram"]
	p.TokenFile, p.ChatFile = filepath.Join(t.TempDir(), "token"), filepath.Join(t.TempDir(), "chat")
	if err = os.WriteFile(p.TokenFile, []byte("123:secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(p.ChatFile, []byte("chat-one"), 0600); err != nil {
		t.Fatal(err)
	}
	input.Providers["telegram"] = p
	c, err := input.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	op, _ := safefs.NewID("n")
	if _, err = worker.BootstrapGateway(context.Background(), spec.State, op, c.Worker); err != nil {
		t.Fatal(err)
	}
	raw, _ = input.Bytes()
	if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := i.ensureVerification(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return spec, i, f, m
}

func changeBootstrapProviderInput(t *testing.T, spec ManagedSpec, audience bool) RuntimeInput {
	t.Helper()
	raw, err := os.ReadFile(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	var input RuntimeInput
	if err = safefs.DecodeJSON(raw, &input, safefs.MaxRecordBytes); err != nil {
		t.Fatal(err)
	}
	for name, p := range input.Providers {
		if p.Kind != "telegram" {
			continue
		}
		token, err := os.ReadFile(p.TokenFile)
		if err != nil {
			t.Fatal(err)
		}
		chat, err := os.ReadFile(p.ChatFile)
		if err != nil {
			t.Fatal(err)
		}
		bot, _, _ := strings.Cut(strings.TrimSpace(string(token)), ":")
		token = []byte(bot + ":rotated_fixture")
		if audience {
			chat = []byte("987654321")
		}
		p.TokenFile, p.ChatFile = filepath.Join(t.TempDir(), "token"), filepath.Join(t.TempDir(), "chat")
		if err = os.WriteFile(p.TokenFile, token, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(p.ChatFile, chat, 0600); err != nil {
			t.Fatal(err)
		}
		p.Destination = provider.TelegramIdentity(string(token), strings.TrimSpace(string(chat)))
		input.Providers[name] = p
	}
	raw, err = input.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return input
}

func TestManagedBootstrapProvidersDurableBoundaries(t *testing.T) {
	for _, audience := range []bool{false, true} {
		for _, boundary := range []string{"prepared", "stop-returned", "bootstrap-providers-migrated", "files", "published", "start-returned", "complete"} {
			t.Run(boundary+map[bool]string{false: "/token", true: "/audience"}[audience], func(t *testing.T) {
				ctx := context.Background()
				spec, i, f, before := providerUpdateFixture(t)
				i.bootstrapLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
				i.bootstrapProvidersProbe = i.bootstrapLimitsProbe
				original, err := os.ReadFile(filepath.Join(spec.State, "bootstrap.json"))
				if err != nil {
					t.Fatal(err)
				}
				input := changeBootstrapProviderInput(t, spec, audience)
				stop := errors.New("lost provider reply")
				i.fault = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				if _, err = i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "bootstrap-providers"); !errors.Is(err, stop) {
					t.Fatal(boundary, err)
				}
				i.fault = nil
				startsBeforeResume, stopsBeforeResume := f.starts, f.stops
				after, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "bootstrap-providers")
				if err != nil {
					t.Fatal(err)
				}
				if (boundary == "start-returned" || boundary == "complete") && (f.starts != startsBeforeResume || f.stops != stopsBeforeResume) {
					t.Fatal("acknowledged running revision restarted on resume")
				}
				if after.Identity != before.Identity || after.Installation != before.Installation || after.ConfigHash == before.ConfigHash || after.Mode != "verification" {
					t.Fatal("provider update changed ownership")
				}
				starts := f.starts
				for range 2 {
					again, err := i.migrateBootstrapPolicy(ctx, spec, protocol.Hash(before), "bootstrap-providers")
					if err != nil || protocol.Hash(again) != protocol.Hash(after) || f.starts != starts {
						t.Fatal("provider replay", err)
					}
				}
				c, err := input.Resolve()
				if err != nil {
					t.Fatal(err)
				}
				s, err := worker.OpenBootstrapVerification(spec.State, c.Worker, after.Identity, "")
				if err != nil {
					t.Fatal(err)
				}
				s.Close()
				retained, err := os.ReadFile(filepath.Join(spec.State, "bootstrap.json"))
				if err != nil || string(retained) != string(original) {
					t.Fatal("original bootstrap rewritten", err)
				}
				// Only an audience change needs a route-policy database transition.
				_, err = os.Stat(filepath.Join(spec.State, "bootstrap-limits.json"))
				if audience && err != nil || !audience && !os.IsNotExist(err) {
					t.Fatal("unexpected route history", err)
				}
				input.Worker.MaxMessages++
				raw, _ := input.Bytes()
				if err = os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
				if _, err = i.migrateBootstrapLimits(ctx, spec, protocol.Hash(after)); err != nil {
					t.Fatal("limits after providers", err)
				}
			})
		}
	}
}

func TestManagedBootstrapProviderLostStartRequiresExactHealthyRevision(t *testing.T) {
	spec, i, f, before := providerUpdateFixture(t)
	i.bootstrapLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
	i.bootstrapProvidersProbe = i.bootstrapLimitsProbe
	changeBootstrapProviderInput(t, spec, true)
	f.lostStart = true
	if _, err := i.migrateBootstrapPolicy(context.Background(), spec, protocol.Hash(before), "bootstrap-providers"); err == nil {
		t.Fatal("lost start hidden")
	}
	starts, stops := f.starts, f.stops
	f.unhealthy = true
	if _, err := i.resume(context.Background(), spec.Root); err == nil {
		t.Fatal("unverified runtime accepted")
	}
	if f.starts != starts || f.stops != stops {
		t.Fatal("health failure repeated service mutation")
	}
	f.unhealthy = false
	if _, err := i.resume(context.Background(), spec.Root); err != nil {
		t.Fatal(err)
	}
	if f.starts != starts || f.stops != stops {
		t.Fatal("healthy retained revision restarted")
	}
}

func TestManagedBootstrapProvidersRejectsMixedPolicyAndIncapableHelper(t *testing.T) {
	for _, change := range []string{"limits", "routes", "capability", "interactive", "missing-old-credential"} {
		t.Run(change, func(t *testing.T) {
			spec, i, f, before := providerUpdateFixture(t)
			i.bootstrapLimitsProbe = func(context.Context, *safefs.Directory, string, string) error { return nil }
			i.bootstrapProvidersProbe = i.bootstrapLimitsProbe
			input := changeBootstrapProviderInput(t, spec, true)
			switch change {
			case "limits":
				input.Worker.MaxMessages++
			case "routes":
				input.Worker.Routes["added"] = input.Worker.Routes["ops"]
			case "capability":
				i.bootstrapProvidersProbe = func(context.Context, *safefs.Directory, string, string) error { return errors.New("old helper") }
			case "interactive":
				input.Telegram = &provider.TelegramAccount{Account: "123", TokenFile: input.Providers["telegram"].TokenFile}
			case "missing-old-credential":
				d, err := safefs.Open(spec.Root)
				if err != nil {
					t.Fatal(err)
				}
				old, err := managedRuntime(d, before)
				d.Close()
				if err != nil {
					t.Fatal(err)
				}
				for _, p := range old.Providers {
					if err = os.Remove(p.TokenFile); err != nil {
						t.Fatal(err)
					}
				}
			}
			raw, _ := input.Bytes()
			if err := os.WriteFile(spec.RuntimeFile, raw, 0600); err != nil {
				t.Fatal(err)
			}
			starts := f.starts
			if _, err := i.migrateBootstrapPolicy(context.Background(), spec, protocol.Hash(before), "bootstrap-providers"); err == nil {
				t.Fatal("unsupported migration accepted")
			}
			if f.starts != starts {
				t.Fatal("invalid migration restarted service")
			}
		})
	}
}
