//go:build imageintegration

package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// SSH transport is enabled only by the disposable harness's generated connection
// settings. Manual isolated runs can retain the fast local transport.
type integrationRunner struct{}

type integrationBinaryRunner struct{ integrationRunner }

func (integrationBinaryRunner) Local(cmd *exec.Cmd) error {
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("local build: %w: %s", err, out)
	}
	return nil
}

// The statically compiled integration executable doubles as the deployed Go app.
func TestBinaryHelperServer(t *testing.T) {
	if os.Getenv("DEPLOY_BINARY_HELPER") != "serve" {
		t.Skip("container helper only")
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, os.Getenv("DEPLOY_TEST_VERSION")) })
	if err := http.ListenAndServe(":8080", nil); err != nil {
		t.Fatal(err)
	}
}

func integrationSSHEnvironment(e Environment) (Environment, bool) {
	port := os.Getenv("DEPLOY_TEST_SSH_PORT")
	if port == "" {
		return e, false
	}
	e.Host = "127.0.0.1"
	e.User = "podman"
	e.Port, _ = strconv.Atoi(port)
	e.SSHKey = os.Getenv("DEPLOY_TEST_SSH_KEY")
	e.KnownHostsFile = os.Getenv("DEPLOY_TEST_SSH_KNOWN_HOSTS")
	return e, true
}

func (integrationRunner) SSH(e Environment, script string) (string, error) {
	if transport, enabled := integrationSSHEnvironment(e); enabled {
		return runSSHOutput(transport, "export XDG_RUNTIME_DIR=/run/user/1000 DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus\n"+script)
	}
	cmd := exec.Command("bash", "-c", script)
	var out, errs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errs
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%w: %s", err, errs.String())
	}
	return out.String(), nil
}
func (integrationRunner) Sync(e Environment, sources []string, dest string, args ...string) error {
	_, local, ok := strings.Cut(dest, ":")
	if !ok {
		return fmt.Errorf("invalid test destination")
	}
	if transport, enabled := integrationSSHEnvironment(e); enabled {
		return runRsyncSafe(transport, sources, remoteDestination(transport, local), args...)
	}
	a := append([]string{"-a"}, args...)
	a = append(a, "--")
	a = append(a, sources...)
	a = append(a, local)
	out, err := exec.Command("rsync", a...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("rsync: %w: %s", err, out)
	}
	return nil
}
func (integrationRunner) Local(*exec.Cmd) error {
	return fmt.Errorf("integration image release must not build locally")
}

