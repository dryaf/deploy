package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func podmanSignalSpecFixture(t *testing.T) PodmanSignalServiceSpec {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("rootless fixture requires ordinary UID")
	}
	n, rc := nativeSignalSpecFixture(t)
	base := filepath.Dir(n.Executable)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(base, "runtime"))
	s := PodmanSignalServiceSpec{Quadlet: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "containers/systemd", podmanSignalName+".container"), Fragment: filepath.Join(os.Getenv("XDG_RUNTIME_DIR"), "systemd/generator", nativeSignalUnit), ImageID: strings.Repeat("a", 64), ContainerID: strings.Repeat("b", 64), ExecutableSHA256: n.ExecutableSHA256, RelayConfig: n.RelayConfig, AccountState: rc.StateDir, Inbox: rc.Interactive.InboxDir, SocketDirectory: filepath.Dir(rc.Socket), AccountFile: rc.AccountFile, RecipientFile: rc.RecipientFile, Network: "none"}
	for _, p := range []string{filepath.Dir(s.Quadlet), filepath.Dir(s.Fragment), s.AccountState, s.Inbox, s.SocketDirectory} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	rc.StateDir, rc.Socket, rc.AccountFile, rc.RecipientFile, rc.Interactive.InboxDir = "/signal-state", "/relay/signal.sock", "/secrets/signal-account", "/secrets/signal-recipient", "/signal-inbox"
	b, _ := json.Marshal(rc)
	write := func(p string, b []byte) string {
		t.Helper()
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		return managedDigest(b)
	}
	s.RelaySHA256 = write(s.RelayConfig, b)
	s.QuadletSHA256 = write(s.Quadlet, []byte(RenderPodmanSignalRecoveryQuadlet(s)))
	s.FragmentSHA256 = write(s.Fragment, []byte("fixture-generated-unit"))
	if _, _, err := s.files(); err != nil {
		t.Fatal(err)
	}
	return s
}

