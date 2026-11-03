package install

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type signalPodmanContainer struct {
	ID                                          string `json:"Id"`
	Name, Image, ImageName, Path, ConmonPidFile string
	Args                                        []string
	State                                       struct {
		Status                            string
		Running, Paused, Restarting, Dead bool
		Pid                               uint32
	}
	Mounts []struct {
		Type, Source, Destination string
		RW                        bool
		Options                   []string
		Propagation               string
	}
	Config struct {
		User            string
		Entrypoint, Cmd []string
		CreateCommand   []string
	}
	HostConfig struct {
		Privileged                       bool
		ReadonlyRootfs                   bool
		UsernsMode, PidMode, NetworkMode string
		AutoRemove                       bool
		SecurityOpt, CapAdd              []string
		Devices                          []json.RawMessage
		RestartPolicy                    struct{ Name string }
	}
	EffectiveCaps, BoundingCaps []string
}

// Podman owns additional version-specific fields. Decode only this bounded
// inspection record, reject duplicate keys, and explicitly validate the fields
// used as authority. Unknown metadata is neither logged nor included in output.
func decodeSignalPodman(b []byte, v any) error {
	if len(b) > 128<<10 || safefs.RejectDuplicateJSONKeys(b) != nil || json.Unmarshal(b, v) != nil {
		return fmt.Errorf("invalid Signal Podman inspection")
	}
	return nil
}

func requireSignalPodmanFields(b []byte) error {
	var root map[string]json.RawMessage
	if json.Unmarshal(b, &root) != nil {
		return fmt.Errorf("invalid Signal container record")
	}
	check := func(m map[string]json.RawMessage, keys ...string) error {
		for _, key := range keys {
			v, ok := m[key]
			if !ok || string(v) == "null" {
				return fmt.Errorf("missing Signal container authority field %s", key)
			}
		}
		return nil
	}
	if err := check(root, "Id", "Name", "Image", "ImageName", "Path", "Args", "State", "Config", "HostConfig", "Mounts"); err != nil {
		return err
	}
	for section, keys := range map[string][]string{"State": {"Status", "Running", "Paused", "Restarting", "Dead", "Pid"}, "Config": {"User", "Entrypoint", "Cmd", "CreateCommand"}, "HostConfig": {"Privileged", "ReadonlyRootfs", "UsernsMode", "PidMode", "NetworkMode", "AutoRemove", "SecurityOpt", "CapAdd", "Devices", "RestartPolicy"}} {
		var sub map[string]json.RawMessage
		if json.Unmarshal(root[section], &sub) != nil {
			return fmt.Errorf("invalid Signal container section")
		}
		if err := check(sub, keys...); err != nil {
			return err
		}
	}
	for _, key := range []string{"EffectiveCaps", "BoundingCaps"} {
		if _, ok := root[key]; !ok {
			return fmt.Errorf("missing Signal capability inventory")
		}
	}
	return nil
}

type signalPodmanRun func(context.Context, string, ...string) ([]byte, error)

