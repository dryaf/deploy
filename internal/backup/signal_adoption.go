package backup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

type backupSignalAdoptionSpec struct {
	Version        int    `json:"version"`
	Operation      string `json:"operation"`
	Home           string `json:"home"`
	ConfigHash     string `json:"config_hash"`
	ServiceBinding string `json:"service_binding"`
	Account        string `json:"account"`
	Inbox          string `json:"inbox"`
}
type backupSignalAdoption struct {
	Spec     backupSignalAdoptionSpec     `json:"spec"`
	Inbox    relay.LegacyInboxBootstrap   `json:"inbox"`
	Recovery signalaccount.RecoveryStatus `json:"recovery"`
}

func (s backupSignalAdoptionSpec) accountState() string {
	return filepath.Join(s.Home, ".local/share/deploy/backup-worker/signal")
}
func (s backupSignalAdoptionSpec) validate() error {
	if s.Version != 1 || !safefs.ValidID(s.Operation, "n") || !backupRecoveryPath(s.Home) || !backupRecoveryPath(s.Inbox) || !provider.ValidSignalAccount(s.Account) || !safefs.DigestPattern.MatchString(s.ConfigHash) || !safefs.DigestPattern.MatchString(s.ServiceBinding) {
		return fmt.Errorf("invalid legacy Signal adoption pins")
	}
	for _, root := range []string{s.accountState(), filepath.Join(s.Home, ".local/state/deploy/backup-worker"), filepath.Join(s.Home, ".config/deploy/backup-worker")} {
		if s.Inbox == root || strings.HasPrefix(s.Inbox, root+"/") || strings.HasPrefix(root, s.Inbox+"/") {
			return fmt.Errorf("legacy Signal inbox overlaps installed backup state")
		}
	}
	return nil
}
func (a backupSignalAdoption) validate() error {
	if err := a.Spec.validate(); err != nil {
		return err
	}
	if a.Inbox.Operation != a.Spec.Operation || a.Inbox.Account != a.Spec.Account || a.Inbox.HistoryComplete || a.Recovery.Operation != a.Spec.Operation || a.Recovery.Binding != a.Spec.ServiceBinding || a.Recovery.Target != notify.Hash(a.Spec) || a.Recovery.ResultHash != notify.Hash(a.Inbox) || a.Recovery.Phase != "complete" || a.Recovery.RuntimeAllowed {
		return fmt.Errorf("legacy Signal adoption result changed")
	}
	return nil
}

// No interactive account or inbox is assumed. The existing backup service is
// permanently fenced through the common account lease before creating an empty
// command-history source. Provider history completeness is explicitly false.
func adoptBackupSignal(ctx context.Context, s backupSignalAdoptionSpec, svc signalaccount.RecoveryService) (a backupSignalAdoption, err error) {
	if err = s.validate(); err != nil {
		return
	}
	a.Spec = s
	apply := func(ctx context.Context, check func() error) (string, error) {
		if err := check(); err != nil {
			return "", err
		}
		v, err := relay.BootstrapLegacyInbox(ctx, s.Inbox, s.Account, s.Operation)
		if err != nil {
			return "", err
		}
		if err = check(); err != nil {
			return "", err
		}
		return notify.Hash(v), nil
	}
	verify := func(ctx context.Context, hash string, check func() error) error {
		if err := check(); err != nil {
			return err
		}
		v, err := relay.ReadLegacyInboxBootstrap(ctx, s.Inbox, hash)
		if err != nil {
			return err
		}
		if v.Account != s.Account || v.Operation != s.Operation {
			return fmt.Errorf("legacy bootstrap target changed")
		}
		if err = check(); err != nil {
			return err
		}
		a.Inbox = v
		return nil
	}
	a.Recovery, err = signalaccount.FenceRecovery(ctx, s.accountState(), signalaccount.RecoveryRequest{Operation: s.Operation, Binding: s.ServiceBinding, Target: notify.Hash(s)}, svc, apply, verify)
	if err != nil {
		return
	}
	err = a.validate()
	if err != nil {
		return
	}
	d, err := safefs.Open(s.Inbox)
	if err != nil {
		return a, err
	}
	defer d.Close()
	var old backupSignalAdoption
	err = d.ReadJSON("legacy-account-adoption.json", &old)
	if err == nil {
		if notify.Hash(old) != notify.Hash(a) {
			return a, fmt.Errorf("legacy account adoption changed")
		}
		return a, nil
	}
	if !os.IsNotExist(err) {
		return a, err
	}
	err = d.WriteJSON("legacy-account-adoption.json", a, false)
	return
}

