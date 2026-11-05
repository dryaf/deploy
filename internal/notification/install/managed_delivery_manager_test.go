package install

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func deliveryManagerProperties(m ManagedManifest, pid uint32) map[string]string {
	p := map[string]string{"LoadState": "loaded", "ActiveState": "inactive", "SubState": "dead", "FragmentPath": filepath.Join(m.UnitDirectory, managedUnit), "Names": managedUnit, "UnitFileState": "enabled", "MainPID": "0", "NeedDaemonReload": "no", "Type": "simple", "ProtectSystem": "strict", "ProtectHome": "read-only", "NoNewPrivileges": "yes", "PrivateUsers": "yes", "PrivateDevices": "yes", "ReadWritePaths": m.Paths.State, "CapabilityBoundingSet": "", "AmbientCapabilities": "", "RestrictAddressFamilies": "AF_UNIX"}
	for _, k := range signalServiceEmptyProperties {
		p[k] = ""
	}
	p["ControlPID"] = "0"
	for k, v := range signalServiceFixedProperties {
		p[k] = v
	}
	if m.Mode == "delivery" {
		p["RestrictAddressFamilies"] = "AF_INET AF_INET6 AF_UNIX"
	}
	if pid > 0 {
		p["ActiveState"] = "active"
		p["SubState"] = "running"
		p["MainPID"] = strconv.FormatUint(uint64(pid), 10)
	}
	return p
}

func TestManagedDeliveryManagerTransition(t *testing.T) {
	before := ManagedManifest{Root: "/private/install", UnitDirectory: "/private/units", Revision: "n-01234567890123456789012345678901", Mode: "verification", DeliveryOwnerHash: protocol.Hash("owner"), Paths: ServicePaths{Executable: "/private/install/releases/old/deploy", State: "/private/state"}}
	after := before
	after.Revision = "n-01234567890123456789012345678902"
	after.Paths.Executable = "/private/install/releases/new/deploy"
	foreign := after
	foreign.Paths.Executable = "/foreign/deploy"
	for _, loaded := range []ManagedManifest{before, after, foreign} {
		t.Run(loaded.Paths.Executable, func(t *testing.T) {
			inspector := userDeliveryInspector{run: func(_ context.Context, program string, _ ...string) ([]byte, error) {
				if program == "busctl" {
					return deliveryExecOutput(loaded), nil
				}
				p := deliveryManagerProperties(loaded, 0)
				p["NeedDaemonReload"] = "yes"
				return nativeSignalManagerOutput(p), nil
			}}
			i := managedInstaller{deliveryInspect: func(ctx context.Context, m ManagedManifest, stale bool) error {
				_, err := inspector.inspect(ctx, m, stale)
				return err
			}}
			err := i.inspectDeliveryTransition(context.Background(), managedJournal{Intent: managedIntent{Before: &before, After: after}})
			if (err == nil) != (loaded.Paths.Executable != foreign.Paths.Executable) {
				t.Fatal("transition accepted an unrelated command or refused a retained revision", err)
			}
			if _, err := inspector.inspect(context.Background(), after, false); err == nil {
				t.Fatal("readiness admitted a stale loaded unit")
			}
		})
	}
}

func deliveryExecOutput(m ManagedManifest) []byte {
	raw, _ := json.Marshal(map[string]any{"type": "a(sasasttttuii)", "data": [][]any{{m.Paths.Executable, managedGatewayArgs(m), []string{}, 0, 0, 0, 0, 0, 0, 0}}})
	return raw
}

func TestManagedDeliveryManagerProfile(t *testing.T) {
	m := ManagedManifest{Root: "/private/install", UnitDirectory: "/private/units", Revision: "n-01234567890123456789012345678901", Mode: "delivery", Paths: ServicePaths{Executable: "/private/install/deploy", State: "/private/state"}}
	for _, kind := range []string{"valid", "environment", "drop-in", "alias", "network", "stop-budget", "readwrite", "type", "extra", "missing", "command", "reload", "changing"} {
		t.Run(kind, func(t *testing.T) {
			p := deliveryManagerProperties(m, 0)
			switch kind {
			case "environment":
				p["Environment"] = "TOKEN=private"
			case "drop-in":
				p["DropInPaths"] = "/foreign/drop-in"
			case "alias":
				p["Names"] += " other.service"
			case "network":
				p["RestrictAddressFamilies"] = "AF_UNIX AF_INET AF_INET6 AF_NETLINK"
			case "stop-budget":
				p["TimeoutStopUSec"] = "infinity"
			case "readwrite":
				p["ReadWritePaths"] += " /foreign"
			case "type":
				p["Type"] = "forking"
			case "extra":
				p["Unexpected"] = "1"
			case "missing":
				delete(p, "Delegate")
			case "reload":
				p["NeedDaemonReload"] = "yes"
			}
			shows := 0
			u := userDeliveryInspector{run: func(_ context.Context, program string, args ...string) ([]byte, error) {
				if program == "systemctl" {
					shows++
					if kind == "changing" && shows > 1 {
						p["UnitFileState"] = "disabled"
					}
					if len(args) != 6 || args[3] != managedUnit || !strings.HasPrefix(args[5], "--property=") {
						t.Fatal(args)
					}
					return nativeSignalManagerOutput(p), nil
				}
				if program == "busctl" {
					if kind == "command" {
						return []byte(`{}`), nil
					}
					return deliveryExecOutput(m), nil
				}
				return nil, errors.New("unexpected command")
			}}
			_, err := u.inspect(context.Background(), m, false)
			if (err == nil) != (kind == "valid") {
				t.Fatal(kind, err)
			}
			if kind == "reload" {
				if _, err := u.inspect(context.Background(), m, true); err != nil {
					t.Fatal("owned stale reload prevented deactivation", err)
				}
			}
		})
	}
}
