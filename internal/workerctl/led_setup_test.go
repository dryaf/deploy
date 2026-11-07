package workerctl

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/ledartifact"
	"github.com/dryaf/deploy/internal/notification/ledhost"
)

func ledArchiveFixture(t *testing.T) Artifact {
	t.Helper()
	launcher, err := os.ReadFile("../notification/ledartifact/launcher.py")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"__main__.py": launcher, "LICENSE": []byte("fixture"), "pistatus/__init__.py": []byte("fixture"), "pistatus/daemon.py": []byte("fixture"), "pistatus/sysfs.py": []byte("fixture")}
	files["pistatus/deploy_removal.py"], err = os.ReadFile("../notification/ledartifact/removal_guard.py")
	if err != nil {
		t.Fatal(err)
	}
	m := ledartifact.Metadata{Version: 1, DaemonVersion: "0.1.0", SourceCommit: ledartifact.SourceCommit, RestoreGuard: 1, RemovalGuard: 1, Files: map[string]string{}}
	for name, body := range files {
		m.Files[name] = inputDigest(body)
	}
	files["deploy-led-artifact.json"], err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, body := range files {
		w, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write(body); e != nil {
			t.Fatal(e)
		}
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "daemon.pyz")
	if err = os.WriteFile(file, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return Artifact{File: file, SHA256: inputDigest(b.Bytes())}
}

func managedLEDSetupFixture(t *testing.T) (ControlRequest, ResolvedPaths, Artifact, Artifact, ledSetupLifecycle) {
	r, paths, _, _ := declaredLEDFixture(t)
	r.Desired.Notification.StatusLEDs = config.LEDDeclaration{Enabled: true, Daemon: config.LEDDaemon{Mode: "managed"}}
	helper := elfFixture(t, r.Facts.Arch)
	artifact := ledArchiveFixture(t)
	r.HelperSHA256 = helper.SHA256
	r.LEDSHA256 = artifact.SHA256
	r.DesiredHash, _ = r.Desired.Fingerprint()
	life := ledSetupLifecycle{discover: func(name string) (string, error) {
		if name != "ACT" {
			return "", errors.New("wrong device")
		}
		return "/sys/devices/platform/leds/leds/ACT", nil
	}, receipt: func(int) (ledhost.Receipt, error) { return ledhost.Receipt{}, os.ErrNotExist }}
	return r, paths, helper, artifact, life
}

func TestManagedLEDStagesReviewAndReusesHostReceipt(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless staging")
	}
	r, paths, helper, artifact, life := managedLEDSetupFixture(t)
	p, err := prepareManagedLED(context.Background(), r, paths, helper.File, artifact.File, life)
	if err != nil || p == nil {
		t.Fatal(p, err)
	}
	if p.Plan.Spec.ArtifactSHA256 != artifact.SHA256 || p.Plan.Spec.UID != os.Getuid() || !strings.HasPrefix(p.Command, "sudo -- ") || !strings.HasSuffix(p.Command, p.Plan.Hash) {
		t.Fatal("incorrect reviewed prerequisite", p)
	}
	if _, err = os.Stat(paths.Installation); !os.IsNotExist(err) {
		t.Fatal("initialized gateway before host prerequisite")
	}
	status, found, err := inspectDeclaredLEDProgress(context.Background(), r, paths, life.receipt)
	if err != nil || !found || status.Code != "led_host_prerequisite" || status.LEDPrerequisite == nil || !reflect.DeepEqual(status.LEDPrerequisite, p) {
		t.Fatal("pending host prerequisite invisible to status", status, err)
	}
	request, err := os.ReadFile(p.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	again, err := prepareManagedLED(context.Background(), r, paths, helper.File, artifact.File, life)
	if err != nil || !reflect.DeepEqual(again, p) {
		t.Fatal("retry changed prerequisite", again, err)
	}
	now, err := os.ReadFile(p.RequestFile)
	if err != nil || !bytes.Equal(now, request) {
		t.Fatal("retry rewrote reviewed request")
	}
	var output bytes.Buffer
	if err = RunLEDHostCLI(context.Background(), []string{"review", p.RequestFile}, &output); err != nil {
		t.Fatal(err)
	}
	var reviewed ledhost.Plan
	if json.Unmarshal(output.Bytes(), &reviewed) != nil || !reflect.DeepEqual(reviewed, p.Plan) {
		t.Fatal("review differs from proposed changes")
	}
	if err = RunLEDHostCLI(context.Background(), []string{"apply", p.RequestFile, p.Plan.Hash}, &output); err == nil {
		t.Fatal("rootless command applied host changes")
	}
	life.receipt = func(int) (ledhost.Receipt, error) {
		return ledhost.Receipt{Version: 1, Spec: p.Plan.Spec, Hash: p.Plan.Hash, Complete: true}, nil
	}
	ready, err := prepareManagedLED(context.Background(), r, paths, "missing", "missing", life)
	if err != nil || ready != nil {
		t.Fatal("completed host receipt required restaging", ready, err)
	}
	status, found, err = inspectDeclaredLEDProgress(context.Background(), r, paths, life.receipt)
	if err != nil || !found || status.Code != "led_host_ready" || status.LEDPrerequisite != nil {
		t.Fatal("host completion claimed worker activation", status, err)
	}
	r.LEDSHA256 = strings.Repeat("a", 64)
	if _, err = prepareManagedLED(context.Background(), r, paths, helper.File, artifact.File, life); err == nil {
		t.Fatal("catalog silently upgraded managed daemon")
	}
}