func prepareBackupSignalAdoption(s backupSignalAdoptionSpec) (*signalService, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	configuration := filepath.Join(s.Home, ".config/deploy/backup-worker/runtime.json")
	c, err := readBackupRecoveryInstallation(configuration)
	if err != nil {
		return nil, err
	}
	if notify.Hash(c) != s.ConfigHash || c.Policy.Notifications.Signal == nil {
		return nil, fmt.Errorf("legacy Signal installation changed")
	}
	raw, err := signalFile(filepath.Join(s.Home, ".config/deploy/backup-worker/images.json"), 8192)
	if err != nil {
		return nil, err
	}
	var images map[string]string
	if err = safefs.DecodeJSON(raw, &images, 8192); err != nil {
		return nil, err
	}
	if err = config.PinnedWorkerImage(images["relay"]); err != nil {
		return nil, err
	}
	paths := backupCutoverPaths(s.Home, c)
	paths.RelayState = s.accountState()
	paths.RelaySocket = filepath.Join(s.Home, ".local/state/deploy/backup-worker/relay")
	units, err := renderWorkerUnits(c, paths, images["worker"], images["relay"])
	if err != nil {
		return nil, err
	}
	return &signalService{home: s.Home, configuration: configuration, image: images["relay"], unitText: units["deploy-backup-signal.container"], imagesHash: notify.Hash(raw), installation: c, run: signalSystemctl, pinned: s.ServiceBinding}, nil
}

type backupSignalHandoffSpec struct {
	Version          int                  `json:"version"`
	Operation        string               `json:"operation"`
	Adoption         backupSignalAdoption `json:"adoption"`
	GatewayConfig    string               `json:"gateway_config"`
	GatewaySHA256    string               `json:"gateway_sha256"`
	GatewayState     string               `json:"gateway_state"`
	CommitHash       string               `json:"commit_hash"`
	RelayDestination string               `json:"relay_destination"`
}

func (s backupSignalHandoffSpec) prepare() (install.Runtime, error) {
	var zero install.Runtime
	if s.Version != 1 || s.Adoption.validate() != nil || !safefs.ValidID(s.Operation, "n") || s.Operation == s.Adoption.Spec.Operation || !safefs.DigestPattern.MatchString(s.GatewaySHA256) || !safefs.DigestPattern.MatchString(s.CommitHash) {
		return zero, fmt.Errorf("invalid legacy Signal handoff pins")
	}
	for _, path := range []string{s.GatewayConfig, s.GatewayState, s.RelayDestination} {
		if !backupRecoveryPath(path) {
			return zero, fmt.Errorf("legacy Signal handoff requires absolute paths")
		}
	}
	runtime, err := install.ReadPinnedRuntime(s.GatewayConfig, s.GatewaySHA256)
	if err != nil {
		return zero, err
	}
	if runtime.Signal == nil || runtime.Signal.Account != s.Adoption.Spec.Account || runtime.Signal.InboxIdentity != s.Adoption.Inbox.Identity {
		return zero, fmt.Errorf("gateway differs from bootstrapped Signal inbox")
	}
	return runtime, nil
}
func handoffBackupSignal(ctx context.Context, s backupSignalHandoffSpec, c install.Runtime, svc signalaccount.NextRecoveryService) (relay.SignalRuntimeInstallation, signalaccount.RecoveryStatus, error) {
	var zero relay.SignalRuntimeInstallation
	if s.Adoption.validate() != nil {
		return zero, signalaccount.RecoveryStatus{}, fmt.Errorf("invalid legacy adoption")
	}
	a := s.Adoption
	d, err := safefs.Open(a.Spec.Inbox)
	if err != nil {
		return zero, signalaccount.RecoveryStatus{}, err
	}
	var retained backupSignalAdoption
	err = d.ReadJSON("legacy-account-adoption.json", &retained)
	d.Close()
	if err != nil {
		return zero, signalaccount.RecoveryStatus{}, err
	}
	if notify.Hash(retained) != notify.Hash(a) {
		return zero, signalaccount.RecoveryStatus{}, fmt.Errorf("legacy adoption receipt changed")
	}
	if _, err = relay.ReadLegacyInboxBootstrap(ctx, a.Spec.Inbox, a.Recovery.ResultHash); err != nil {
		return zero, signalaccount.RecoveryStatus{}, err
	}
	request := install.SignalRecoverySpec{AccountState: a.Spec.accountState(), GatewayState: s.GatewayState, RelaySource: a.Spec.Inbox, RelayDestination: s.RelayDestination, CommitHash: s.CommitHash, Operation: s.Operation, ServiceBinding: a.Spec.ServiceBinding}
	return install.InstallNextSignalRecoveryWithService(ctx, c, request, a.Recovery, svc)
}
