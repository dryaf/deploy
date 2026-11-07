package workerctl

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
)

func TestSignalPathsDefaultsAndSeparateAuthority(t *testing.T) {
	facts := TargetFacts{OS: "linux", Arch: "arm64", UID: 1000, User: "deploy", Home: "/home/deploy"}
	paths, err := ResolvePaths("notification-worker", config.WorkerPaths{}, facts)
	if err != nil {
		t.Fatal(err)
	}
	p := config.ProviderDeclaration{AccountInput: "/local/account", RecipientInput: "/local/recipient"}
	hash := strings.Repeat("a", 64)
	r, err := ResolveSignalPaths(p, paths, facts.Home, hash)
	if err != nil {
		t.Fatal(err)
	}
	if r.Account != "/home/deploy/.local/state/deploy/notification-worker/signal-account" || r.Socket != filepath.Join(paths.Data, "signal/service/api.sock") || r.AccountFile != filepath.Join(paths.Credentials, "notification-worker/signal-account") {
		t.Fatal("unexpected home defaults", r)
	}
	p.Data = "~/private/signal-account"
	p.Socket = "~/private/signal-service/custom.sock"
	p.Executable = "~/bin/signal-cli"
	p.ExecutableSHA256 = hash
	r, err = ResolveSignalPaths(p, paths, facts.Home, hash)
	if err != nil {
		t.Fatal(err)
	}
	if r.Account != "/home/deploy/private/signal-account" || r.Service != "/home/deploy/private/signal-service" || r.Executable != "/home/deploy/bin/signal-cli" {
		t.Fatal("overrides not expanded on target", r)
	}
}
func TestSignalPathsRejectOverlappingRootsAndInputs(t *testing.T) {
	facts := TargetFacts{OS: "linux", Arch: "arm64", UID: 1000, User: "deploy", Home: "/home/deploy"}
	paths, err := ResolvePaths("notification-worker", config.WorkerPaths{}, facts)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	for name, mutate := range map[string]func(*config.ProviderDeclaration){
		"gateway-state":        func(p *config.ProviderDeclaration) { p.Data = paths.State },
		"gateway-installation": func(p *config.ProviderDeclaration) { p.Socket = filepath.Join(paths.Installation, "api.sock") },
		"socket-parent":        func(p *config.ProviderDeclaration) { p.Socket = filepath.Join(paths.Control, "api.sock") },
		"account-credential": func(p *config.ProviderDeclaration) {
			p.Data = "~/private/account"
			p.AccountInput = ""
			p.AccountFile = "~/private/account/selector"
		},
		"credential-alias": func(p *config.ProviderDeclaration) {
			p.AccountInput = ""
			p.RecipientInput = ""
			p.AccountFile = "~/private/selector"
			p.RecipientFile = p.AccountFile
		},
		"executable-pin": func(p *config.ProviderDeclaration) {
			p.Executable = "~/bin/signal-cli"
			p.ExecutableSHA256 = strings.Repeat("b", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := config.ProviderDeclaration{AccountInput: "/local/account", RecipientInput: "/local/recipient"}
			mutate(&p)
			if _, err := ResolveSignalPaths(p, paths, facts.Home, hash); err == nil {
				t.Fatal("unsafe Signal paths accepted")
			}
		})
	}
}
