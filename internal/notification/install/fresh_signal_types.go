package install

import (
	"fmt"
	"path/filepath"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
)

const freshSignalUnit = "deploy-notification-signal.service"

// FreshSignalSpec enrolls a genuinely linked account, without synthesizing a
// recovery, handoff or command cursor. Policy compilation owns these pins.
type FreshSignalSpec struct {
	Socket          string                    `json:"socket,omitempty"`
	Version         int                       `json:"version"`
	Operation       string                    `json:"operation"`
	Root            string                    `json:"root"`
	UnitDirectory   string                    `json:"unit_directory"`
	BinaryFile      string                    `json:"binary_file"`
	BinarySHA256    string                    `json:"binary_sha256"`
	CLIFile         string                    `json:"cli_file"`
	CLISHA256       string                    `json:"cli_sha256"`
	CLIVersion      string                    `json:"cli_version"`
	AccountState    string                    `json:"account_state"`
	AccountFile     string                    `json:"account_file"`
	AccountSHA256   string                    `json:"account_sha256"`
	RecipientFile   string                    `json:"recipient_file"`
	RecipientSHA256 string                    `json:"recipient_sha256"`
	LinkOperation   string                    `json:"link_operation"`
	LinkHash        string                    `json:"link_hash"`
	Inbox           relay.FreshInboxBootstrap `json:"inbox"`
}

type FreshSignalManifest struct {
	Spec            FreshSignalSpec `json:"spec"`
	RootDevice      uint64          `json:"root_device"`
	RootInode       uint64          `json:"root_inode"`
	OperationDevice uint64          `json:"operation_device"`
	OperationInode  uint64          `json:"operation_inode"`
	RuntimeDevice   uint64          `json:"runtime_device"`
	RuntimeInode    uint64          `json:"runtime_inode"`
	UnitHash        string          `json:"unit_hash"`
	ManifestHash    string          `json:"manifest_hash"`
}

type FreshSignalReference struct {
	Root         string `json:"root"`
	ManifestHash string `json:"manifest_hash"`
}

type FreshSignalHealth struct {
	Version         int               `json:"version"`
	ServiceHash     string            `json:"service_hash"`
	Account         string            `json:"account"`
	Inbox           protocol.Identity `json:"inbox"`
	Destination     string            `json:"destination"`
	CommandsEnabled bool              `json:"commands_enabled"`
}

func (s FreshSignalSpec) validate() error {
	if s.Version != 1 || !safefs.ValidID(s.Operation, "n") || !safefs.ValidID(s.LinkOperation, "n") || s.CLIVersion != "0.14.9" {
		return fmt.Errorf("invalid fresh Signal service specification")
	}
	for _, hash := range []string{s.BinarySHA256, s.CLISHA256, s.AccountSHA256, s.RecipientSHA256, s.LinkHash} {
		if !safefs.DigestPattern.MatchString(hash) {
			return fmt.Errorf("fresh Signal service requires exact input pins")
		}
	}
	roots := []string{s.Root, s.UnitDirectory, s.AccountState, s.Inbox.State}
	for i, p := range roots {
		if !managedPath(p) {
			return fmt.Errorf("fresh Signal service requires absolute roots")
		}
		for _, q := range roots[:i] {
			if managedWithin(p, q) || managedWithin(q, p) {
				return fmt.Errorf("fresh Signal service roots must be separate")
			}
		}
	}
	for _, p := range []string{s.BinaryFile, s.CLIFile, s.AccountFile, s.RecipientFile} {
		if !managedPath(p) {
			return fmt.Errorf("fresh Signal service requires absolute inputs")
		}
		for _, r := range roots {
			if managedWithin(p, r) {
				return fmt.Errorf("fresh Signal inputs must be outside managed roots")
			}
		}
	}
	if s.Inbox.Version != 1 || !safefs.ValidID(s.Inbox.Operation, "n") || !s.Inbox.Identity.Valid() || !protocol.ValidToken(s.Inbox.Account) {
		return fmt.Errorf("fresh Signal service requires a canonical inbox reservation")
	}
	if !managedPath(s.socket()) || filepath.Dir(s.socket()) != s.Root || len(s.socket()) > 100 {
		return fmt.Errorf("fresh Signal service socket path too long")
	}
	return nil
}
func (s FreshSignalSpec) socket() string {
	if s.Socket != "" {
		return s.Socket
	}
	return filepath.Join(s.Root, "api.sock")
}
func (m FreshSignalManifest) executable() string { return filepath.Join(m.Spec.Root, "deploy") }
func (m FreshSignalManifest) commandArgs() []string {
	return []string{m.executable(), "notification-worker", "__fresh-signal-serve", "--local", "--root", m.Spec.Root, "--operation", m.Spec.Operation}
}
func (m FreshSignalManifest) reference() FreshSignalReference {
	return FreshSignalReference{m.Spec.Root, m.ManifestHash}
}
func (m FreshSignalManifest) binding() signalaccount.NativeServiceBinding {
	s := m.Spec
	return signalaccount.NativeServiceBinding{Version: 1, ServiceRoot: s.Root, ServiceHash: m.ManifestHash, Account: s.Inbox.Account, LinkOperation: s.LinkOperation, LinkHash: s.LinkHash, CLISHA256: s.CLISHA256, CLIVersion: s.CLIVersion}
}
func (m FreshSignalManifest) validate() error {
	if err := m.Spec.validate(); err != nil {
		return err
	}
	if m.RootInode == 0 || m.OperationInode == 0 || m.RuntimeInode == 0 {
		return fmt.Errorf("fresh Signal service authority missing")
	}
	body, err := renderFreshSignal(m)
	if err != nil {
		return err
	}
	if managedDigest([]byte(body)) != m.UnitHash {
		return fmt.Errorf("fresh Signal unit changed")
	}
	hash := m.ManifestHash
	m.ManifestHash = ""
	if !safefs.DigestPattern.MatchString(hash) || protocol.Hash(m) != hash {
		return fmt.Errorf("fresh Signal manifest changed")
	}
	return nil
}
func renderFreshSignal(m FreshSignalManifest) (string, error) {
	if err := m.Spec.validate(); err != nil {
		return "", err
	}
	s := m.Spec
	return "[Unit]\nDescription=Managed deploy Signal account\nBefore=" + managedUnit + "\nStartLimitIntervalSec=60\nStartLimitBurst=3\n\n[Service]\nType=exec\nExecStart=" + systemdPath(m.executable()) + " notification-worker __fresh-signal-serve --local --root " + systemdArgument(s.Root) + " --operation " + systemdArgument(s.Operation) + "\nRestart=on-failure\nRestartSec=5\nTimeoutStopSec=35\nKillMode=control-group\nUMask=0077\nNoNewPrivileges=true\nPrivateUsers=true\nPrivateDevices=true\nProtectSystem=strict\nProtectHome=read-only\nReadWritePaths=" + systemdPath(s.Root) + " " + systemdPath(s.AccountState) + " " + systemdPath(s.Inbox.State) + "\nCapabilityBoundingSet=\nAmbientCapabilities=\nRestrictAddressFamilies=AF_UNIX AF_INET AF_INET6\nRestrictSUIDSGID=true\nLockPersonality=true\nMemoryMax=512M\nTasksMax=64\nLimitNOFILE=1024\nCPUQuota=100%\n\n[Install]\nWantedBy=default.target\n", nil
}
func freshSignalPhase(p string) int {
	return map[string]int{"prepared": 1, "staged": 2, "verified": 3, "bound": 4, "unit": 5, "published": 6, "started": 7, "complete": 8, "removing": 9, "removed": 10}[p]
}
