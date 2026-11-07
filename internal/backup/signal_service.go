package backup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/backup/notification"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

const backupSignalUnit = "deploy-backup-signal.service"

func selectSignalOperation(status signalaccount.MaintenanceStatus, explicit string, fresh bool) (string, error) {
	if explicit != "" && (!safefs.ValidID(explicit, "n") || fresh) {
		return "", fmt.Errorf("use a valid Signal operation ID or --new-link, never both")
	}
	unfinished := status.Operation != "" && (status.Phase != "complete" || status.Pending)
	if unfinished && (fresh || explicit != "" && explicit != status.Operation) {
		return "", fmt.Errorf("resume the unfinished Signal service operation before a new link")
	}
	if explicit != "" {
		return explicit, nil
	}
	if status.Operation != "" && !fresh {
		return status.Operation, nil
	}
	return safefs.NewID("n")
}

// Serialize maintenance with setup/update/recovery before any worker state is
// opened. The installation owns this persistent lock; linking never creates it.
func signalInstallationLease(home string) (*safefs.Lock, error) {
	d, err := safefs.Open(home)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Lock(".local/state/deploy/locks/backup-worker-setup.lock", true, false)
}

func requireNoSignalMaintenance(home string) error {
	layout, err := lookupInstalledBackupPaths(home)
	if err != nil {
		return err
	}
	path := layout.RelayState()
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	view, err := signalaccount.InspectMaintenance(path)
	if err != nil {
		return err
	}
	if !view.RuntimeAllowed || view.Pending || (view.Phase != "none" && view.Phase != "complete") {
		return fmt.Errorf("resume Signal account/service maintenance before changing the worker installation")
	}
	return nil
}

type signalService struct {
	home, configuration, image, unitText, imagesHash, pinned string
	installation                                             WorkerInstallation
	run                                                      func(context.Context, ...string) ([]byte, error)
	adoptionProbe                                            func(context.Context, string) error
}

func signalSystemctl(ctx context.Context, args ...string) ([]byte, error) {
	return process.LimitedOutput(exec.CommandContext(ctx, "systemctl", append([]string{"--user", "--no-pager"}, args...)...), 45*time.Second, 16384)
}

func signalFile(path string, limit int64) ([]byte, error) {
	d, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return safefs.ReadFile(d, filepath.Base(path), limit)
}

func (s *signalService) Inspect(ctx context.Context) (signalaccount.ServiceState, error) {
	var zero signalaccount.ServiceState
	c, err := loadWorkerInstallation(s.configuration)
	if err != nil {
		return zero, err
	}
	if protocol.Hash(c) != protocol.Hash(s.installation) || c.Policy.Notifications.Signal == nil {
		return zero, fmt.Errorf("Signal installation configuration changed")
	}
	paths := backupCutoverPaths(s.home, c)
	if c.Paths != nil {
		current, e := loadInstalledWorker(s.home)
		if e != nil || protocol.Hash(current) != protocol.Hash(c) || s.configuration != c.Paths.Runtime {
			return zero, fmt.Errorf("Signal installation layout changed")
		}
	}
	images, err := signalFile(filepath.Join(filepath.Dir(paths.containerConfigFile()), "images.json"), 8192)
	if err != nil {
		return zero, err
	}
	if protocol.Hash(images) != s.imagesHash {
		return zero, fmt.Errorf("Signal image enrollment changed")
	}
	unitPath := filepath.Join(paths.quadletDirectory(), "deploy-backup-signal.container")
	unit, err := signalFile(unitPath, 64<<10)
	if err != nil {
		return zero, err
	}
	if string(unit) != s.unitText {
		return zero, fmt.Errorf("Signal Quadlet differs from managed configuration")
	}
	account, err := notification.ReadWorkerSecret(c.Policy.Notifications.Signal.AccountFile)
	if err != nil {
		return zero, err
	}
	recipient, err := notification.ReadWorkerSecret(c.Policy.Notifications.Signal.RecipientFile)
	if err != nil {
		return zero, err
	}
	raw, err := s.run(ctx, "show", backupSignalUnit, "--all", "--property=LoadState,ActiveState,SubState,MainPID,ControlPID,Job,FragmentPath,SourcePath,DropInPaths,UnitFileState,Names,NeedDaemonReload")
	if err != nil {
		return zero, err
	}
	properties, err := parseSignalService(raw)
	if err != nil {
		return zero, err
	}
	if properties["SourcePath"] != unitPath {
		return zero, fmt.Errorf("Signal service source is not the enrolled Quadlet")
	}
	runtimePath := os.Getenv("XDG_RUNTIME_DIR")
	if runtimePath == "" {
		runtimePath = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	validFragment := false
	for _, generator := range []string{"generator", "generator.early", "generator.late"} {
		if properties["FragmentPath"] == filepath.Join(runtimePath, "systemd", generator, backupSignalUnit) {
			validFragment = true
		}
	}
	if !filepath.IsAbs(runtimePath) || filepath.Clean(runtimePath) != runtimePath || !validFragment {
		return zero, fmt.Errorf("Signal service fragment is not generated in the user runtime")
	}
	fragment, err := signalFile(properties["FragmentPath"], 256<<10)
	if err != nil {
		return zero, err
	}
	// The manager's definition must be current, not only the source on disk.
	if !strings.Contains(string(fragment), s.image) || !strings.Contains(string(fragment), "__signal_relay") {
		return zero, fmt.Errorf("loaded Signal service does not identify the pinned relay")
	}
	stable := map[string]string{}
	for _, key := range []string{"FragmentPath", "SourcePath", "UnitFileState", "Names"} {
		stable[key] = properties[key]
	}
	binding := protocol.Hash(struct {
		Configuration, Images, Unit, Fragment, Credentials string
		Properties                                         map[string]string
	}{protocol.Hash(c), s.imagesHash, protocol.Hash(unit), protocol.Hash(fragment), protocol.Hash([]string{account, recipient}), stable})
	return signalaccount.ServiceState{Binding: binding, Active: properties["ActiveState"] == "active"}, nil
}

func parseSignalService(raw []byte) (map[string]string, error) {
	expected := map[string]bool{}
	for _, key := range strings.Split("LoadState,ActiveState,SubState,MainPID,ControlPID,Job,FragmentPath,SourcePath,DropInPaths,UnitFileState,Names,NeedDaemonReload", ",") {
		expected[key] = true
	}
	p := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || !expected[key] {
			return nil, fmt.Errorf("invalid Signal service inspection")
		}
		if _, seen := p[key]; seen {
			return nil, fmt.Errorf("duplicate Signal service property")
		}
		p[key] = value
	}
	if len(p) != len(expected) || p["LoadState"] != "loaded" || p["DropInPaths"] != "" || p["Names"] != backupSignalUnit || p["UnitFileState"] != "generated" || p["NeedDaemonReload"] != "no" || p["Job"] != "" || p["ControlPID"] != "0" {
		return nil, fmt.Errorf("Signal service has unsupported ownership, overrides or pending work")
	}
	pid, err := strconv.Atoi(p["MainPID"])
	if err != nil || pid < 0 {
		return nil, fmt.Errorf("invalid Signal service process identity")
	}
	switch p["ActiveState"] {
	case "active":
		if pid == 0 || p["SubState"] != "running" {
			return nil, fmt.Errorf("Signal service is not steadily running")
		}
	case "inactive":
		if pid != 0 || p["SubState"] != "dead" {
			return nil, fmt.Errorf("Signal service has not drained")
		}
	case "failed":
		if pid != 0 || p["SubState"] != "failed" {
			return nil, fmt.Errorf("Signal failed service has remaining work")
		}
	default:
		return nil, fmt.Errorf("Signal service is transitioning; retry after it settles")
	}
	return p, nil
}

