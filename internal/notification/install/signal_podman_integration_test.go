//go:build notificationintegration

package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func generateSignalPodmanFixture(t *testing.T, s *PodmanSignalServiceSpec) {
	t.Helper()
	tool := "/usr/libexec/podman/quadlet"
	if _, err := os.Stat(tool); err != nil {
		t.Skip("Quadlet generator unavailable")
	}
	cmd := exec.Command(tool, "--user", "--dryrun")
	cmd.Env = append(os.Environ(), "QUADLET_UNIT_DIRS="+filepath.Dir(s.Quadlet))
	b, err := signalPodmanFixtureOutput(cmd, 10*time.Second, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "---" + nativeSignalUnit + "---\n"
	if !bytes.HasPrefix(b, []byte(prefix)) {
		t.Fatal("unexpected generator output", string(b))
	}
	b = bytes.TrimPrefix(b, []byte(prefix))
	starts := 0
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			starts++
			if e := s.checkStartArgv(strings.Fields(strings.TrimPrefix(line, "ExecStart="))); e != nil {
				t.Fatal(e, line)
			}
		}
	}
	if starts != 1 {
		t.Fatal("unexpected generated start commands")
	}
	for _, line := range []string{"ExecStop=/usr/bin/podman --remote=false rm -v -f -i " + podmanSignalName, "ExecStopPost=-/usr/bin/podman --remote=false rm -v -f -i " + podmanSignalName} {
		if !bytes.Contains(b, []byte(line+"\n")) {
			t.Fatal("generator command differs", line, string(b))
		}
	}
	if err = os.WriteFile(s.Fragment, b, 0600); err != nil {
		t.Fatal(err)
	}
	s.FragmentSHA256 = managedDigest(b)
}

func TestPodmanSignalQuadletContract(t *testing.T) {
	s := podmanSignalSpecFixture(t)
	generateSignalPodmanFixture(t, &s)
	if _, _, err := s.files(); err != nil {
		t.Fatal(err)
	}
}