func podmanSignalContainerFixture(t *testing.T, s PodmanSignalServiceSpec) signalPodmanContainer {
	t.Helper()
	var c signalPodmanContainer
	c.ID, c.Name, c.Image, c.ImageName, c.Path = s.ContainerID, podmanSignalName, s.ImageID, "sha256:"+s.ImageID, podmanSignalExecutable
	c.Args = signalContainerArgv()[1:]
	c.Config.User = "0"
	c.Config.Entrypoint = []string{podmanSignalExecutable}
	c.Config.Cmd = signalContainerArgv()[1:]
	c.Config.CreateCommand = s.startArgv()
	c.State.Status, c.State.Running, c.State.Pid = "running", true, 333
	c.ConmonPidFile = filepath.Join(t.TempDir(), "conmon.pid")
	if err := os.WriteFile(c.ConmonPidFile, []byte("123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c.HostConfig.ReadonlyRootfs, c.HostConfig.AutoRemove = true, true
	c.HostConfig.NetworkMode = s.Network
	c.HostConfig.PidMode = "private"
	c.HostConfig.SecurityOpt = []string{"no-new-privileges"}
	c.HostConfig.RestartPolicy.Name = "no"
	c.HostConfig.CapAdd = []string{}
	c.HostConfig.Devices = []json.RawMessage{}
	for _, m := range s.mounts() {
		c.Mounts = append(c.Mounts, struct {
			Type, Source, Destination string
			RW                        bool
			Options                   []string
			Propagation               string
		}{"bind", m.Source, m.Destination, m.RW, []string{"rbind"}, "rprivate"})
	}
	return c
}

type podmanSignalManagerFixture struct {
	t                            *testing.T
	spec                         PodmanSignalServiceSpec
	container                    signalPodmanContainer
	active, removed, unsupported bool
	stops                        int
	lease                        *signalaccount.Lease
	onStop                       func() error
}

func (f *podmanSignalManagerFixture) run(_ context.Context, cmd string, args ...string) ([]byte, error) {
	marshal := func(v any) ([]byte, error) { return json.Marshal(v) }
	switch cmd {
	case "systemctl":
		if reflect.DeepEqual(args, []string{"--user", "--no-pager", "stop", nativeSignalUnit}) {
			f.stops++
			f.active = false
			f.removed = true
			if f.lease != nil {
				f.lease.Close()
				f.lease = nil
			}
			if f.onStop != nil {
				return nil, f.onStop()
			}
			return nil, nil
		}
		if len(args) != 6 || args[2] != "show" || args[3] != nativeSignalUnit {
			f.t.Fatal("unexpected manager command", args)
		}
		m := f.spec.managerProperties()
		m["ActiveState"], m["SubState"], m["MainPID"] = "inactive", "dead", "0"
		if f.active {
			m["ActiveState"], m["SubState"], m["MainPID"] = "active", "running", "123"
		}
		return nativeSignalManagerOutput(m), nil
	case "busctl":
		if len(args) != 7 {
			f.t.Fatal(args)
		}
		a := f.spec.startArgv()
		flags := []string{}
		if args[6] != "ExecStartEx" {
			a = []string{"/usr/bin/podman", "--remote=false", "rm", "-v", "-f", "-i", podmanSignalName}
		}
		if args[6] == "ExecStopPostEx" {
			flags = []string{"ignore-failure"}
		}
		return marshal(map[string]any{"type": "a(sasasttttuii)", "data": [][]any{{"/usr/bin/podman", a, flags, 0, 0, 0, 0, 0, 0, 0}}})
	case "podman":
		if len(args) < 2 || args[0] != "--remote=false" {
			f.t.Fatal("remote podman allowed", args)
		}
		switch args[1] {
		case "info":
			return marshal(map[string]any{"rootless": true, "cgroups": "v2", "graph": "/fixture/graph", "run": "/fixture/run"})
		case "image":
			return marshal(map[string]any{"id": f.spec.ImageID, "os": "linux", "arch": runtime.GOARCH})
		case "ps":
			if f.removed {
				return nil, nil
			}
			return []byte(f.container.ID + "\n"), nil
		case "container":
			return marshal(f.container)
		case "unshare":
			var p signalPodmanProcessProof
			if len(args) != 6 || safefs.DecodeJSON([]byte(args[5]), &p, 32768) != nil || p.PID != f.container.State.Pid {
				f.t.Fatal("wrong process proof", args)
			}
			return marshal(map[string]any{"version": 1, "proof_hash": protocol.Hash(p)})
		case "run":
			for _, arg := range args {
				if strings.Contains(arg, f.spec.AccountState) || strings.Contains(arg, f.spec.Inbox) {
					f.t.Fatal("probe has account mount")
				}
			}
			for _, want := range []string{"--pull=never", "--network=none", "--image-volume=ignore", "__signal-podman-recovery-probe"} {
				found := false
				for _, arg := range args {
					found = found || arg == want
				}
				if !found {
					f.t.Fatal("unsafe probe", args)
				}
			}
			version := 1
			if f.unsupported {
				version = 0
			}
			return marshal(map[string]any{"version": version, "executable_sha256": f.spec.ExecutableSHA256})
		}
	}
	f.t.Fatal("unexpected external command", cmd, args)
	return nil, errors.New("unexpected command")
}
func podmanSignalServiceFixture(t *testing.T) (*podmanSignalRecoveryService, *podmanSignalManagerFixture) {
	s := podmanSignalSpecFixture(t)
	f := &podmanSignalManagerFixture{t: t, spec: s, container: podmanSignalContainerFixture(t, s), active: true}
	svc := newPodmanSignalRecoveryService(s)
	svc.run = f.run
	return svc, f
}

func TestPodmanSignalRecoveryFenceAndLostStop(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "lost-stop"}[lost], func(t *testing.T) {
			svc, f := podmanSignalServiceFixture(t)
			ctx := context.Background()
			var err error
			f.lease, err = signalaccount.AcquireRuntime(f.spec.AccountState)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if f.lease != nil {
					f.lease.Close()
				}
			})
			state, err := svc.Inspect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			svc.binding = state.Binding
			id, _ := safefs.NewID("n")
			req := signalaccount.RecoveryRequest{Operation: id, Binding: state.Binding, Target: protocol.Hash("target")}
			applyCalls := 0
			apply := func(_ context.Context, check func() error) (string, error) {
				applyCalls++
				return protocol.Hash("installed"), check()
			}
			verify := func(_ context.Context, h string, check func() error) error {
				if h != protocol.Hash("installed") {
					t.Fatal(h)
				}
				return check()
			}
			if lost {
				f.onStop = func() error { return errors.New("lost response") }
			}
			status, err := signalaccount.FenceRecovery(ctx, f.spec.AccountState, req, svc, apply, verify)
			if lost {
				if err == nil || status.Phase != "fenced" || applyCalls != 0 {
					t.Fatal(status, err)
				}
				f.onStop = nil
				status, err = signalaccount.FenceRecovery(ctx, f.spec.AccountState, req, svc, apply, verify)
			}
			if err != nil || status.Phase != "complete" || status.RuntimeAllowed || f.stops != 1 || applyCalls != 1 {
				t.Fatal(status, err, f.stops, applyCalls)
			}
			again, err := signalaccount.FenceRecovery(ctx, f.spec.AccountState, req, svc, apply, verify)
			if err != nil || again != status || f.stops != 1 || applyCalls != 1 {
				t.Fatal(again, err)
			}
			f.removed = false
			f.container.ID = strings.Repeat("c", 64)
			if _, err = svc.Inspect(ctx); err == nil {
				t.Fatal("replacement container accepted")
			}
		})
	}
}