func signalPodmanCommand(ctx context.Context, run signalPodmanRun, args ...string) ([]byte, error) {
	b, err := run(ctx, "podman", append([]string{"--remote=false"}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("Signal Podman %s operation failed: %w", args[0], err)
	}
	return b, nil
}

func signalPodmanStore(ctx context.Context, run signalPodmanRun) (string, error) {
	b, err := signalPodmanCommand(ctx, run, "info", "--format", `{"rootless":{{json .Host.Security.Rootless}},"cgroups":{{json .Host.CgroupsVersion}},"graph":{{json .Store.GraphRoot}},"run":{{json .Store.RunRoot}}}`)
	if err != nil {
		return "", err
	}
	var v struct {
		Rootless            bool
		Cgroups, Graph, Run string
	}
	if safefs.DecodeJSON(b, &v, 128<<10) != nil || !v.Rootless || v.Cgroups != "v2" || !managedPath(v.Graph) || !managedPath(v.Run) {
		return "", fmt.Errorf("Signal recovery requires a local rootless cgroup-v2 Podman store")
	}
	return protocol.Hash(v), nil
}

func (s PodmanSignalServiceSpec) image(ctx context.Context, run signalPodmanRun) error {
	b, err := signalPodmanCommand(ctx, run, "image", "inspect", "sha256:"+s.ImageID, "--format", `{"id":{{json .ID}},"os":{{json .Os}},"arch":{{json .Architecture}}}`)
	if err != nil {
		return err
	}
	var v struct{ ID, OS, Arch string }
	if safefs.DecodeJSON(b, &v, 4096) != nil || strings.TrimPrefix(v.ID, "sha256:") != s.ImageID || v.OS != "linux" || v.Arch != runtime.GOARCH {
		return fmt.Errorf("Signal local image differs from native architecture/content pin")
	}
	return nil
}

func (s PodmanSignalServiceSpec) container(ctx context.Context, run signalPodmanRun, active bool, managerPID uint32) (*signalPodmanContainer, error) {
	// Query the exact name even after --rm removed the enrolled ID. A replacement
	// with the same service name must never be silently treated as stopped.
	b, err := signalPodmanCommand(ctx, run, "ps", "--all", "--no-trunc", "--filter", "name=^"+podmanSignalName+"$", "--format", "{{.ID}}")
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		if active {
			return nil, fmt.Errorf("active Signal service has no enrolled container")
		}
		// A renamed enrolled container must not escape the name check.
		b, err = signalPodmanCommand(ctx, run, "ps", "--all", "--no-trunc", "--filter", "id="+s.ContainerID, "--format", "{{.ID}}")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(b)) != "" {
			return nil, fmt.Errorf("enrolled Signal container was renamed")
		}
		return nil, nil
	}
	if id != s.ContainerID {
		return nil, fmt.Errorf("Signal container identity changed")
	}
	b, err = signalPodmanCommand(ctx, run, "container", "inspect", id, "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var c signalPodmanContainer
	if err = decodeSignalPodman(b, &c); err != nil {
		return nil, err
	}
	if err = requireSignalPodmanFields(b); err != nil {
		return nil, err
	}
	if c.ID != s.ContainerID || c.Name != podmanSignalName || c.Image != s.ImageID || c.ImageName != "sha256:"+s.ImageID || c.Path != podmanSignalExecutable || !reflect.DeepEqual(c.Args, signalContainerArgv()[1:]) || c.Config.User != "0" || !reflect.DeepEqual(c.Config.Entrypoint, []string{podmanSignalExecutable}) || !reflect.DeepEqual(c.Config.Cmd, signalContainerArgv()[1:]) {
		return nil, fmt.Errorf("Signal container executable/image/configuration differs")
	}
	created := append([]string{}, c.Config.CreateCommand...)
	if len(created) == 0 || (created[0] != "podman" && created[0] != "/usr/bin/podman") {
		return nil, fmt.Errorf("Signal container creation command unavailable")
	}
	created[0] = "/usr/bin/podman"
	if err = s.checkStartArgv(created); err != nil {
		return nil, fmt.Errorf("Signal container creation differs from enrolled Quadlet: %w", err)
	}
	h := c.HostConfig
	if h.Privileged || !h.ReadonlyRootfs || h.UsernsMode != "" || h.PidMode != "private" || h.NetworkMode != s.Network || !h.AutoRemove || len(h.CapAdd) != 0 || len(h.Devices) != 0 || !reflect.DeepEqual(h.SecurityOpt, []string{"no-new-privileges"}) || h.RestartPolicy.Name != "no" || len(c.EffectiveCaps) != 0 || len(c.BoundingCaps) != 0 {
		return nil, fmt.Errorf("unsupported Signal container isolation")
	}
	if err = s.checkMounts(c); err != nil {
		return nil, err
	}
	if c.State.Paused || c.State.Restarting || c.State.Dead || c.State.Running != active {
		return nil, fmt.Errorf("Signal service and container state differ")
	}
	if !active {
		if c.State.Pid != 0 || (c.State.Status != "exited" && c.State.Status != "stopped") {
			return nil, fmt.Errorf("Signal container has not drained")
		}
		return &c, nil
	}
	if c.State.Status != "running" || c.State.Pid == 0 || !managedPath(c.ConmonPidFile) {
		return nil, fmt.Errorf("Signal container process identity unavailable")
	}
	b, err = nativeSignalRead(c.ConmonPidFile, 64, false)
	if err != nil || strings.TrimSpace(string(b)) != strconv.FormatUint(uint64(managerPID), 10) {
		return nil, fmt.Errorf("Signal manager does not supervise the enrolled conmon")
	}
	return &c, nil
}

func (s PodmanSignalServiceSpec) checkMounts(c signalPodmanContainer) error {
	want := map[string]signalPodmanMount{}
	for _, m := range s.mounts() {
		want[m.Destination] = m
	}
	seen := map[string]bool{}
	for _, m := range c.Mounts {
		if seen[m.Destination] {
			return fmt.Errorf("duplicate Signal container mount")
		}
		seen[m.Destination] = true
		if m.Type == "tmpfs" && m.Destination == "/tmp" && m.RW {
			continue
		}
		w, ok := want[m.Destination]
		if !ok || m.Type != "bind" || m.Source != w.Source || m.RW != w.RW || (m.Propagation != "rprivate" && m.Propagation != "private") {
			return fmt.Errorf("Signal container mount differs from enrolled paths/mode")
		}
		for _, o := range m.Options {
			if o != "rbind" && o != "bind" && o != "rw" && o != "ro" && o != "rprivate" && o != "private" && o != "nosuid" && o != "nodev" && o != "noexec" {
				return fmt.Errorf("unsupported Signal mount option")
			}
		}
		delete(want, m.Destination)
	}
	if len(want) != 0 {
		return fmt.Errorf("Signal account/inbox/configuration mount missing")
	}
	return nil
}

func (s PodmanSignalServiceSpec) proof(c signalPodmanContainer) signalPodmanProcessProof {
	return signalPodmanProcessProof{PID: c.State.Pid, ExecutableSHA256: s.ExecutableSHA256, Mounts: s.mounts(), Account: filepath.Join(s.AccountState, "deploy-account.lock"), Inbox: filepath.Join(s.Inbox, "inbox.lock")}
}
