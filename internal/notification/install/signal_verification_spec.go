package install

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

// SignalVerificationSpec enrolls a separate, verification-only socket after the
// account handoff. It neither replaces the old service nor releases its fence.
type SignalVerificationSpec struct {
	Version          int                          `json:"version"`
	Operation        string                       `json:"operation"`
	Root             string                       `json:"root"`
	GatewayConfig    string                       `json:"gateway_config"`
	GatewaySHA256    string                       `json:"gateway_sha256"`
	GatewayState     string                       `json:"gateway_state"`
	CommitHash       string                       `json:"commit_hash"`
	AccountState     string                       `json:"account_state"`
	InboxDestination string                       `json:"inbox_destination"`
	Recovery         signalaccount.RecoveryStatus `json:"recovery"`
}

type SignalVerificationManifest struct {
	Spec        SignalVerificationSpec `json:"spec"`
	RootDevice  uint64                 `json:"root_device"`
	RootInode   uint64                 `json:"root_inode"`
	LeaseDevice uint64                 `json:"lease_device"`
	LeaseInode  uint64                 `json:"lease_inode"`
	// Bytes, not a re-marshaled resolved Runtime: revisions must resolve once.
	OriginalConfig    []byte                          `json:"original_config"`
	Installation      relay.SignalRuntimeInstallation `json:"installation"`
	NextGatewaySHA256 string                          `json:"next_gateway_sha256"`
	ManifestHash      string                          `json:"manifest_hash"`
}

func (s SignalVerificationSpec) validate() error {
	if s.Version != 1 || !safefs.ValidID(s.Operation, "n") || !safefs.DigestPattern.MatchString(s.GatewaySHA256) || !safefs.DigestPattern.MatchString(s.CommitHash) || s.Recovery.Phase != "complete" || s.Recovery.RuntimeAllowed || !safefs.ValidID(s.Recovery.Operation, "n") {
		return fmt.Errorf("completed, pinned Signal recovery required for verification endpoint")
	}
	for _, h := range []string{s.Recovery.Binding, s.Recovery.Target, s.Recovery.ResultHash} {
		if !safefs.DigestPattern.MatchString(h) {
			return fmt.Errorf("invalid Signal recovery pin")
		}
	}
	paths := []string{s.Root, s.GatewayState, s.AccountState, s.InboxDestination}
	for i, p := range paths {
		if !managedPath(p) {
			return fmt.Errorf("verification requires absolute paths")
		}
		for _, q := range paths[:i] {
			if managedWithin(p, q) || managedWithin(q, p) {
				return fmt.Errorf("verification state roots must be separate")
			}
		}
		if managedWithin(s.GatewayConfig, p) {
			return fmt.Errorf("original gateway configuration must be outside verification state")
		}
	}
	if !managedPath(s.GatewayConfig) || len(filepath.Join(s.Root, "api.sock")) >= 108 {
		return fmt.Errorf("invalid Signal verification input/socket path")
	}
	return nil
}
func (s SignalVerificationSpec) prepare() (Runtime, []byte, error) {
	if err := s.validate(); err != nil {
		return Runtime{}, nil, err
	}
	raw, err := nativeSignalRead(s.GatewayConfig, safefs.MaxRecordBytes, true)
	if err != nil || managedDigest(raw) != s.GatewaySHA256 {
		return Runtime{}, nil, fmt.Errorf("original gateway configuration missing, unsafe or changed")
	}
	c, err := decodeRuntime(raw)
	if err != nil {
		return c, nil, err
	}
	if c.Signal == nil || managedWithin(c.Signal.Socket, s.Root) || managedWithin(s.Root, filepath.Dir(c.Signal.Socket)) {
		return c, nil, fmt.Errorf("verification endpoint requires a separate Signal socket directory")
	}
	return c, raw, nil
}

func (m SignalVerificationManifest) receipt() relay.SignalVerificationEndpoint {
	return relay.SignalVerificationEndpoint{Version: 1, ManifestHash: m.ManifestHash, Root: m.Spec.Root, Device: m.RootDevice, Inode: m.RootInode}
}

func signalRecoveryTarget(c Runtime, s SignalRecoverySpec, cursor interaction.SignalRecoveryCursor) string {
	return protocol.Hash(struct {
		Spec    SignalRecoverySpec
		Runtime Runtime
		Cursor  interaction.SignalRecoveryCursor
	}{s, c, cursor})
}

func (m SignalVerificationManifest) gateway() ([]byte, error) {
	if err := m.Spec.validate(); err != nil {
		return nil, err
	}
	if managedDigest(m.OriginalConfig) != m.Spec.GatewaySHA256 {
		return nil, fmt.Errorf("verification original configuration pin changed")
	}
	c, err := decodeRuntime(m.OriginalConfig)
	if err != nil {
		return nil, err
	}
	p := m.Installation
	if c.Signal == nil || p.IntakeEnabled || p.InstallationHash != m.Spec.Recovery.ResultHash || p.Destination != m.Spec.InboxDestination || c.Signal.InboxIdentity != p.Cursor.Handoff.Before.Identity || c.Signal.Account != p.Cursor.Handoff.Target.Account {
		return nil, fmt.Errorf("verification gateway/account/inbox mismatch")
	}
	if managedWithin(c.Signal.Socket, m.Spec.Root) || managedWithin(m.Spec.Root, filepath.Dir(c.Signal.Socket)) {
		return nil, fmt.Errorf("verification socket must be separate")
	}
	spec := SignalRecoverySpec{AccountState: m.Spec.AccountState, GatewayState: m.Spec.GatewayState, RelaySource: p.Source, RelayDestination: p.Destination, CommitHash: m.Spec.CommitHash, Operation: m.Spec.Recovery.Operation, ServiceBinding: m.Spec.Recovery.Binding}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	for _, path := range []string{m.Spec.Root, m.Spec.GatewayConfig} {
		if managedWithin(path, p.Source) || managedWithin(p.Source, path) {
			return nil, fmt.Errorf("verification inputs overlap source inbox")
		}
	}
	if signalRecoveryTarget(c, spec, p.Cursor) != m.Spec.Recovery.Target {
		return nil, fmt.Errorf("verification differs from the completed account recovery target")
	}
	var next Runtime
	if err = safefs.DecodeJSON(m.OriginalConfig, &next, safefs.MaxRecordBytes); err != nil {
		return nil, err
	}
	next.Signal.Socket = filepath.Join(m.Spec.Root, "api.sock")
	next.Signal.InboxIdentity = p.Cursor.Handoff.Target.Identity
	raw, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	// Validate the staged public configuration using the ordinary one-time resolver.
	if _, err = decodeRuntime(raw); err != nil {
		return nil, err
	}
	return raw, nil
}
func (m SignalVerificationManifest) validate() error {
	pin := m.ManifestHash
	m.ManifestHash = ""
	if !safefs.DigestPattern.MatchString(pin) || protocol.Hash(m) != pin || m.RootInode == 0 || m.LeaseInode == 0 {
		return fmt.Errorf("invalid Signal verification manifest")
	}
	raw, err := m.gateway()
	if err != nil {
		return err
	}
	if managedDigest(raw) != m.NextGatewaySHA256 {
		return fmt.Errorf("staged gateway configuration pin changed")
	}
	return nil
}