func TestPodmanSignalRejectsContainerDrift(t *testing.T) {
	for _, kind := range []string{"image", "argv", "creation-env", "readonly", "privileged", "userns", "caps", "network", "mount-path", "mount-rw", "extra-mount", "duplicate-mount", "missing-mount", "conmon", "paused", "pid", "process-proof", "manager-command", "store", "capability", "capability-hash"} {
		t.Run(kind, func(t *testing.T) {
			svc, f := podmanSignalServiceFixture(t)
			switch kind {
			case "image":
				f.container.Image = strings.Repeat("e", 64)
			case "argv":
				f.container.Args = append(f.container.Args, "extra")
			case "creation-env":
				f.container.Config.CreateCommand = append(f.container.Config.CreateCommand[:3], append([]string{"--env=EXTRA=1"}, f.container.Config.CreateCommand[3:]...)...)
			case "readonly":
				f.container.HostConfig.ReadonlyRootfs = false
			case "privileged":
				f.container.HostConfig.Privileged = true
			case "userns":
				f.container.HostConfig.UsernsMode = "keep-id"
			case "caps":
				f.container.EffectiveCaps = []string{"CAP_SYS_ADMIN"}
			case "network":
				f.container.HostConfig.NetworkMode = "host"
			case "mount-path":
				f.container.Mounts[0].Source += "-other"
			case "mount-rw":
				f.container.Mounts[3].RW = true
			case "extra-mount":
				m := f.container.Mounts[0]
				m.Destination = "/usr/local/bin"
				f.container.Mounts = append(f.container.Mounts, m)
			case "duplicate-mount":
				f.container.Mounts = append(f.container.Mounts, f.container.Mounts[0])
			case "missing-mount":
				f.container.Mounts = f.container.Mounts[1:]
			case "conmon":
				if err := os.WriteFile(f.container.ConmonPidFile, []byte("124"), 0600); err != nil {
					t.Fatal(err)
				}
			case "paused":
				f.container.State.Paused = true
			case "pid":
				f.container.State.Pid = 0
			case "capability":
				f.unsupported = true
			case "capability-hash":
				f.spec.ExecutableSHA256 = strings.Repeat("0", 64)
			default:
				svc.run = func(ctx context.Context, cmd string, args ...string) ([]byte, error) {
					if (kind == "process-proof" && cmd == "podman" && args[1] == "unshare") || (kind == "manager-command" && cmd == "busctl") || (kind == "store" && cmd == "podman" && args[1] == "info") {
						return []byte(`{}`), nil
					}
					return f.run(ctx, cmd, args...)
				}
			}
			if err := svc.VerifyRecoverySupport(context.Background()); err == nil {
				t.Fatal("unsafe container accepted")
			}
			if f.stops != 0 {
				t.Fatal("inspection stopped a service")
			}
		})
	}
}

