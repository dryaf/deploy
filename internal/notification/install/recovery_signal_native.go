package install

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

type NativeSignalRecoverySpec struct {
	Version          int                     `json:"version"`
	Operation        string                  `json:"operation"`
	GatewayConfig    string                  `json:"gateway_config"`
	GatewaySHA256    string                  `json:"gateway_sha256"`
	GatewayState     string                  `json:"gateway_state"`
	CommitHash       string                  `json:"commit_hash"`
	AccountState     string                  `json:"account_state"`
	RelaySource      string                  `json:"relay_source"`
	RelayDestination string                  `json:"relay_destination"`
	Service          NativeSignalServiceSpec `json:"service"`
}

func (s NativeSignalRecoverySpec) request(binding string) SignalRecoverySpec {
	return SignalRecoverySpec{Operation: s.Operation, GatewayState: s.GatewayState, CommitHash: s.CommitHash, AccountState: s.AccountState, RelaySource: s.RelaySource, RelayDestination: s.RelayDestination, ServiceBinding: binding}
}

// Prepare only reads pinned local inputs. In particular, dry-run never opens
// account/inbox/database state, probes a binary or contacts a service manager.
func (s NativeSignalRecoverySpec) prepare() (Runtime, error) {
	var zero Runtime
	if s.Version != 1 || !managedPath(s.GatewayConfig) || !safefs.DigestPattern.MatchString(s.GatewaySHA256) {
		return zero, fmt.Errorf("invalid native Signal recovery specification")
	}
	if err := s.request(protocol.Hash(s.Service)).validate(); err != nil {
		return zero, err
	}
	rc, _, err := s.Service.files()
	if err != nil {
		return zero, err
	}
	if rc.StateDir != s.AccountState || rc.Interactive.InboxDir != s.RelaySource {
		return zero, fmt.Errorf("native Signal relay account/inbox differs from recovery request")
	}
	for _, p := range []string{s.GatewayConfig, s.Service.UnitFile, s.Service.Executable, s.Service.RelayConfig, rc.AccountFile, rc.RecipientFile, rc.Socket} {
		for _, root := range []string{s.AccountState, s.GatewayState, s.RelaySource, s.RelayDestination} {
			if managedWithin(p, root) {
				return zero, fmt.Errorf("native Signal recovery inputs/socket must be outside state directories")
			}
		}
	}
	b, err := nativeSignalRead(s.GatewayConfig, safefs.MaxRecordBytes, true)
	if err != nil || managedDigest(b) != s.GatewaySHA256 {
		return zero, fmt.Errorf("native Signal gateway configuration missing, unsafe or changed")
	}
	c, err := decodeRuntime(b)
	if err != nil {
		return zero, err
	}
	if c.Signal == nil || c.Signal.Account != rc.Interactive.Account || c.Signal.Socket != rc.Socket {
		return zero, fmt.Errorf("native Signal gateway and relay account/socket differ")
	}
	return c, nil
}

func runRecoverySignalNativeCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-signal-handoff", flag.ContinueOnError)
	local := f.Bool("local", false, "Fence an existing native local Signal relay and install its offline recovery inbox")
	path := f.String("spec", "", "Private, pinned native Signal recovery JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*path) {
		return fmt.Errorf("Signal handoff requires --local --spec ABS")
	}
	b, err := nativeSignalRead(*path, safefs.MaxRecordBytes, true)
	if err != nil {
		return err
	}
	var spec NativeSignalRecoverySpec
	if err = safefs.DecodeJSON(b, &spec, safefs.MaxRecordBytes); err != nil {
		return err
	}
	c, err := spec.prepare()
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] Native Signal handoff inputs validated; no service inspected/stopped, binary probed or recovery state opened")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc := newNativeSignalRecoveryService(spec.Service)
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
