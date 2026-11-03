package install

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/safefs"
)

const podmanSignalName = "deploy-notification-signal"
const podmanSignalExecutable = "/usr/local/bin/deploy"

// This profile is for the interactive relay, not the legacy backup relay. Pins
// survive removal of the original container by Quadlet after a successful stop.
type PodmanSignalServiceSpec struct {
	Quadlet          string `json:"quadlet"`
	QuadletSHA256    string `json:"quadlet_sha256"`
	Fragment         string `json:"fragment"`
	FragmentSHA256   string `json:"fragment_sha256"`
	ImageID          string `json:"image_id"`
	ContainerID      string `json:"container_id"`
	ExecutableSHA256 string `json:"executable_sha256"`
	RelayConfig      string `json:"relay_config"`
	RelaySHA256      string `json:"relay_sha256"`
	AccountState     string `json:"account_state"`
	Inbox            string `json:"inbox"`
	SocketDirectory  string `json:"socket_directory"`
	AccountFile      string `json:"account_file"`
	RecipientFile    string `json:"recipient_file"`
	Network          string `json:"network"`
}

type signalPodmanMount struct {
	Source, Destination string
	RW                  bool
}

func (s PodmanSignalServiceSpec) mounts() []signalPodmanMount {
	return []signalPodmanMount{{s.AccountState, "/signal-state", true}, {s.Inbox, "/signal-inbox", true}, {s.SocketDirectory, "/relay", true}, {s.RelayConfig, "/relay-config.json", false}, {s.AccountFile, "/secrets/signal-account", false}, {s.RecipientFile, "/secrets/signal-recipient", false}}
}

func (s PodmanSignalServiceSpec) validate() error {
	if os.Getuid() == 0 {
		return fmt.Errorf("Signal Podman recovery requires a rootless user")
	}
	if s.Network != "none" && s.Network != "pasta" {
		return fmt.Errorf("Signal Podman network must be none or pasta")
	}
	for _, h := range []string{s.QuadletSHA256, s.FragmentSHA256, s.ImageID, s.ContainerID, s.ExecutableSHA256, s.RelaySHA256} {
		if !safefs.DigestPattern.MatchString(h) {
			return fmt.Errorf("Signal Podman recovery requires full SHA-256 pins")
		}
	}
	paths := []string{s.Quadlet, s.Fragment, s.AccountState, s.Inbox, s.SocketDirectory, s.RelayConfig, s.AccountFile, s.RecipientFile}
	for i, p := range paths {
		// Colons and systemd/PODMAN expansion characters are excluded from this
		// first profile; no shell or volume-option parser interprets user paths.
		if !managedPath(p) || strings.ContainsAny(p, ":%$ \t\"\\") {
			return fmt.Errorf("unsupported Signal Podman path")
		}
		for _, q := range paths[:i] {
			if managedWithin(p, q) || managedWithin(q, p) {
				return fmt.Errorf("Signal Podman inputs/state paths must be disjoint")
			}
		}
	}
	config, err := os.UserConfigDir()
	if err != nil || s.Quadlet != filepath.Join(config, "containers/systemd", podmanSignalName+".container") {
		return fmt.Errorf("Signal Quadlet is not in the current user's configuration directory")
	}
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
	}
	if !managedPath(runtime) {
		return fmt.Errorf("invalid user runtime directory")
	}
	for _, g := range []string{"generator", "generator.early", "generator.late"} {
		if s.Fragment == filepath.Join(runtime, "systemd", g, nativeSignalUnit) {
			return nil
		}
	}
	return fmt.Errorf("Signal service is not a user-generated Quadlet")
}

// RenderPodmanSignalRecoveryQuadlet documents the supported existing profile.
// It is pure rendering: recovery never writes or loads a unit or starts intake.
func RenderPodmanSignalRecoveryQuadlet(s PodmanSignalServiceSpec) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=deploy interactive Signal relay\n\n[Container]\nContainerName=" + podmanSignalName + "\nImage=sha256:" + s.ImageID + "\nGlobalArgs=--remote=false\nPull=never\nReadOnly=true\nNoNewPrivileges=true\nDropCapability=ALL\nUser=0\nEntrypoint=" + podmanSignalExecutable + "\n")
	for _, m := range s.mounts() {
		mode := "ro"
		if m.RW {
			mode = "rw"
		}
		b.WriteString("Volume=" + (m.Source + ":" + m.Destination + ":" + mode) + "\n")
	}
	b.WriteString("Exec=notification-worker __signal-relay --local --config /relay-config.json\nTmpfs=/tmp:rw,size=256m,nosuid,nodev\nNetwork=" + s.Network + "\nPodmanArgs=--image-volume=ignore\n\n[Service]\nRestart=on-failure\nTimeoutStopSec=30s\n")
	return b.String()
}