func TestPodmanSignalDryRun(t *testing.T) {
	s := podmanSignalSpecFixture(t)
	c, path := signalRuntimeCLIConfiguration(t)
	c.Signal.Socket = filepath.Join(s.SocketDirectory, "signal.sock")
	writeRuntime(t, path, c)
	b, _ := os.ReadFile(path)
	id, _ := safefs.NewID("n")
	base := t.TempDir()
	spec := PodmanSignalRecoverySpec{Version: 1, Operation: id, GatewayConfig: path, GatewaySHA256: managedDigest(b), GatewayState: filepath.Join(base, "gateway"), CommitHash: strings.Repeat("c", 64), RelayDestination: filepath.Join(base, "destination"), Service: s}
	b, _ = json.Marshal(spec)
	p := filepath.Join(base, "request.json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	old := ui.DryRun
	ui.DryRun = true
	t.Cleanup(func() { ui.DryRun = old })
	var out bytes.Buffer
	if err := runRecoverySignalPodmanCLI([]string{"--local", "--spec", p}, &out); err != nil || !strings.Contains(out.String(), "[DRY]") {
		t.Fatal(err, out.String())
	}
	for _, p := range []string{spec.GatewayState, spec.RelayDestination, filepath.Join(s.AccountState, "deploy-recovery-intent.json")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("dry run changed state", p, err)
		}
	}
}

func TestPodmanSignalStartOptions(t *testing.T) {
	s := podmanSignalSpecFixture(t)
	a := s.startArgv()
	if err := s.checkStartArgv(a); err != nil {
		t.Fatal(err)
	}
	// The generator may reorder the two singleton options, but must not
	// replace their values or append another interpretation of the same flag.
	entry, pull := -1, -1
	for i, v := range a {
		if v == "--entrypoint" {
			entry = i
		}
		if v == "--pull" {
			pull = i
		}
	}
	a[entry], a[pull] = a[pull], a[entry]
	a[entry+1], a[pull+1] = a[pull+1], a[entry+1]
	if err := s.checkStartArgv(a); err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"--privileged", "--remote=true", "--rm", "--volume=/other:/signal-state:rw"} {
		b := append([]string{}, a[:3]...)
		b = append(b, option)
		b = append(b, a[3:]...)
		if err := s.checkStartArgv(b); err == nil {
			t.Fatal("extra option accepted", option)
		}
	}
	a = s.startArgv()
	for i, v := range a {
		if v == "--pull" {
			a[i+1] = "always"
		}
	}
	if err := s.checkStartArgv(a); err == nil {
		t.Fatal("pull policy changed")
	}
}

func TestPodmanSignalMissingAuthorityAndManagerDrift(t *testing.T) {
	for _, name := range []string{"privileged", "running", "caps", "mounts", "job", "source", "reload", "stop-command", "renamed"} {
		t.Run(name, func(t *testing.T) {
			svc, f := podmanSignalServiceFixture(t)
			run := svc.run
			svc.run = func(ctx context.Context, cmd string, args ...string) ([]byte, error) {
				b, err := run(ctx, cmd, args...)
				if err != nil {
					return b, err
				}
				if cmd == "podman" && args[1] == "container" {
					var m map[string]any
					if json.Unmarshal(b, &m) != nil {
						t.Fatal("fixture JSON")
					}
					switch name {
					case "privileged":
						delete(m["HostConfig"].(map[string]any), "Privileged")
					case "running":
						delete(m["State"].(map[string]any), "Running")
					case "caps":
						delete(m, "BoundingCaps")
					case "mounts":
						delete(m, "Mounts")
					}
					return json.Marshal(m)
				}
				if cmd == "systemctl" && args[2] == "show" {
					switch name {
					case "job":
						b = bytes.ReplaceAll(b, []byte("Job=\n"), []byte("Job=99\n"))
					case "source":
						b = bytes.ReplaceAll(b, []byte("SourcePath="+f.spec.Quadlet), []byte("SourcePath=/foreign"))
					case "reload":
						b = bytes.ReplaceAll(b, []byte("NeedDaemonReload=no"), []byte("NeedDaemonReload=yes"))
					}
				}
				if name == "stop-command" && cmd == "busctl" && args[6] == "ExecStopEx" {
					b = bytes.ReplaceAll(b, []byte(podmanSignalName), []byte("unrelated-container"))
				}
				if name == "renamed" && cmd == "podman" && args[1] == "ps" && strings.HasPrefix(args[5], "name=") {
					return nil, nil
				}
				return b, nil
			}
			if name == "renamed" {
				f.active = false
			}
			if _, err := svc.Inspect(context.Background()); err == nil {
				t.Fatal("missing/changed authority accepted")
			}
			if f.stops != 0 {
				t.Fatal("inspection mutated service")
			}
		})
	}
}