func (s *signalService) command(ctx context.Context, verb string) error {
	view, err := s.Inspect(ctx)
	if err != nil {
		return err
	}
	if view.Binding != s.pinned {
		return fmt.Errorf("Signal service binding changed before manager mutation")
	}
	_, err = s.run(ctx, verb, backupSignalUnit)
	return err
}
func (s *signalService) Stop(ctx context.Context) error  { return s.command(ctx, "stop") }
func (s *signalService) Start(ctx context.Context) error { return s.command(ctx, "start") }

func signalAccountProbe(ctx context.Context, image string) error {
	cmd := exec.CommandContext(ctx, "podman", "run", "--rm", "--pull=never", "--network=none", "--read-only", "--cap-drop=all", "--security-opt=no-new-privileges", "--memory=128m", "--pids-limit=64", "--entrypoint=/usr/local/bin/deploy", image, "notification-worker", "__signal-account-probe")
	raw, err := process.LimitedOutput(cmd, 30*time.Second, 4096)
	if err != nil {
		return fmt.Errorf("Signal relay image must be upgraded to support coordinated account maintenance")
	}
	var v struct {
		Version int `json:"version"`
	}
	if err = safefs.DecodeJSON(raw, &v, 4096); err != nil || v.Version != signalaccount.MaintenanceVersion {
		return fmt.Errorf("Signal relay image maintenance protocol mismatch")
	}
	return nil
}

func (s *signalService) VerifyRecoverySupport(ctx context.Context) error {
	return s.VerifyNextRecoverySupport(ctx)
}
func (s *signalService) VerifyNextRecoverySupport(ctx context.Context) error {
	if s.adoptionProbe != nil {
		return s.adoptionProbe(ctx, s.image)
	}
	cmd := exec.CommandContext(ctx, "podman", "run", "--rm", "--pull=never", "--network=none", "--read-only", "--cap-drop=all", "--security-opt=no-new-privileges", "--memory=128m", "--pids-limit=64", "--entrypoint=/usr/local/bin/deploy", s.image, "backup-worker", "__signal-adoption-probe")
	raw, err := process.LimitedOutput(cmd, 30*time.Second, 1024)
	if err != nil || strings.TrimSpace(string(raw)) != "backup-signal-adoption-v1" {
		return fmt.Errorf("legacy Signal image lacks account adoption support; update it before migration")
	}
	return nil
}