func TestRootlessImageDeploymentIntegration(t *testing.T) {
	if os.Getenv("DEPLOY_IMAGE_INTEGRATION") != "isolated" {
		t.Fatal("run only in a disposable Linux VM/account with DEPLOY_IMAGE_INTEGRATION=isolated")
	}
	gotenberg := os.Getenv("DEPLOY_TEST_GOTENBERG_IMAGE")
	if err := validateImageReference(gotenberg); err != nil || !strings.Contains(gotenberg, "@sha256:") {
		t.Fatal("DEPLOY_TEST_GOTENBERG_IMAGE must be a verified Chromium image pinned by digest")
	}
	r := integrationRunner{}
	if os.Getenv("DEPLOY_TEST_REQUIRE_SSH") == "1" {
		if _, enabled := integrationSSHEnvironment(Environment{}); !enabled {
			t.Fatal("release gate requires real SSH transport")
		}
		if _, err := r.SSH(Environment{}, "exit 42"); err == nil {
			t.Fatal("SSH lost remote exit status")
		}
	}
	if _, err := r.SSH(Environment{}, imageRemotePreflight); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("deploy-it-%d", time.Now().UnixNano())
	target := t.TempDir()
	service := prefix + "-http"
	network := prefix + "-net"
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	units := filepath.Join(configHome, "containers/systemd")
	if err := os.MkdirAll(units, 0700); err != nil {
		t.Fatal(err)
	}
	networkFile := filepath.Join(units, network+".network")
	if err := os.WriteFile(networkFile, []byte("[Network]\nNetworkName="+network+"\nDriver=bridge\n"), 0600); err != nil {
		t.Fatal(err)
	}
	services := []string{service, prefix + "-pdf"}
	t.Cleanup(func() {
		for _, s := range services {
			r.SSH(Environment{}, "systemctl --user stop "+shellQuote(s+".service"))
			os.Remove(filepath.Join(units, s+".container"))
			os.RemoveAll(filepath.Join(units, s+".container.d"))
		}
		r.SSH(Environment{}, "systemctl --user stop "+shellQuote(network+"-network.service"))
		os.Remove(networkFile)
		r.SSH(Environment{}, "systemctl --user daemon-reload")
		exec.Command("podman", "network", "rm", network).Run()
		// Staging preserves read-only artifact modes. Make this disposable tree
		// removable after stopping its services, before testing.TempDir cleanup.
		filepath.WalkDir(target, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return os.Chmod(p, 0700)
			}
			return nil
		})
	})
	c := imageConfig()
	changeEnv(&c, func(e *Environment) {
		e.Dir = target
		e.Quadlet = Quadlet{ServiceName: service, Image: "docker.io/library/nginx:1.28-alpine", Network: network + ".network", NetworkAliases: []string{service}, Router: RouterConfig{Enabled: ptr(false)}, AutoRestart: true, CPU: "0.5", Memory: "128M", HealthCmd: "wget -q --spider http://127.0.0.1/", HealthTimeout: "30s"}
	})
	deploy := func(c Config) (ReleasePlan, error) {
		n, err := normalizeAndValidateConfig(c, "prod")
		if err != nil {
			return ReleasePlan{}, err
		}
		p, err := planImageRelease(n, "integration", "")
		if err == nil {
			err = executeImageRelease(p, r)
		}
		return p, err
	}
	first, err := deploy(c)
	if err != nil {
		t.Fatal(err)
	}
	changeEnv(&c, func(e *Environment) { e.Quadlet.Image = "docker.io/library/nginx:1.27-alpine" })
	second, err := deploy(c)
	if err != nil {
		t.Fatal(err)
	}
	// Generated units need a persistent Quadlet override, rather than systemctl enable.
	for _, action := range []string{"disable", "enable"} {
		if err := executeImageServiceAction(second.Config, action, r); err != nil {
			t.Fatal(err)
		}
		fragment, err := r.SSH(Environment{}, "systemctl --user show "+shellQuote(service+".service")+" -p FragmentPath --value")
		if err != nil {
			t.Fatal(err)
		}
		wantedLink := filepath.Join(filepath.Dir(strings.TrimSpace(fragment)), "default.target.wants", service+".service")
		_, statErr := os.Lstat(wantedLink)
		if action == "enable" && statErr != nil {
			t.Fatal("generator did not enable service", statErr)
		}
		if action == "disable" && !os.IsNotExist(statErr) {
			t.Fatal("generator kept disabled service enabled", statErr)
		}
	}
	currentPath := filepath.Join(target, ".deploy/image/current.json")
	before, err := os.ReadFile(currentPath)
	if err != nil {
		t.Fatal(err)
	}
	changeEnv(&c, func(e *Environment) {
		e.Quadlet.HealthCmd = "false"
		e.Quadlet.HealthTimeout = "2s"
		e.Quadlet.HealthStableFor = "100ms"
		e.Quadlet.HealthInterval = "20ms"
		e.Quadlet.HealthSuccesses = 2
	})
	if _, err := deploy(c); err == nil {
		t.Fatal("unhealthy rollout unexpectedly succeeded")
	}
	after, err := os.ReadFile(currentPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("commit changed after failed rollout: %v", err)
	}
	if connection, realSSH := integrationSSHEnvironment(Environment{}); realSSH {
		if err := pruneImages(connection); err != nil {
			t.Fatal(err)
		}
	} else if _, err := r.SSH(Environment{}, transactionCommonScript+"\nstore_lock -x\npodman image prune -f\npodman image prune --build-cache -f\n"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []ReleasePlan{first, second} {
		manifest := filepath.Join(target, ".deploy/image/releases", p.ReleaseID, "manifest.json")
		if _, err := r.SSH(Environment{}, "podman image exists \"$(jq -r .image_id "+shellQuote(manifest)+")\""); err != nil {
			t.Fatal("retained current/previous image missing after prune", err)
		}
	}

	changeEnv(&c, func(e *Environment) {
		e.Dir = filepath.Join(target, "pdf")
		e.Quadlet = Quadlet{ServiceName: prefix + "-pdf", Image: gotenberg, Network: network + ".network", NetworkAliases: []string{prefix + "-pdf"}, Router: RouterConfig{Enabled: ptr(false)}, AutoRestart: true, CPU: "1", Memory: "1G", HealthCmd: "curl --fail --silent --max-time 3 http://127.0.0.1:3000/health", HealthTimeout: "60s", EnvVars: []string{"CHROMIUM_AUTO_START=true", "CHROMIUM_MAX_CONCURRENCY=2", "CHROMIUM_MAX_QUEUE_SIZE=10"}}
	})
	if _, err := deploy(c); err != nil {
		t.Fatal(err)
	}
	pdfService := prefix + "-pdf"
	if out, err := r.SSH(Environment{}, "podman port "+shellQuote("systemd-"+pdfService)); err != nil || strings.TrimSpace(out) != "" {
		t.Fatal("PDF service published a host port", err, out)
	}
	if _, err := r.SSH(Environment{}, "systemctl --user restart "+shellQuote(pdfService+".service")); err != nil {
		t.Fatal(err)
	}
	n, err := normalizeAndValidateConfig(c, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SSH(Environment{}, "timeout -s KILL 60 bash -c "+shellQuote(strings.Split(imageTransactionScript, "\nfail() {")[0])+" -- health "+shellQuote(pdfService)+" '' true ''"); err != nil {
		t.Fatal(err)
	}
	html := filepath.Join(target, "index.html")
	if err := os.WriteFile(html, []byte("<!doctype html><h1>PDF integration test</h1>"), 0644); err != nil {
		t.Fatal(err)
	}
	// The second container proves DNS and PDF conversion through the private network.
	client := "docker.io/curlimages/curl:8.12.1"
	pdf := filepath.Join(target, "test.pdf")
	command := "podman run --rm --network " + shellQuote(network) + " -v " + shellQuote(html+":/input/index.html:ro,Z") + " " + shellQuote(client) + " --fail --silent --show-error --max-time 60 -F files=@/input/index.html " + shellQuote("http://"+pdfService+":3000/forms/chromium/convert/html") + " > " + shellQuote(pdf)
	if _, err := r.SSH(n.Env, command); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pdf)
	if err != nil || !bytes.HasPrefix(data, []byte("%PDF-")) {
		t.Fatal("invalid generated PDF", err)
	}
	for key, want := range map[string]string{"CPUQuotaPerSecUSec": "1s", "MemoryMax": "1073741824"} {
		out, err := r.SSH(Environment{}, "systemctl --user show "+shellQuote(pdfService+".service")+" -p "+key+" --value")
		if err != nil || strings.TrimSpace(out) != want {
			t.Fatalf("resource %s: %q %v", key, out, err)
		}
	}
	controlGroup, err := r.SSH(Environment{}, "systemctl --user show "+shellQuote(pdfService+".service")+" -p ControlGroup --value")
	if err != nil {
		t.Fatal(err)
	}
	cgroupDir := "/sys/fs/cgroup" + strings.TrimSpace(controlGroup)
	actualMemory, err := r.SSH(Environment{}, "cat -- "+shellQuote(filepath.Join(cgroupDir, "memory.max")))
	if err != nil || strings.TrimSpace(actualMemory) != "1073741824" {
		diagnostics, _ := r.SSH(Environment{}, "systemctl --user show "+shellQuote(pdfService+".service")+" -p ActiveState -p SubState -p ControlGroup; cat /proc/self/cgroup /sys/fs/cgroup/cgroup.controllers /sys/fs/cgroup/cgroup.subtree_control; ls -ld -- "+shellQuote(cgroupDir)+"; ls -- "+shellQuote(cgroupDir)+"; true")
		t.Fatalf("kernel memory limit: %q %v\n%s", actualMemory, err, diagnostics)
	}
	actualCPU, err := r.SSH(Environment{}, "cat -- "+shellQuote(filepath.Join(cgroupDir, "cpu.max")))
	quota := strings.Fields(actualCPU)
	if err != nil || len(quota) != 2 || quota[0] == "max" || quota[0] != quota[1] {
		t.Fatalf("kernel CPU limit is not one CPU: %q %v", actualCPU, err)
	}
	t.Run("binary configuration rollback", func(t *testing.T) {
		t.Chdir(t.TempDir())
		binaryService := prefix + "-binary"
		services = append(services, binaryService)
		binaryTarget := filepath.Join(target, "binary")
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		c := imageConfig()
		c.Deployment.Mode = ModeBinary
		c.BinaryName = "server"
		c.Build.Cmd = "cp " + shellQuote(executable) + " build/server"
		c.Artifacts.Include = []string{"Dockerfile.vps", "config"}
		changeEnv(&c, func(e *Environment) {
			e.Dir = binaryTarget
			e.SyncEnvFile = ".env.prod"
			e.Quadlet = Quadlet{ServiceName: binaryService, Image: "localhost/" + binaryService + ":latest", Network: network + ".network", AutoRestart: true, StopOnDeploy: true, EnvVars: []string{"DEPLOY_BINARY_HELPER=serve"}, Exec: "/server -test.run=^TestBinaryHelperServer$", HealthCmd: "wget -q --spider http://127.0.0.1:8080/", HealthTimeout: "30s", Memory: "128MiB"}
		})
		if err := os.Mkdir("config", 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("config/version", []byte("old configuration"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod("config", 0555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod("config", 0700); os.Chmod(filepath.Join(binaryTarget, "config"), 0700) })
		if err := os.WriteFile("Dockerfile.vps", []byte("FROM docker.io/library/alpine:3.21\nCOPY server /server\nCOPY config /config\nENTRYPOINT [\"/server\"]\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(".env.prod", []byte("DEPLOY_TEST_VERSION=old\n"), 0600); err != nil {
			t.Fatal(err)
		}
		deployBinary := func() error {
			n, err := normalizeAndValidateConfig(c, "prod")
			if err != nil {
				return err
			}
			p, err := planBinaryRelease(n, "integration", "")
			if err != nil {
				return err
			}
			return executeBinaryRelease(p, integrationBinaryRunner{})
		}
		if err := deployBinary(); err != nil {
			t.Fatal(err)
		}
		unitPath := filepath.Join(units, binaryService+".container")
		oldUnit, err := os.ReadFile(unitPath)
		if err != nil {
			t.Fatal(err)
		}
		os.Chmod("config", 0700)
		if err := os.WriteFile("config/version", []byte("new configuration"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(".env.prod", []byte("DEPLOY_TEST_VERSION=new\n"), 0600); err != nil {
			t.Fatal(err)
		}
		changeEnv(&c, func(e *Environment) {
			e.Quadlet.HealthCmd = "false"
			e.Quadlet.HealthTimeout = "2s"
			e.Quadlet.HealthStableFor = "100ms"
			e.Quadlet.HealthInterval = "20ms"
			e.Quadlet.HealthSuccesses = 2
		})
		if err := deployBinary(); err == nil {
			t.Fatal("unhealthy binary release succeeded")
		}
		restored, err := os.ReadFile(unitPath)
		if err != nil || !bytes.Equal(oldUnit, restored) {
			t.Fatal("old binary Quadlet not restored", err)
		}
		for file, want := range map[string]string{".env": "DEPLOY_TEST_VERSION=old\n", "config/version": "old configuration"} {
			data, err := os.ReadFile(filepath.Join(binaryTarget, file))
			if err != nil || string(data) != want {
				t.Fatal(file, string(data), err)
			}
		}
		out, err := r.SSH(Environment{}, "podman exec "+shellQuote("systemd-"+binaryService)+" wget -qO- http://127.0.0.1:8080/")
		if err != nil || strings.TrimSpace(out) != "old" {
			t.Fatal("old app/environment not running", out, err)
		}
		memory, err := r.SSH(Environment{}, "systemctl --user show "+shellQuote(binaryService+".service")+" -p MemoryMax --value")
		if err != nil || strings.TrimSpace(memory) != "134217728" {
			t.Fatal("MiB limit not enforced", memory, err)
		}
		// Exercise a real rootless database update where only the container's
		// mapped UID can traverse the data directory or read the old database.
		n, err := normalizeAndValidateConfig(c, "prod")
		if err != nil {
			t.Fatal(err)
		}
		if err := executeDeploymentServiceAction(n, "stop", r, binaryTransactionScript); err != nil {
			t.Fatal(err)
		}
		dataDir := filepath.Join(binaryTarget, "data")
		t.Cleanup(func() { r.SSH(Environment{}, "podman unshare chown -R 0:0 "+shellQuote(dataDir)) })
		remoteDB := filepath.Join(dataDir, "app.db")
		if _, err := r.SSH(Environment{}, "mkdir -p "+shellQuote(dataDir)+" && sqlite3 "+shellQuote(remoteDB)+" \"CREATE TABLE sample(value TEXT); INSERT INTO sample VALUES ('old');\" && podman unshare chown -R 65532:65532 "+shellQuote(dataDir)+" && podman unshare chmod 700 "+shellQuote(dataDir)); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir("data", 0700); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("sqlite3", "data/app.db", "CREATE TABLE sample(value TEXT); INSERT INTO sample VALUES ('new');").CombinedOutput(); err != nil {
			t.Fatal(string(out), err)
		}
		dbEnv := n.Env
		dbEnv.Database = DatabaseConfig{Driver: "sqlite", Source: "data/app.db"}
		dbEnv.Quadlet.ContainerUID = 65532
		dbEnv.Quadlet.ContainerGID = 65532
		if err := pushDatabase(dbEnv, ModeBinary, r); err != nil {
			t.Fatal(err)
		}
		for file, want := range map[string]string{remoteDB: "new", remoteDB + ".bak": "old"} {
			out, err := r.SSH(Environment{}, "podman unshare sqlite3 -readonly "+shellQuote(file)+" 'SELECT value FROM sample;'")
			if err != nil || strings.TrimSpace(out) != want {
				t.Fatal("rootless database or backup wrong", out, err)
			}
		}
	})

}
