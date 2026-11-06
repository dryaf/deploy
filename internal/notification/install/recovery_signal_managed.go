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

	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

// A later account handoff names the removed service and its immutable endpoint.
// Account and source paths come from that publication, never from new overrides.
type ManagedSignalRecoverySpec struct {
	Version          int                         `json:"version"`
	Operation        string                      `json:"operation"`
	GatewayConfig    string                      `json:"gateway_config"`
	GatewaySHA256    string                      `json:"gateway_sha256"`
	GatewayState     string                      `json:"gateway_state"`
	CommitHash       string                      `json:"commit_hash"`
	RelayDestination string                      `json:"relay_destination"`
	Previous         SignalVerificationReference `json:"previous"`
}

func removedSignalRecoveryService(ref SignalVerificationReference) (m SignalServiceManifest, err error) {
	m, err = inspectSignalServiceRetained(ref, true)
	if err != nil {
		return
	}
	d, err := privateManagedDir(ref.ServiceRoot, true)
	if err != nil {
		return m, err
	}
	defer d.Close()
	j, err := readSignalServiceJournal(d, m)
	if err != nil {
		return m, err
	}
	var removal signalServiceBinding
	if err = d.ReadJSON("remove.json", &removal); err != nil {
		return m, err
	}
	if j.Phase != "removed" || removal != m.binding() {
		return m, fmt.Errorf("complete Signal service removal before later recovery")
	}
	return m, nil
}

func (s ManagedSignalRecoverySpec) prepare() (c Runtime, m SignalServiceManifest, req SignalRecoverySpec, err error) {
	if s.Version != 1 || s.Previous.validate() != nil || s.Previous.ServiceRoot == "" || !managedPath(s.GatewayConfig) || !safefs.DigestPattern.MatchString(s.GatewaySHA256) {
		return c, m, req, fmt.Errorf("invalid managed Signal recovery specification")
	}
	m, err = removedSignalRecoveryService(s.Previous)
	if err != nil {
		return
	}
	p := m.Endpoint
	req = SignalRecoverySpec{AccountState: p.Spec.AccountState, GatewayState: s.GatewayState, RelaySource: p.Installation.Destination, RelayDestination: s.RelayDestination, CommitHash: s.CommitHash, Operation: s.Operation, ServiceBinding: m.ManifestHash}
	if err = req.validate(); err != nil {
		return
	}
	if p.Spec.GatewayState != s.GatewayState || p.Spec.CommitHash == s.CommitHash || p.NextGatewaySHA256 != s.GatewaySHA256 {
		return c, m, req, fmt.Errorf("later Signal recovery changed its gateway predecessor")
	}
	raw, err := nativeSignalRead(s.GatewayConfig, safefs.MaxRecordBytes, true)
	if err != nil {
		return c, m, req, err
	}
	if managedDigest(raw) != s.GatewaySHA256 {
		return c, m, req, fmt.Errorf("Signal predecessor configuration changed")
	}
	c, err = decodeRuntime(raw)
	if err != nil {
		return
	}
	if c.Signal == nil || c.Signal.InboxIdentity != p.Installation.Cursor.Handoff.Target.Identity || c.Signal.Account != p.Installation.Cursor.Handoff.Target.Account {
		return c, m, req, fmt.Errorf("Signal predecessor account/inbox changed")
	}
	return
}

type managedSignalRecoveryService struct {
	ref     SignalVerificationReference
	manager signalServiceManager
	probe   func(context.Context, *safefs.Directory, string, string, string) error
}

func (s managedSignalRecoveryService) Inspect(ctx context.Context) (signalaccount.ServiceState, error) {
	m, err := removedSignalRecoveryService(s.ref)
	if err != nil {
		return signalaccount.ServiceState{}, err
	}
	state, err := s.manager.Inspect(ctx, m)
	if err != nil {
		return signalaccount.ServiceState{}, err
	}
	if state.Loaded || state.Active || state.Enabled || state.PID != 0 {
		return signalaccount.ServiceState{}, fmt.Errorf("removed Signal service remains loaded")
	}
	return signalaccount.ServiceState{Binding: m.ManifestHash}, nil
}
func (s managedSignalRecoveryService) Stop(ctx context.Context) error {
	_, err := s.Inspect(ctx)
	return err
}
func (s managedSignalRecoveryService) Start(context.Context) error {
	return fmt.Errorf("account recovery cannot start a Signal service")
}
func (s managedSignalRecoveryService) VerifyRecoverySupport(ctx context.Context) error {
	return s.VerifyNextRecoverySupport(ctx)
}
func (s managedSignalRecoveryService) VerifyNextRecoverySupport(ctx context.Context) error {
	m, err := removedSignalRecoveryService(s.ref)
	if err != nil {
		return err
	}
	d, err := privateManagedDir(m.Spec.Root, true)
	if err != nil {
		return err
	}
	defer d.Close()
	probe := s.probe
	if probe == nil {
		probe = probeManagedCapability
	}
	return probe(ctx, d, "deploy", m.Spec.BinarySHA256, "__signal-next-recovery-probe")
}

func runRecoverySignalManagedCLI(args []string, out io.Writer) error {
	f := flag.NewFlagSet("notification-worker recovery-signal-managed-handoff", flag.ContinueOnError)
	local := f.Bool("local", false, "Recover a removed managed Signal service")
	path := f.String("spec", "", "Private pinned managed Signal recovery specification")
	if err := f.Parse(args); err != nil {
		return err
	}
	if !*local || f.NArg() != 0 || !managedPath(*path) {
		return fmt.Errorf("managed Signal handoff requires --local --spec ABS")
	}
	raw, err := nativeSignalRead(*path, safefs.MaxRecordBytes, true)
	if err != nil {
		return err
	}
	var spec ManagedSignalRecoverySpec
	if err = safefs.DecodeJSON(raw, &spec, safefs.MaxRecordBytes); err != nil {
		return err
	}
	c, previous, req, err := spec.prepare()
	if err != nil {
		return err
	}
	if ui.DryRun {
		_, err = fmt.Fprintln(out, "[DRY] Retained Signal service and configuration validated; no account/inbox opened, service manager contacted or executable probed")
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	svc := managedSignalRecoveryService{ref: spec.Previous, manager: userSignalServiceManager{}}
	installation, status, err := InstallNextSignalRecoveryWithService(ctx, c, req, previous.Endpoint.Spec.Recovery, svc)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(struct {
		Installation relay.SignalRuntimeInstallation `json:"installation"`
		Account      signalaccount.RecoveryStatus    `json:"account"`
	}{installation, status})
}