func TestManagedLEDPreparationRefusesDriftAndUnsupportedHostBeforeGatewayEffects(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless staging")
	}
	for _, kind := range []string{"device", "power", "socket", "digest", "foreign-receipt", "existing-worker", "request-drift", "artifact-drift"} {
		t.Run(kind, func(t *testing.T) {
			r, paths, helper, artifact, life := managedLEDSetupFixture(t)
			switch kind {
			case "device":
				r.Facts.LEDs = []string{"unknown"}
			case "power":
				r.Desired.Notification.StatusLEDs.PowerLED = true
			case "socket":
				r.Desired.Notification.StatusLEDs.Socket = "/run/foreign.sock"
			case "digest":
				r.LEDSHA256 = "bad"
			case "foreign-receipt":
				life.receipt = func(int) (ledhost.Receipt, error) { return ledhost.Receipt{}, errors.New("changed root files") }
			case "existing-worker":
				if err := os.MkdirAll(paths.Installation, 0700); err != nil {
					t.Fatal(err)
				}
			case "request-drift", "artifact-drift":
				p, err := prepareManagedLED(context.Background(), r, paths, helper.File, artifact.File, life)
				if err != nil {
					t.Fatal(err)
				}
				file := p.RequestFile
				if kind == "artifact-drift" {
					file = filepath.Join(filepath.Dir(file), "daemon.pyz")
				}
				if err = os.WriteFile(file, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := prepareManagedLED(context.Background(), r, paths, helper.File, artifact.File, life); err == nil {
				t.Fatal("unsafe setup accepted")
			}
			if _, err := os.Stat(paths.State); !os.IsNotExist(err) {
				t.Fatal("setup initialized worker state")
			}
		})
	}
}

func TestManagedPowerLEDStagesBothDevicesAndPinsOptIn(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("rootless staging")
	}
	r, paths, helper, artifact, life := managedLEDSetupFixture(t)
	r.Desired.Notification.StatusLEDs.PowerLED = true
	r.DesiredHash, _ = r.Desired.Fingerprint()
	r.Facts.LEDs = []string{"ACT", "PWR"}
	discovered := []string{}
	life.discover = func(name string) (string, error) {
		discovered = append(discovered, name)
		return "/sys/devices/platform/leds/leds/" + name, nil
	}
	if err := directNotificationSupport(r.Desired); err != nil {
		t.Fatal(err)
	}
	p, err := prepareManagedLED(context.Background(), r, paths, helper.File, artifact.File, life)
	if err != nil || p == nil {
		t.Fatal(p, err)
	}
	if p.Plan.Spec.Version != 2 || p.Plan.Spec.PowerDevice != "PWR" || strings.Join(discovered, ",") != "ACT,PWR" {
		t.Fatal("lost power opt-in/discovery", p.Plan.Spec, discovered)
	}
	var request ledhost.Request
	raw, err := os.ReadFile(p.RequestFile)
	if err != nil || json.Unmarshal(raw, &request) != nil || request.Spec != p.Plan.Spec {
		t.Fatal("administrator request differs", err)
	}
	if _, err := os.Stat(paths.Installation); !os.IsNotExist(err) {
		t.Fatal("staging installed gateway", err)
	}
	life.receipt = func(int) (ledhost.Receipt, error) {
		return ledhost.Receipt{Version: 1, Spec: request.Spec, Hash: p.Plan.Hash, Complete: true}, nil
	}
	if next, err := prepareManagedLED(context.Background(), r, paths, "missing", "missing", life); err != nil || next != nil {
		t.Fatal("completed power prerequisite restaged", next, err)
	}
	r.Desired.Notification.StatusLEDs.PowerLED = false
	r.DesiredHash, _ = r.Desired.Fingerprint()
	if _, err := prepareManagedLED(context.Background(), r, paths, helper.File, artifact.File, life); err == nil {
		t.Fatal("removed installed power hardware through ordinary apply")
	}
}