func (s PodmanSignalServiceSpec) files() (relay.Config, string, error) {
	var c relay.Config
	if err := s.validate(); err != nil {
		return c, "", err
	}
	for _, v := range []struct {
		path, hash string
		private    bool
	}{{s.Quadlet, s.QuadletSHA256, false}, {s.Fragment, s.FragmentSHA256, false}, {s.RelayConfig, s.RelaySHA256, true}} {
		b, err := nativeSignalRead(v.path, 256<<10, v.private)
		if err != nil || managedDigest(b) != v.hash {
			return c, "", fmt.Errorf("Signal Podman pinned file missing, unsafe or changed")
		}
		if v.path == s.Quadlet && string(b) != RenderPodmanSignalRecoveryQuadlet(s) {
			return c, "", fmt.Errorf("Signal Quadlet does not match the supported recovery profile")
		}
		if v.path == s.RelayConfig && safefs.DecodeJSON(b, &c, 256<<10) != nil {
			return c, "", fmt.Errorf("invalid Signal container configuration")
		}
	}
	if c.StateDir != "/signal-state" || c.Socket != "/relay/signal.sock" || c.AccountFile != "/secrets/signal-account" || c.RecipientFile != "/secrets/signal-recipient" || c.Interactive == nil || c.Interactive.Validate() != nil || c.Interactive.InboxDir != "/signal-inbox" {
		return c, "", fmt.Errorf("Signal relay configuration differs from container mounts")
	}
	a, err := nativeSignalRead(s.AccountFile, 4096, true)
	if err != nil || len(a) == 0 {
		return c, "", fmt.Errorf("Signal account credential unavailable")
	}
	r, err := nativeSignalRead(s.RecipientFile, 4096, true)
	if err != nil || len(r) == 0 {
		return c, "", fmt.Errorf("Signal recipient credential unavailable")
	}
	return c, protocol.Hash(struct {
		Spec               PodmanSignalServiceSpec
		Account, Recipient string
	}{s, managedDigest(a), managedDigest(r)}), nil
}

func signalContainerArgv() []string {
	return []string{podmanSignalExecutable, "notification-worker", "__signal-relay", "--local", "--config", "/relay-config.json"}
}

// Quadlet 5.7 profile. A different generator command layout is refused rather
// than accepting unrecognized arguments with possible filesystem side effects.
func (s PodmanSignalServiceSpec) startArgv() []string {
	a := []string{"/usr/bin/podman", "--remote=false", "run", "--name", podmanSignalName, "--replace", "--rm", "--cgroups=split", "--entrypoint", podmanSignalExecutable, "--pull", "never", "--tmpfs", "/tmp:rw,size=256m,nosuid,nodev", "--network", s.Network, "--sdnotify=conmon", "-d", "--security-opt=no-new-privileges", "--cap-drop", "all", "--read-only", "--user", "0"}
	for _, m := range s.mounts() {
		mode := "ro"
		if m.RW {
			mode = "rw"
		}
		a = append(a, "-v", m.Source+":"+m.Destination+":"+mode)
	}
	a = append(a, "--image-volume=ignore", "sha256:"+s.ImageID)
	return append(a, signalContainerArgv()[1:]...)
}

// Quadlet emits some independent options from Go maps; their order can differ
// even with identical input. Only these exact, nonoverlapping option/value pairs
// may commute. Unknown options, duplicate flags and changed mounts are rejected.
func (s PodmanSignalServiceSpec) checkStartArgv(got []string) error {
	want := s.startArgv()
	canonical := func(a []string) ([]string, error) {
		if len(a) < 10 || !reflect.DeepEqual(a[:3], want[:3]) || !reflect.DeepEqual(a[len(a)-6:], want[len(want)-6:]) {
			return nil, fmt.Errorf("Signal Podman command prefix/image/relay arguments differ")
		}
		var tokens []string
		seen := map[string]bool{}
		for i := 3; i < len(a)-6; i++ {
			key := a[i]
			switch key {
			case "--replace", "--rm", "--cgroups=split", "--sdnotify=conmon", "-d", "--security-opt=no-new-privileges", "--read-only", "--image-volume=ignore":
			case "--name", "--entrypoint", "--pull", "--tmpfs", "--network", "--cap-drop", "--user", "-v":
				i++
				if i >= len(a)-6 {
					return nil, fmt.Errorf("missing Signal Podman option value")
				}
				key += "=" + a[i]
			default:
				return nil, fmt.Errorf("unsupported Signal Podman start option")
			}
			if seen[key] {
				return nil, fmt.Errorf("duplicate Signal Podman option")
			}
			seen[key] = true
			tokens = append(tokens, key)
		}
		sort.Strings(tokens)
		return tokens, nil
	}
	a, err := canonical(got)
	if err != nil {
		return err
	}
	b, err := canonical(want)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("Signal Podman loaded start command differs")
	}
	return nil
}
