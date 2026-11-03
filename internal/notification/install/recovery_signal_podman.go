package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type PodmanSignalRecoverySpec struct {
	Version          int                     `json:"version"`
	Operation        string                  `json:"operation"`
	GatewayConfig    string                  `json:"gateway_config"`
	GatewaySHA256    string                  `json:"gateway_sha256"`
	GatewayState     string                  `json:"gateway_state"`
	CommitHash       string                  `json:"commit_hash"`
	RelayDestination string                  `json:"relay_destination"`
	Service          PodmanSignalServiceSpec `json:"service"`
}

func (s PodmanSignalRecoverySpec) request(binding string) SignalRecoverySpec {
	return SignalRecoverySpec{Operation: s.Operation, GatewayState: s.GatewayState, CommitHash: s.CommitHash, AccountState: s.Service.AccountState, RelaySource: s.Service.Inbox, RelayDestination: s.RelayDestination, ServiceBinding: binding}
}
func (s PodmanSignalRecoverySpec) prepare() (Runtime, error) {
	var zero Runtime
	if s.Version != 1 || !managedPath(s.GatewayConfig) || !safefs.DigestPattern.MatchString(s.GatewaySHA256) {
		return zero, fmt.Errorf("invalid Signal Podman recovery request")
	}
	if err := s.request(protocol.Hash(s.Service)).validate(); err != nil {
		return zero, err
	}
	rc, _, err := s.Service.files()
	if err != nil {
		return zero, err
	}
	for _, p := range []string{s.GatewayConfig, s.Service.Quadlet, s.Service.Fragment, s.Service.RelayConfig, s.Service.AccountFile, s.Service.RecipientFile, s.Service.SocketDirectory} {
		for _, root := range []string{s.GatewayState, s.RelayDestination, s.Service.AccountState, s.Service.Inbox} {
			if managedWithin(p, root) || managedWithin(root, p) {
				return zero, fmt.Errorf("Signal Podman recovery inputs and state must be separate")
			}
		}
	}
	b, err := nativeSignalRead(s.GatewayConfig, safefs.MaxRecordBytes, true)
	if err != nil || managedDigest(b) != s.GatewaySHA256 {
		return zero, fmt.Errorf("Signal gateway configuration changed")
	}
	c, err := decodeRuntime(b)
	if err != nil {
		return zero, err
	}
	if c.Signal == nil || c.Signal.Account != rc.Interactive.Account || c.Signal.Socket != filepath.Join(s.Service.SocketDirectory, "signal.sock") {
		return zero, fmt.Errorf("Signal gateway differs from enrolled container account/socket")
	}
	return c, nil
}

func runRecoverySignalPodmanCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-signal-podman-handoff", flag.ContinueOnError)
	local := f.Bool("local", false, "Fence a local rootless Signal Quadlet and install its offline recovery inbox")
	path := f.String("spec", "", "Private pinned Signal Podman recovery JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*path) {
		return fmt.Errorf("Signal Podman handoff requires --local --spec ABS")
	}
	b, err := nativeSignalRead(*path, safefs.MaxRecordBytes, true)
	if err != nil {
		return err
	}
	var spec PodmanSignalRecoverySpec
	if err = safefs.DecodeJSON(b, &spec, safefs.MaxRecordBytes); err != nil {
		return err
	}
	c, err := spec.prepare()
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] Signal Podman handoff inputs validated; no manager/Podman command, probe or recovery state opened")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc := newPodmanSignalRecoveryService(spec.Service)
	state, err := svc.Inspect(ctx)
	if err != nil {
		return err
	}
	svc.binding = state.Binding
	installation, status, err := InstallSignalRecoveryWithService(ctx, c, spec.request(state.Binding), svc)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Installation relay.SignalRuntimeInstallation `json:"installation"`
		Account      signalaccount.RecoveryStatus    `json:"account"`
	}{installation, status})
}