func TestLEDArtifactCatalogOverrideAndTampering(t *testing.T) {
	a := ledArchiveFixture(t)
	d := config.LEDDaemon{Mode: "managed"}
	if _, err := ResolveLEDArtifact(d, nil); err == nil {
		t.Fatal("missing artifact accepted")
	}
	got, err := ResolveLEDArtifact(d, &a)
	if err != nil || *got != a {
		t.Fatal(got, err)
	}
	d.Artifact = a.File
	d.SHA256 = a.SHA256
	if _, err = ResolveLEDArtifact(d, &Artifact{File: "missing"}); err != nil {
		t.Fatal("explicit override ignored", err)
	}
	if err = os.WriteFile(a.File, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveLEDArtifact(d, &a); err == nil {
		t.Fatal("changed archive accepted")
	}
}

type ledInspector struct{}

func (ledInspector) Inspect(context.Context, config.ServerConfig) (TargetFacts, error) {
	f := testFacts()
	f.Board = "Raspberry Pi 4 Model B"
	f.LEDs = []string{"ACT", "PWR"}
	f.LEDPaths = map[string]string{"ACT": "/sys/devices/platform/leds/leds/ACT"}
	return f, nil
}

func TestShortManagedLEDCLISelectsOfflineArtifactAndShowsPrerequisite(t *testing.T) {
	dir := t.TempDir()
	helper := elfFixture(t, "arm64")
	artifact := ledArchiveFixture(t)
	for name, source := range map[string]string{"deploy": helper.File, "daemon.pyz": artifact.File} {
		body, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), body, 0700); err != nil {
			t.Fatal(err)
		}
	}
	catalog := Catalog{Version: 1, Release: "fixture", Linux: map[string]ArchitectureArtifacts{"arm64": {Helper: Artifact{"deploy", helper.SHA256}, LEDs: &Artifact{"daemon.pyz", artifact.SHA256}}}}
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	catalogFile := filepath.Join(dir, "catalog.json")
	if err = os.WriteFile(catalogFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(cliInventory, "providers: [telegram]", `providers:
        telegram:
          token_file: ~/.config/private/token
          chat_id_file: ~/.config/private/chat
      status_leds:
        enabled: true`, 1)
	inventory := filepath.Join(dir, "server.yaml")
	if err = os.WriteFile(inventory, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	controller := qualificationController(func(r ControlRequest) (ControlResult, error) {
		calls++
		if r.LocalLED == nil || r.LEDSHA256 != artifact.SHA256 || r.LocalLED.File != filepath.Join(dir, "daemon.pyz") || len(r.Inputs) != 0 {
			t.Fatal("selected archive or target inputs changed", r)
		}
		return ControlResult{Version: 1, State: "needs_action", Code: "led_host_prerequisite", Next: "review the staged administrator prerequisite"}, nil
	})
	var out bytes.Buffer
	c := CLI{Inspector: ledInspector{}, Controller: controller, Out: &out}
	for _, args := range [][]string{{"manager"}, {"resume", "manager"}} {
		out.Reset()
		if err = c.Run(context.Background(), "notification-worker", args, Options{Inventory: inventory, Catalog: catalogFile, JSON: true, NonInteractive: true}); err != nil {
			t.Fatal(err)
		}
		var result ControlResult
		if json.Unmarshal(out.Bytes(), &result) != nil || result.Code != "led_host_prerequisite" {
			t.Fatal("missing prerequisite result", out.String())
		}
	}
	if calls != 2 {
		t.Fatal("short command did not reach target coordinator", calls)
	}
}
