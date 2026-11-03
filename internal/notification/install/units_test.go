package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotificationVerificationUnitPathsAndSystemd(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home %percent $literal")
	p := ServicePaths{Executable: filepath.Join(home, "bin/deploy"), Config: filepath.Join(home, "config/runtime.json"), State: filepath.Join(home, "state")}
	p.Socket = filepath.Join(p.State, "api.sock")
	p.DeniedPaths = []string{filepath.Join(home, "archive"), filepath.Join(home, "source-keys")}
	if err := os.MkdirAll(filepath.Dir(p.Executable), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	unit, err := RenderVerificationService(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"serve --local --verification", "RestrictAddressFamilies=AF_UNIX\n", "UMask=0077", "KillMode=control-group", "PrivateUsers=true", "PrivateDevices=true", "InaccessiblePaths=-%t/podman -%t/docker.sock", "ReadWritePaths=" + systemdPath(p.State)} {
		if !strings.Contains(unit, fragment) {
			t.Fatal("missing restriction", fragment, unit)
		}
	}
	for _, path := range p.DeniedPaths {
		if !strings.Contains(unit, "InaccessiblePaths="+systemdPath("-"+path)) {
			t.Fatal("missing source exclusion")
		}
	}
	if strings.Contains(unit, "AF_INET") || !strings.Contains(unit, "$$literal") || !strings.Contains(unit, "%%percent") {
		t.Fatal("unexpected permissions or expansions", unit)
	}
	analyzer, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd analyzer unavailable")
	}
	path := filepath.Join(root, "deploy-notification-worker.service")
	if err := os.WriteFile(path, []byte(unit), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(analyzer, "--user", "verify", "--man=no", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("invalid native verification unit: %v\n%s", err, output)
	}
}

func TestNotificationVerificationUnitRejectsWritableRuntimeAndInjection(t *testing.T) {
	base := ServicePaths{Executable: "/private/bin/deploy", Config: "/private/config/runtime.json", State: "/private/state", Socket: "/private/state/api.sock"}
	for _, change := range []func(*ServicePaths){
		func(p *ServicePaths) { p.State = "/" },
		func(p *ServicePaths) { p.Executable = p.State + "/deploy" },
		func(p *ServicePaths) { p.Config = p.State + "/runtime.json" },
		func(p *ServicePaths) { p.Socket = "/private/other/api.sock" },
		func(p *ServicePaths) { p.Socket = p.State },
		func(p *ServicePaths) { p.Config = "/private/../runtime.json" },
		func(p *ServicePaths) { p.Config = "/private/config\nExecStart=/bin/false" },
		func(p *ServicePaths) { p.Config = p.Executable },
		func(p *ServicePaths) { p.DeniedPaths = []string{"/private"} },
		func(p *ServicePaths) { p.DeniedPaths = []string{p.State + "/api.sock"} },
		func(p *ServicePaths) { p.DeniedPaths = []string{"/private/archive\nOther=value"} },
	} {
		p := base
		change(&p)
		if _, err := RenderVerificationService(p); err == nil {
			t.Fatal("unsafe service paths accepted", p)
		}
	}
}