func TestPodmanSignalHandoffIsolatedContainer(t *testing.T) {
	if os.Getenv("DEPLOY_PODMAN_FIXTURE") != "1" {
		t.Skip("set DEPLOY_PODMAN_FIXTURE=1 for a disposable rootless Podman store")
	}
	base := t.TempDir()
	if err := os.Chmod(base, 0700); err != nil {
		t.Fatal(err)
	}
	// Storage and manager fixtures are private. No existing image, container,
	// real account or user systemd unit is selected or modified by this test.
	storage := filepath.Join(base, "storage.conf")
	if err := os.WriteFile(storage, []byte("[storage]\ndriver=\"vfs\"\nrunroot="+strconv.Quote(filepath.Join(base, "runroot"))+"\ngraphroot="+strconv.Quote(filepath.Join(base, "graph"))+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_STORAGE_CONF", storage)
	engine := filepath.Join(base, "containers.conf")
	if err := os.WriteFile(engine, []byte("[engine]\ncgroup_manager=\"cgroupfs\"\nevents_logger=\"file\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_CONF", engine)
	run := func(args ...string) []byte {
		t.Helper()
		b, e := signalPodmanFixtureOutput(exec.Command("podman", append([]string{"--remote=false"}, args...)...), time.Minute, 128<<10)
		if e != nil {
			t.Fatal(args, e)
		}
		return b
	}
	flags := signalRuntimeCLIArguments(t)
	s := podmanSignalSpecFixture(t)
	// XDG_RUNTIME_DIR is the fake generator location. Podman has its own
	// runroot above; retain it for both inspection and user-namespace proof.
	if err := os.MkdirAll(os.Getenv("XDG_RUNTIME_DIR"), 0700); err != nil {
		t.Fatal(err)
	}
	buildRoot := filepath.Join(base, "build")
	if err := os.Mkdir(buildRoot, 0700); err != nil {
		t.Fatal(err)
	}
	deploy := filepath.Join(base, "deploy")
	cmd := exec.Command("go", "build", "-o", deploy, "./cmd/deploy")
	cmd.Dir = "../../.."
	if b, e := signalPodmanFixtureOutput(cmd, time.Minute, 16384); e != nil {
		t.Fatal(e, string(b))
	}
	// The actual deploy entry point advertises its own ELF hash without any
	// account/configuration argument. The scratch relay below implements the
	// same response solely for the controlled live-process fixture.
	capRaw, e := signalPodmanFixtureOutput(exec.Command(deploy, "notification-worker", "__signal-podman-recovery-probe"), 10*time.Second, 4096)
	var capability struct {
		Version          int    `json:"version"`
		ExecutableSHA256 string `json:"executable_sha256"`
	}
	deployBytes, readErr := os.ReadFile(deploy)
	if e != nil || readErr != nil || json.Unmarshal(capRaw, &capability) != nil || capability.Version != 1 || capability.ExecutableSHA256 != managedDigest(deployBytes) {
		t.Fatal("built deploy capability mismatch", e, readErr, string(capRaw))
	}
	const source = `package main
import("encoding/json";"crypto/sha256";"fmt";"os";"path/filepath";"syscall";"time")
func main(){
 if len(os.Args)==3 && os.Args[2]=="__signal-podman-recovery-probe"{b,e:=os.ReadFile("/proc/self/exe");if e!=nil{os.Exit(8)};fmt.Printf("{\"version\":1,\"executable_sha256\":\"%x\"}\n",sha256.Sum256(b));return}
 if len(os.Args)!=6{os.Exit(2)}
 var c struct{State string ` + "`json:\"state_dir\"`" + `;Interactive struct{Inbox string ` + "`json:\"inbox_dir\"`" + `} ` + "`json:\"interactive\"`" + `}
 b,e:=os.ReadFile(os.Args[5]);if e!=nil || json.Unmarshal(b,&c)!=nil{os.Exit(3)}
 var held []*os.File
 for _,p:=range []string{filepath.Join(c.State,"deploy-account.lock"),filepath.Join(c.Interactive.Inbox,"inbox.lock")}{f,e:=os.OpenFile(p,os.O_RDWR,0600);if e!=nil || syscall.Flock(int(f.Fd()),syscall.LOCK_EX|syscall.LOCK_NB)!=nil{os.Exit(4)};held=append(held,f)}
 defer func(){for _,f:=range held{f.Close()}}()
 if os.WriteFile(filepath.Join(c.State,"fixture-ready"),[]byte("ready"),0600)!=nil{os.Exit(5)}
 for{time.Sleep(time.Hour)}
}`
	goFile := filepath.Join(base, "relay.go")
	if err := os.WriteFile(goFile, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(buildRoot, "deploy")
	cmd = exec.Command("go", "build", "-o", helper, goFile)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, e := signalPodmanFixtureOutput(cmd, time.Minute, 16384); e != nil {
		t.Fatal(e, string(b))
	}
	b, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	s.ExecutableSHA256 = managedDigest(b)
	if err = os.WriteFile(filepath.Join(buildRoot, "Containerfile"), []byte("FROM scratch\nCOPY deploy /usr/local/bin/deploy\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s.ImageID = strings.TrimPrefix(strings.TrimSpace(string(run("build", "--quiet", "--pull=never", "--network=none", buildRoot))), "sha256:")
	if !safefs.DigestPattern.MatchString(s.ImageID) {
		t.Fatal("invalid built image ID", s.ImageID)
	}
	t.Cleanup(func() { _ = exec.Command("podman", "--remote=false", "image", "rm", s.ImageID).Run() })
	s.Inbox = flags[8]
	var rc relay.Config
	b, err = os.ReadFile(s.RelayConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &rc); err != nil {
		t.Fatal(err)
	}
	l, err := signalaccount.AcquireRuntime(s.AccountState)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	var raw Runtime
	b, err = os.ReadFile(flags[2])
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	raw.Signal.Socket = filepath.Join(s.SocketDirectory, "signal.sock")
	writeRuntime(t, flags[2], raw)
	quadlet := []byte(RenderPodmanSignalRecoveryQuadlet(s))
	if err = os.WriteFile(s.Quadlet, quadlet, 0600); err != nil {
		t.Fatal(err)
	}
	s.QuadletSHA256 = managedDigest(quadlet)
	generateSignalPodmanFixture(t, &s)
	a := s.startArgv()[2:] // run, ...; run() supplies --remote=false.
	s.ContainerID = strings.TrimSpace(string(run(a...)))
	if !safefs.DigestPattern.MatchString(s.ContainerID) {
		t.Fatal("invalid container ID", s.ContainerID)
	}
	t.Cleanup(func() { _ = exec.Command("podman", "--remote=false", "rm", "--force", s.ContainerID).Run() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(s.AccountState, "fixture-ready")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture relay failed to hold its leases", string(run("logs", s.ContainerID)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	b = run("container", "inspect", s.ContainerID, "--format", "{{json .}}")
	var container signalPodmanContainer
	if err = decodeSignalPodman(b, &container); err != nil {
		t.Fatal(err)
	}
	proofJSON, _ := json.Marshal(s.proof(container))
	proofBinary, e := os.Open(deploy)
	if e != nil {
		t.Fatal(e)
	}
	proofCmd := exec.Command("podman", "--remote=false", "unshare", "/proc/self/fd/3", "notification-worker", "__signal-podman-proof", string(proofJSON))
	proofCmd.ExtraFiles = []*os.File{proofBinary}
	_, e = signalPodmanFixtureOutput(proofCmd, 30*time.Second, 4096)
	proofBinary.Close()
	if e != nil {
		t.Fatal(e)
	}
	// A bind-mounted config can retain the old inode after atomic replacement
	// at the host path. Matching bytes alone must not pass live-mount proof.
	retained := s.RelayConfig + ".retained"
	configBytes, err := os.ReadFile(s.RelayConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(s.RelayConfig, retained); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.RelayConfig, configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if _, e = signalPodmanFixtureOutput(exec.Command("podman", "--remote=false", "unshare", deploy, "notification-worker", "__signal-podman-proof", string(proofJSON)), 30*time.Second, 4096); e == nil {
		t.Fatal("replacement bind-mount inode accepted")
	}
	if err = os.Remove(s.RelayConfig); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(retained, s.RelayConfig); err != nil {
		t.Fatal(err)
	}
	pidBytes, err := nativeSignalRead(container.ConmonPidFile, 64, false)
	if err != nil {
		t.Fatal(err)
	}
	manager := filepath.Join(base, "manager")
	if err = os.Mkdir(manager, 0700); err != nil {
		t.Fatal(err)
	}
	m := s.managerProperties()
	m["ActiveState"], m["SubState"], m["MainPID"] = "active", "running", strings.TrimSpace(string(pidBytes))
	write := func(name string, b []byte, mode os.FileMode) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(manager, name), b, mode); e != nil {
			t.Fatal(e)
		}
	}
	write("active", nativeSignalManagerOutput(m), 0600)
	m["ActiveState"], m["SubState"], m["MainPID"] = "inactive", "dead", "0"
	write("inactive", nativeSignalManagerOutput(m), 0600)
	fake := &podmanSignalManagerFixture{t: t, spec: s}
	for _, name := range []string{"ExecStartEx", "ExecStopEx", "ExecStopPostEx"} {
		b, e := fake.run(context.Background(), "busctl", "--user", "--json=short", "get-property", "org.freedesktop.systemd1", "object", "interface", name)
		if e != nil {
			t.Fatal(e)
		}
		write(name, b, 0600)
	}
	write("cid", []byte(s.ContainerID), 0600)
	write("systemctl", []byte(`#!/bin/sh
set -eu
d=${0%/*}
[ "$1" = --user ] && [ "$2" = --no-pager ] && [ "$4" = deploy-notification-signal.service ] || exit 90
case "$3" in
show) if [ -f "$d/stopped" ]; then cat "$d/inactive"; else cat "$d/active"; fi;;
stop) podman --remote=false rm --force "$(cat "$d/cid")" >/dev/null; touch "$d/stopped"; echo stop >> "$d/stops"; if [ ! -f "$d/lost" ]; then touch "$d/lost"; exit 91; fi;;
*) exit 92;;
esac
`), 0700)
	write("busctl", []byte("#!/bin/sh\nset -eu\ncase \"$7\" in ExecStartEx|ExecStopEx|ExecStopPostEx) cat \"${0%/*}/$7\";; *) exit 93;; esac\n"), 0700)
	t.Setenv("PATH", manager+":"+os.Getenv("PATH"))
	b, err = os.ReadFile(flags[2])
	if err != nil {
		t.Fatal(err)
	}
	op, _ := safefs.NewID("n")
	spec := PodmanSignalRecoverySpec{Version: 1, Operation: op, GatewayConfig: flags[2], GatewaySHA256: managedDigest(b), GatewayState: flags[4], CommitHash: flags[6], RelayDestination: flags[10], Service: s}
	if _, err = spec.prepare(); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(spec)
	request := filepath.Join(base, "request.json")
	if err = os.WriteFile(request, b, 0600); err != nil {
		t.Fatal(err)
	}
	invoke := func() ([]byte, error) {
		return signalPodmanFixtureOutput(exec.Command(deploy, "notification-worker", "recovery-signal-podman-handoff", "--local", "--spec", request), 2*time.Minute, 32768)
	}
	firstOutput, firstErr := invoke()
	if firstErr == nil {
		t.Fatal("lost stop response should fail", string(b))
	}
	status, err := signalaccount.InspectRecovery(s.AccountState)
	if err != nil || status.Phase != "fenced" {
		t.Fatal("handoff did not reach durable fence", status, err, firstErr, string(firstOutput))
	}
	b, err = invoke()
	if err != nil {
		t.Fatal(err, string(b))
	}
	var result struct {
		Installation relay.SignalRuntimeInstallation `json:"installation"`
		Account      signalaccount.RecoveryStatus    `json:"account"`
	}
	if err = json.Unmarshal(b, &result); err != nil || result.Account.Phase != "complete" || result.Account.RuntimeAllowed || result.Installation.IntakeEnabled {
		t.Fatal(result, err, string(b))
	}
	again, err := invoke()
	if err != nil || !bytes.Equal(b, again) {
		t.Fatal("exact replay failed", err, string(again))
	}
	stops, err := os.ReadFile(filepath.Join(manager, "stops"))
	if err != nil || string(stops) != "stop\n" {
		t.Fatal("repeat stop", err, string(stops))
	}
}

func signalPodmanFixtureOutput(cmd *exec.Cmd, duration time.Duration, limit int) ([]byte, error) {
	var out, errs process.Buffer
	out.Limit, errs.Limit = limit, 16384
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := process.Run(cmd, duration, time.Second); err != nil {
		return out.Bytes(), fmt.Errorf("%w: %s", err, errs.String())
	}
	return out.Bytes(), nil
}
