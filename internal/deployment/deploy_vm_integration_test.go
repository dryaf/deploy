//go:build imageintegration

package deployment

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/transport"
)

const vmScenarioFile = "/home/podman/deploy-vm-scenario.json"

const vmReadyFile = "/home/podman/deploy-vm-ready.json"

type vmScenario struct {
	Root          string
	Mode          config.DeploymentMode
	Phase         string
	Config        config.NormalizedConfig
	BaselineImage string
}

type vmCheckpointRunner struct {
	integrationBinaryRunner
	phase, scenario, database string
}

func (r vmCheckpointRunner) Sync(e config.Environment, sources []string, destination string, args ...string) error {
	copySources := append([]string(nil), sources...)
	for i, source := range copySources {
		if filepath.Base(source) != "transaction.sh" {
			continue
		}
		body, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		hook := `phase_checkpoint() {
if [[ $1 == candidate-started && ` + transport.ShellQuote(r.phase) + ` == database-restoring ]]; then sqlite3 ` + transport.ShellQuote(r.database) + ` "UPDATE sample SET value='candidate';"; fi
if [[ $1 == ` + transport.ShellQuote(r.phase) + ` ]]; then
 printf '%s\n' ` + transport.ShellQuote(`{"scenario":"`+r.scenario+`"}`) + ` | atomic_json ` + transport.ShellQuote(vmReadyFile) + `
 while true; do sleep 1; done
fi
}`
		original := "phase_checkpoint() { :; }"
		if strings.Count(string(body), original) != 1 {
			return fmt.Errorf("phase checkpoint hook missing or ambiguous")
		}
		dir, err := os.MkdirTemp("", "deploy-vm-hook-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		copySources[i] = filepath.Join(dir, "transaction.sh")
		if err = os.WriteFile(copySources[i], []byte(strings.Replace(string(body), original, hook, 1)), 0600); err != nil {
			return err
		}
	}
	return r.integrationBinaryRunner.Sync(e, copySources, destination, args...)
}

func requireVMFixture(t *testing.T) {
	t.Helper()
	if os.Getenv("DEPLOY_VM_CRASH_TEST") != "isolated" || os.Getenv("DEPLOY_IMAGE_INTEGRATION") != "isolated" {
		t.Fatal("VM fixture requires the disposable VM harness")
	}
}

func TestVMCrashPrepare(t *testing.T) {
	requireVMFixture(t)
	mode := config.DeploymentMode(os.Getenv("DEPLOY_VM_MODE"))
	phase := os.Getenv("DEPLOY_VM_PHASE")
	allowed := map[string]bool{"pending": true, "database-snapshot": true, "candidate-activation": true, "database-restoring": true, "commit": true, "pending-cleared": true}
	if (mode != config.ModeBinary && mode != config.ModeImage) || !allowed[phase] {
		t.Fatal("invalid VM scenario")
	}
	root, err := os.MkdirTemp("/home/podman", "deploy-vm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	database := filepath.Join(root, "target/data/app.db")
	testutil.CreateDatabase(t, database, "original")
	internalPort := 8080
	image := "localhost/deploy-vm-app:latest"
	c := config.Config{AppName: "deploy-vm", Deployment: config.DeploymentConfig{Mode: mode}, Artifacts: config.ArtifactsConfig{Include: []string{"version.txt"}}}
	if mode == config.ModeImage {
		r := integrationRunner{}
		pulled, err := r.SSH(config.Environment{}, "podman pull --quiet docker.io/library/nginx:1.28-alpine")
		if err != nil {
			t.Fatal(err)
		}
		image, err = r.SSH(config.Environment{}, "podman image inspect --format '{{index .RepoDigests 0}}' "+transport.ShellQuote(strings.TrimSpace(pulled)))
		if err != nil {
			t.Fatal(err)
		}
		image = strings.TrimSpace(image)
		internalPort = 80
	} else {
		c.BinaryName = "server"
		c.Build.Cmd = "cp /usr/local/bin/deploy-integration.test build/server"
		c.Artifacts.Include = append(c.Artifacts.Include, "Dockerfile.vps")
		if err = os.WriteFile("Dockerfile.vps", []byte("FROM scratch\nCOPY server /server\nENTRYPOINT [\"/server\",\"-test.run=^TestBinaryHelperServer$\"]\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	service := "deploy-vm-" + filepath.Base(root)
	c.Environments = map[string]config.Environment{"vm": {Host: "127.0.0.1", User: "podman", Dir: filepath.Join(root, "target"), Database: config.DatabaseConfig{Driver: "sqlite", Source: "data/app.db", Rollback: "restore"}, Quadlet: config.Quadlet{ServiceName: service, Image: image, EnvFile: ptr(""), Ports: []string{fmt.Sprintf("127.0.0.1:%d:%d", port, internalPort)}, EnvVars: []string{"DEPLOY_BINARY_HELPER=serve", "DEPLOY_TEST_VERSION=A"}, HealthURL: fmt.Sprintf("http://127.0.0.1:%d/", port), HealthTimeout: "10s", HealthStableFor: "100ms", HealthInterval: "20ms", HealthSuccesses: 2, HealthRequired: true}}}
	if err = os.WriteFile("version.txt", []byte("A"), 0600); err != nil {
		t.Fatal(err)
	}
	n, err := config.Normalize(c, "vm")
	if err != nil {
		t.Fatal(err)
	}
	runner := integrationBinaryRunner{}
	var p ReleasePlan
	if mode == config.ModeImage {
		p, err = planImageRelease(n, "A", strings.Repeat("a", 40))
	} else {
		p, err = planBinaryRelease(n, "A", strings.Repeat("a", 40))
	}
	if err != nil {
		t.Fatal(err)
	}
	if mode == config.ModeImage {
		err = executeImageRelease(p, runner)
	} else {
		err = executeBinaryRelease(p, runner)
	}
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := runner.SSH(n.Env, "podman container inspect --format '{{.Image}}' "+transport.ShellQuote("systemd-"+service))
	if err != nil {
		t.Fatal(err)
	}
	e := c.Environments["vm"]
	e.Quadlet.EnvVars = []string{"DEPLOY_BINARY_HELPER=serve", "DEPLOY_TEST_VERSION=B"}
	c.Environments["vm"] = e
	if phase == "database-restoring" {
		e := c.Environments["vm"]
		e.Quadlet.HealthCmd = "false"
		c.Environments["vm"] = e
	}
	n, err = config.Normalize(c, "vm")
	if err != nil {
		t.Fatal(err)
	}
	if err = safefs.WritePrivateJSON(vmScenarioFile, vmScenario{Root: root, Mode: mode, Phase: phase, Config: n, BaselineImage: strings.TrimSpace(baseline)}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile("version.txt", []byte("B"), 0600); err != nil {
		t.Fatal(err)
	}
	if mode == config.ModeImage {
		p, err = planImageRelease(n, "B", strings.Repeat("b", 40))
	} else {
		p, err = planBinaryRelease(n, "B", strings.Repeat("b", 40))
	}
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := vmCheckpointRunner{phase: phase, scenario: string(mode) + "-" + phase, database: database}
	if mode == config.ModeImage {
		err = executeImageRelease(p, checkpoint)
	} else {
		err = executeBinaryRelease(p, checkpoint)
	}
	t.Fatalf("candidate exited before VM power interruption: %v", err)
}

func TestVMCrashRecover(t *testing.T) {
	requireVMFixture(t)
	body, err := os.ReadFile(vmScenarioFile)
	if err != nil {
		t.Fatal(err)
	}
	var scenario vmScenario
	if err = json.Unmarshal(body, &scenario); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(scenario.Root, "/home/podman/deploy-vm-") {
		t.Fatal("unsafe fixture root")
	}
	n := scenario.Config
	r := integrationRunner{}
	script := imageTransactionScript
	if scenario.Mode == config.ModeBinary {
		script = binaryTransactionScript
	}
	if err = executeDeploymentServiceAction(n, "start", r, script); err != nil {
		t.Fatal(err)
	}
	expected := "A"
	if scenario.Phase == "commit" || scenario.Phase == "pending-cleared" {
		expected = "B"
	}
	state := filepath.Join(n.Env.Dir, ".deploy", string(scenario.Mode))
	current, err := os.ReadFile(filepath.Join(state, "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	var pointers struct{ Current string }
	if err = json.Unmarshal(current, &pointers); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(state, "releases", pointers.Current, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var confirmed struct {
		imageReleaseRequest
		ImageID string `json:"image_id"`
	}
	if err = json.Unmarshal(manifest, &confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed.Version != expected {
		t.Fatalf("expected %s after %s; recovered %s", expected, scenario.Phase, confirmed.Version)
	}
	if data, err := os.ReadFile(filepath.Join(n.Env.Dir, "version.txt")); err != nil || string(data) != expected {
		t.Fatal(string(data), err)
	}
	if testutil.DatabaseValue(t, filepath.Join(n.Env.Dir, "data/app.db")) != "original" {
		t.Fatal("database rewind did not preserve original state")
	}
	if _, err = os.Stat(filepath.Join(state, "pending.json")); !os.IsNotExist(err) {
		t.Fatal("pending marker remains", err)
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(n.Env.Quadlet.HealthURL)
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("application status %d", response.StatusCode)
	}
	if scenario.Mode == config.ModeBinary && string(contents) != expected {
		t.Fatalf("wrong running application: %s", contents)
	}
	actual, err := r.SSH(n.Env, "podman container inspect --format '{{index .Config.Labels \"io.deploy.commit\"}}' "+transport.ShellQuote("systemd-"+n.Env.Quadlet.ServiceName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(actual) != confirmed.Commit {
		t.Fatal("running container commit differs from manifest", actual)
	}
	actualImage, err := r.SSH(n.Env, "podman container inspect --format '{{.Image}}' "+transport.ShellQuote("systemd-"+n.Env.Quadlet.ServiceName))
	if err != nil {
		t.Fatal(err)
	}
	actualImage = "sha256:" + strings.TrimPrefix(strings.TrimSpace(actualImage), "sha256:")
	baselineImage := "sha256:" + strings.TrimPrefix(strings.TrimSpace(scenario.BaselineImage), "sha256:")
	if actualImage != confirmed.ImageID || (expected == "A" && actualImage != baselineImage) {
		t.Fatal("wrong recovered image", actualImage, confirmed.ImageID)
	}

	if scenario.Phase == "database-restoring" {
		backups, _ := filepath.Glob(filepath.Join(n.Env.Dir, "data/backup/app.vm.before-rollback.*.db"))
		if len(backups) != 1 || testutil.DatabaseValue(t, backups[0]) != "candidate" {
			t.Fatal("candidate snapshot was lost", backups)
		}
	}
	_, err = r.SSH(n.Env, "systemctl --user stop "+transport.ShellQuote(n.Env.Quadlet.ServiceName+".service")+"\nrm -f -- "+transport.ShellQuote(filepath.Join("/home/podman/.config/containers/systemd", n.Env.Quadlet.ServiceName+".container"))+"\nsystemctl --user daemon-reload")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(scenario.Root); err != nil {
		t.Fatal(err)
	}
	os.Remove(vmScenarioFile)
	os.Remove(vmReadyFile)
}
