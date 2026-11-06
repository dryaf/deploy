package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

const maxSignalCLI = 512 << 20

// SignalServiceRuntime is explicit production admission in an immutable service
// enrollment. Changing it, or returning from uninstall, requires a fresh service
// root and the retained replacement protocol. The original account fence stays.
type SignalServiceRuntime struct {
	CLIFile       string `json:"cli_file"`
	CLISHA256     string `json:"cli_sha256"`
	CLIVersion    string `json:"cli_version"`
	AccountFile   string `json:"account_file"`
	AccountSHA256 string `json:"account_sha256"`
}

func (r SignalServiceRuntime) validate(s SignalServiceSpec) error {
	if r.CLIVersion != "0.14.9" || !safefs.DigestPattern.MatchString(r.CLISHA256) || !safefs.DigestPattern.MatchString(r.AccountSHA256) {
		return fmt.Errorf("pinned native Signal CLI and account selector required")
	}
	for _, path := range []string{r.CLIFile, r.AccountFile} {
		if !managedPath(path) {
			return fmt.Errorf("Signal runtime input path must be absolute")
		}
		for _, root := range []string{s.Root, s.UnitDirectory, s.Endpoint.Root} {
			if managedWithin(path, root) {
				return fmt.Errorf("Signal runtime inputs must be staged separately")
			}
		}
	}
	return nil
}

func signalProductionConfig(m SignalServiceManifest) (relay.Config, error) {
	var config relay.Config
	if m.Spec.Runtime == nil {
		return config, fmt.Errorf("Signal production enrollment required")
	}
	raw, err := m.Endpoint.gateway()
	if err != nil {
		return config, err
	}
	c, err := decodeRuntime(raw)
	if err != nil {
		return config, err
	}
	if c.Signal == nil || c.Worker.Interaction == nil {
		return config, fmt.Errorf("Signal production requires explicit conversations")
	}
	config = relay.Config{StateDir: m.Endpoint.Spec.AccountState, Socket: c.Signal.Socket, AccountFile: filepath.Join(m.Spec.Root, "account"), Interactive: &relay.InteractiveConfig{Version: m.Spec.Runtime.CLIVersion, Account: c.Signal.Account, InboxDir: m.Endpoint.Spec.InboxDestination}}
	for _, destination := range c.Worker.Interaction.Destinations {
		if destination.Provider != "signal" {
			continue
		}
		if destination.Account != c.Signal.Account {
			return config, fmt.Errorf("Signal conversation account changed")
		}
		config.Interactive.Destinations = append(config.Interactive.Destinations, destination)
		for _, permission := range c.Worker.Interaction.Permissions {
			if permission.Conversation == destination.ID && !slices.Contains(config.Interactive.Actors, permission.Actor) {
				config.Interactive.Actors = append(config.Interactive.Actors, permission.Actor)
			}
		}
	}
	return config, config.Interactive.Validate()
}

func stageSignalProduction(d *safefs.Directory, m SignalServiceManifest) error {
	if m.Spec.Runtime == nil {
		return nil
	}
	r := *m.Spec.Runtime
	for _, input := range []struct {
		name, path, hash string
		limit            int64
		mode             uint32
	}{{"signal-cli", r.CLIFile, r.CLISHA256, maxSignalCLI, 0700}, {"account", r.AccountFile, r.AccountSHA256, 4096, 0600}} {
		raw, err := ownedManagedFile(d, input.name, input.limit)
		if os.IsNotExist(err) {
			raw, err = nativeSignalRead(input.path, input.limit, input.mode == 0600)
		}
		if err != nil {
			return err
		}
		if managedDigest(raw) != input.hash {
			return fmt.Errorf("Signal production input changed: %s", input.name)
		}
		if input.name == "signal-cli" {
			if err = checkManagedELF(raw); err != nil {
				return err
			}
		}
		if err = frozenManagedFile(d, input.name, raw, input.mode); err != nil {
			return err
		}
	}
	return nil
}

func checkSignalProductionInputs(m SignalServiceManifest) error {
	if m.Spec.Runtime == nil {
		return nil
	}
	if _, err := signalProductionConfig(m); err != nil {
		return err
	}
	r := m.Spec.Runtime
	raw, err := nativeSignalRead(r.CLIFile, maxSignalCLI, false)
	if err != nil {
		return err
	}
	if managedDigest(raw) != r.CLISHA256 {
		return fmt.Errorf("Signal CLI input pin changed")
	}
	if err = checkManagedELF(raw); err != nil {
		return err
	}
	raw, err = nativeSignalRead(r.AccountFile, 4096, true)
	if err != nil {
		return err
	}
	if managedDigest(raw) != r.AccountSHA256 {
		return fmt.Errorf("Signal selector input pin changed")
	}
	_, err = provider.ReadSecret(r.AccountFile)
	return err
}

