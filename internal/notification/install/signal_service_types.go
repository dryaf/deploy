package install

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

const signalServiceUnit = "deploy-notification-signal-verification.service"
const signalServiceEndpointBinding = "managed-service.json"

type SignalServiceSpec struct {
	Version       int                         `json:"version"`
	Operation     string                      `json:"operation"`
	Root          string                      `json:"root"`
	UnitDirectory string                      `json:"unit_directory"`
	BinaryFile    string                      `json:"binary_file"`
	BinarySHA256  string                      `json:"binary_sha256"`
	Endpoint      SignalVerificationReference `json:"endpoint"`
}
type SignalServiceManifest struct {
	Spec            SignalServiceSpec          `json:"spec"`
	Endpoint        SignalVerificationManifest `json:"endpoint"`
	RootDevice      uint64                     `json:"root_device"`
	RootInode       uint64                     `json:"root_inode"`
	OperationDevice uint64                     `json:"operation_device"`
	OperationInode  uint64                     `json:"operation_inode"`
	RuntimeDevice   uint64                     `json:"runtime_device"`
	RuntimeInode    uint64                     `json:"runtime_inode"`
	UnitHash        string                     `json:"unit_hash"`
	ManifestHash    string                     `json:"manifest_hash"`
}
type signalServiceBinding struct {
	Root         string `json:"root"`
	ManifestHash string `json:"manifest_hash"`
}
type signalServiceJournal struct {
	ManifestHash string `json:"manifest_hash"`
	Phase        string `json:"phase"`
}

func (s SignalServiceSpec) validate() error {
	if s.Version != 1 || !safefs.ValidID(s.Operation, "n") || !safefs.DigestPattern.MatchString(s.BinarySHA256) || s.Endpoint.validate() != nil || s.Endpoint.ServiceRoot != "" {
		return fmt.Errorf("invalid Signal service specification")
	}
	for _, p := range []string{s.Root, s.UnitDirectory, s.BinaryFile} {
		if !managedPath(p) {
			return fmt.Errorf("Signal service requires absolute paths")
		}
	}
	for _, pair := range [][2]string{{s.Root, s.UnitDirectory}, {s.Root, s.Endpoint.Root}, {s.UnitDirectory, s.Endpoint.Root}} {
		if managedWithin(pair[0], pair[1]) || managedWithin(pair[1], pair[0]) {
			return fmt.Errorf("Signal service roots must be separate")
		}
	}
	if managedWithin(s.BinaryFile, s.Root) || managedWithin(s.BinaryFile, s.Endpoint.Root) {
		return fmt.Errorf("stage Signal service executable outside managed roots")
	}
	return nil
}
func (m SignalServiceManifest) binding() signalServiceBinding {
	return signalServiceBinding{m.Spec.Root, m.ManifestHash}
}
func (m SignalServiceManifest) executable() string { return filepath.Join(m.Spec.Root, "deploy") }

func (m SignalServiceManifest) validate() error {
	if err := m.Spec.validate(); err != nil {
		return err
	}
	if err := m.Endpoint.validate(); err != nil {
		return err
	}
	if m.Endpoint.Spec.Root != m.Spec.Endpoint.Root || m.Endpoint.ManifestHash != m.Spec.Endpoint.ManifestHash || m.RootInode == 0 || m.OperationInode == 0 || m.RuntimeInode == 0 {
		return fmt.Errorf("invalid Signal service authority")
	}
	for _, p := range []string{m.Endpoint.Spec.GatewayState, m.Endpoint.Spec.AccountState, m.Endpoint.Installation.Source, m.Endpoint.Installation.Destination} {
		for _, q := range []string{m.Spec.Root, m.Spec.UnitDirectory} {
			if managedWithin(p, q) || managedWithin(q, p) {
				return fmt.Errorf("Signal service overlaps retained state")
			}
		}
	}
	// Unit arguments carry the operation ID, avoiding a circular manifest/unit hash.
	unit, err := renderSignalService(m)
	if err != nil {
		return err
	}
	if managedDigest([]byte(unit)) != m.UnitHash {
		return fmt.Errorf("Signal service unit hash changed")
	}
	pin := m.ManifestHash
	m.ManifestHash = ""
	if !safefs.DigestPattern.MatchString(pin) || protocol.Hash(m) != pin {
		return fmt.Errorf("Signal service manifest changed")
	}
	return nil
}
func renderSignalService(m SignalServiceManifest) (string, error) {
	if err := m.Spec.validate(); err != nil {
		return "", err
	}
	return "[Unit]\nDescription=Verify recovered deploy Signal account\nBefore=" + managedUnit + "\nStartLimitIntervalSec=60\nStartLimitBurst=3\n\n[Service]\nType=exec\nExecStart=" + systemdPath(m.executable()) + " notification-worker __signal-service-serve --local --root " + systemdArgument(m.Spec.Root) + " --operation " + systemdArgument(m.Spec.Operation) + "\nRestart=on-failure\nRestartSec=5\nTimeoutStopSec=35\nKillMode=control-group\nUMask=0077\nNoNewPrivileges=true\nPrivateUsers=true\nPrivateDevices=true\nProtectSystem=strict\nProtectHome=read-only\nReadWritePaths=" + systemdPath(m.Spec.Endpoint.Root) + "\nCapabilityBoundingSet=\nAmbientCapabilities=\nRestrictAddressFamilies=AF_UNIX\nRestrictSUIDSGID=true\nLockPersonality=true\nMemoryMax=256M\nTasksMax=64\nLimitNOFILE=1024\nCPUQuota=100%\n\n[Install]\nWantedBy=default.target\n", nil
}
func (m SignalServiceManifest) commandArgs() []string {
	return []string{m.executable(), "notification-worker", "__signal-service-serve", "--local", "--root", m.Spec.Root, "--operation", m.Spec.Operation}
}
func signalServicePhase(p string) int {
	return map[string]int{"prepared": 1, "bound": 2, "unit": 3, "published": 4, "reloaded": 5, "started": 6, "complete": 7, "removing": 8, "removed": 9}[p]
}
func signalServiceTemp(name string) bool {
	return strings.HasPrefix(name, ".") && safefs.ValidID(strings.TrimSuffix(strings.TrimPrefix(name, "."), ".json"), "n")
}
