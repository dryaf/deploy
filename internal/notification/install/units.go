package install

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

type ServicePaths struct {
	Executable  string   `json:"executable"`
	Config      string   `json:"config"`
	State       string   `json:"state"`
	Socket      string   `json:"socket"`
	DeniedPaths []string `json:"denied_paths,omitempty"` // Source keys, archives and credentials excluded by the installer inventory.
}

func systemdArgument(value string) string {
	return strconv.Quote(strings.NewReplacer("%", "%%", "$", "$$").Replace(value))
}

func systemdPath(value string) string { return strconv.Quote(strings.ReplaceAll(value, "%", "%%")) }

// RenderVerificationService is side-effect free. The future installation
// coordinator must own/pin these files and complete qualification before replacing
// this verification unit. Rendering is not permission to activate a producer.
func RenderVerificationService(p ServicePaths) (string, error) {
	for _, path := range []string{p.Executable, p.Config, p.State, p.Socket} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") {
			return "", fmt.Errorf("notification service paths must be clean absolute paths")
		}
	}
	within := func(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }
	if p.Socket == p.State || !within(p.Socket, p.State) || within(p.Executable, p.State) || within(p.Config, p.State) || p.Config == p.Executable {
		return "", fmt.Errorf("notification socket must be inside state; executable and config must remain outside writable state")
	}
	var denied strings.Builder
	for _, path := range p.DeniedPaths {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") {
			return "", fmt.Errorf("invalid notification service exclusion")
		}
		for _, required := range []string{p.Executable, p.Config, p.State} {
			if within(required, path) || within(path, p.State) {
				return "", fmt.Errorf("notification service exclusion overlaps runtime paths")
			}
		}
		denied.WriteString("InaccessiblePaths=" + systemdPath("-"+path) + "\n")
	}
	return "[Unit]\nDescription=Verify staged deploy notification worker\nStartLimitIntervalSec=60\nStartLimitBurst=3\n\n[Service]\nType=simple\nExecStart=" + systemdPath(p.Executable) + " notification-worker serve --local --verification --config " + systemdArgument(p.Config) + " --state-dir " + systemdArgument(p.State) + " --socket " + systemdArgument(p.Socket) + "\nRestart=on-failure\nRestartSec=5\nTimeoutStopSec=35\nKillMode=control-group\nUMask=0077\nMemoryMax=256M\nTasksMax=64\nCPUQuota=100%\nLimitNOFILE=1024\nNoNewPrivileges=true\nPrivateUsers=true\nCapabilityBoundingSet=\nAmbientCapabilities=\nPrivateDevices=true\nProtectSystem=strict\nProtectHome=read-only\nReadWritePaths=" + systemdPath(p.State) + "\nInaccessiblePaths=-%t/podman -%t/docker.sock\n" + denied.String() + "RestrictAddressFamilies=AF_UNIX\nRestrictSUIDSGID=true\nLockPersonality=true\n\n[Install]\nWantedBy=default.target\n", nil
}