func checkSignalProductionFiles(m SignalServiceManifest) error {
	if m.Spec.Runtime == nil {
		return nil
	}
	if _, err := signalProductionConfig(m); err != nil {
		return err
	}
	r := *m.Spec.Runtime
	for _, input := range []struct {
		name, hash string
		limit      int64
		mode       os.FileMode
	}{{"signal-cli", r.CLISHA256, maxSignalCLI, 0700}, {"account", r.AccountSHA256, 4096, 0600}} {
		f, raw, err := nativeSignalFile(filepath.Join(m.Spec.Root, input.name), input.limit, input.mode == 0600)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		f.Close()
		if err != nil {
			return err
		}
		if info.Mode().Perm() != input.mode || managedDigest(raw) != input.hash {
			return fmt.Errorf("Signal production file changed: %s", input.name)
		}
		if input.name == "signal-cli" {
			if err = checkManagedELF(raw); err != nil {
				return err
			}
		}
	}
	return nil
}

func serveSignalProduction(ctx context.Context, d *safefs.Directory, m SignalServiceManifest, serviceCheck func() error) error {
	config, err := signalProductionConfig(m)
	if err != nil {
		return err
	}
	root, err := openSignalVerificationRoot(m.Spec.Endpoint.Root, false)
	if err != nil {
		return err
	}
	defer root.close()
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := serviceCheck(); err != nil {
			return err
		}
		if err := root.audit(m.Endpoint, true); err != nil {
			return err
		}
		return checkSignalServiceBinding(root.dir, m.binding())
	}
	if err = check(ctx); err != nil {
		return err
	}
	account, err := signalaccount.OpenRecoveryRuntime(config.StateDir, m.Endpoint.Spec.Recovery, func() error { return check(ctx) })
	if err != nil {
		return err
	}
	defer account.Close()
	inbox, err := relay.OpenManagedSignalInbox(ctx, config.Interactive.InboxDir, m.Endpoint.Installation.InstallationHash, m.Endpoint.receipt(), check)
	if err != nil {
		return err
	}
	defer inbox.Close()
	helper, err := d.Open("deploy", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer helper.Close()
	cli, err := d.Open("signal-cli", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer cli.Close()
	// The callback checks both the retained FD and its path, so a replacement
	// cannot execute the old descriptor under a newly accepted file identity.
	execution := relay.ManagedExecution{Account: account, Inbox: inbox, Check: check, Command: func(ctx context.Context, lease *os.File, output io.Writer, args ...string) error {
		if err := check(ctx); err != nil {
			return err
		}
		for _, pair := range []struct {
			file *os.File
			name string
		}{{helper, "deploy"}, {cli, "signal-cli"}} {
			held, err := pair.file.Stat()
			if err != nil {
				return err
			}
			current, err := os.Stat(filepath.Join(m.Spec.Root, pair.name))
			if err != nil {
				return err
			}
			if !os.SameFile(held, current) {
				return fmt.Errorf("Signal execution descriptor replaced")
			}
		}
		return relay.RunPinnedCommand(ctx, helper, cli, lease, relay.CommandRequest{Version: 1, CLIHash: m.Spec.Runtime.CLISHA256, State: config.StateDir, Args: args}, output)
	}, Health: func(ctx context.Context) (any, error) {
		if err := check(ctx); err != nil {
			return nil, err
		}
		return signalServiceHealth(m)
	}}
	return relay.RunManaged(ctx, config, execution)
}

func signalServiceHealth(m SignalServiceManifest) (SignalVerificationHealth, error) {
	raw, err := m.Endpoint.gateway()
	if err != nil {
		return SignalVerificationHealth{}, err
	}
	c, err := decodeRuntime(raw)
	if err != nil {
		return SignalVerificationHealth{}, err
	}
	return SignalVerificationHealth{Version: 1, ServiceHash: m.ManifestHash, VerificationOnly: m.Spec.Runtime == nil, IntakeEnabled: m.Spec.Runtime != nil, ManifestHash: m.Endpoint.ManifestHash, InstallationHash: m.Endpoint.Installation.InstallationHash, RecoveryOperation: m.Endpoint.Spec.Recovery.Operation, CommitHash: m.Endpoint.Spec.CommitHash, Signal: *c.Signal}, nil
}
