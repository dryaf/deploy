//go:build notificationintegration

package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

func TestNotificationUserSandboxProbe(t *testing.T) {
	base := os.Getenv("DEPLOY_NOTIFICATION_SANDBOX_PROBE")
	if base == "" {
		t.Skip("only run as temporary sandbox probe")
	}
	if err := os.WriteFile(filepath.Join(base, "state", "write-test"), []byte("disposable"), 0600); err != nil {
		t.Fatal("state is not writable", err)
	}
	if err := os.WriteFile(filepath.Join(base, "readonly", "write-test"), []byte("disposable"), 0600); err == nil {
		t.Fatal("ProtectSystem did not prevent writes outside state")
	}
	if _, err := os.ReadFile(filepath.Join(base, "excluded", "synthetic-key")); err == nil {
		t.Fatal("source exclusion is readable")
	}
	for _, family := range []int{unix.AF_INET, unix.AF_INET6} {
		fd, err := unix.Socket(family, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if err == nil {
			unix.Close(fd)
			t.Fatal("IP socket creation permitted", family)
		}
	}
	fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal("Unix sockets unavailable", err)
	}
	unix.Close(fd)
	d, err := safefs.Open(filepath.Join(base, "readonly"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lease, err := d.Lock("installation.lock", false, false)
	if err != nil {
		t.Fatal("readonly installation lease unavailable", err)
	}
	lease.Close()
}

// Explicit opt-in: this test starts only a uniquely named, transient probe in
// the actual user manager. It never installs the notification unit or runs a
// notification/relay process, and never makes network connections.
func TestNotificationRealUserSandbox(t *testing.T) {
	if os.Getenv("DEPLOY_QUALIFY_USER_SYSTEMD") != "1" {
		t.Skip("set DEPLOY_QUALIFY_USER_SYSTEMD=1 to run a transient user-manager probe")
	}
	base := t.TempDir()
	for _, name := range []string{"state", "readonly", "excluded"} {
		if err := os.Mkdir(filepath.Join(base, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "excluded", "synthetic-key"), []byte("disposable"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "readonly", "installation.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	id, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	args := []string{"--user", "--wait", "--pipe", "--collect", "--unit=deploy-notification-probe-" + id, "--setenv=DEPLOY_NOTIFICATION_SANDBOX_PROBE=" + base}
	// Match the generated verification unit's sandbox/resource properties.
	for _, property := range []string{"Type=exec", "RuntimeMaxSec=20s", "TimeoutStopSec=5s", "KillMode=control-group", "UMask=0077", "MemoryMax=256M", "TasksMax=64", "CPUQuota=100%", "LimitNOFILE=1024", "NoNewPrivileges=true", "PrivateUsers=true", "CapabilityBoundingSet=", "AmbientCapabilities=", "PrivateDevices=true", "ProtectSystem=strict", "ProtectHome=read-only", "ReadWritePaths=" + systemdPath(filepath.Join(base, "state")), "InaccessiblePaths=" + systemdPath(filepath.Join(base, "excluded")) + fmt.Sprintf(" -/run/user/%d/podman -/run/user/%d/docker.sock", os.Getuid(), os.Getuid()), "RestrictAddressFamilies=AF_UNIX", "RestrictSUIDSGID=true", "LockPersonality=true"} {
		args = append(args, "--property="+property)
	}
	args = append(args, "--", executable, "-test.run=^TestNotificationUserSandboxProbe$", "-test.v")
	cmd := exec.CommandContext(ctx, "systemd-run", args...)
	var out, stderr process.Buffer
	out.Limit = 16384
	stderr.Limit = 16384
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err = process.Run(cmd, 40*time.Second, 2*time.Second)
	if err != nil {
		t.Fatal(fmt.Errorf("user sandbox is not qualified: %w; %s; %s", err, out.Bytes(), stderr.Bytes()))
	}
	t.Log(out.String())
}
