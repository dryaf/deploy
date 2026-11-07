package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
)

func TestBackupManagerLayoutUsesManagerFacts(t *testing.T) {
	p, err := ResolveInstallationPaths("/home/pi", "/home/pi/config home", "", "", config.WorkerPaths{UnitDirectory: "~/units with spaces"})
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"match", "unit-missing", "xdg-missing", "xdg-changed", "quadlet-excluded", "quadlet-included", "duplicate-env", "invalid-json", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			run := func(_ context.Context, program string, args ...string) ([]byte, error) {
				calls++
				if program != "busctl" || !strings.Contains(strings.Join(args, " "), "get-property org.freedesktop.systemd1 /org/freedesktop/systemd1 org.freedesktop.systemd1.Manager") {
					t.Fatal("unexpected manager command", program, args)
				}
				if scenario == "unavailable" {
					return nil, fmt.Errorf("private response must not escape")
				}
				if scenario == "invalid-json" {
					return []byte(`{"type":"s","data":"wrong type"}`), nil
				}
				values := []string{p.UnitDirectory, "/usr/lib/systemd/user"}
				if args[len(args)-1] == "UnitPath" {
					if scenario == "unit-missing" {
						values = []string{p.UnitDirectory + "/other"}
					}
				} else {
					values = []string{"XDG_CONFIG_HOME=" + p.ConfigHome, "PRIVATE_FIXTURE=must-not-escape"}
					switch scenario {
					case "xdg-missing":
						values = []string{}
					case "xdg-changed":
						values[0] = "XDG_CONFIG_HOME=/other/config"
					case "quadlet-excluded":
						values = append(values, "QUADLET_UNIT_DIRS=/other/quadlets")
					case "quadlet-included":
						values = append(values, "QUADLET_UNIT_DIRS=/other/quadlets:"+p.QuadletDirectory)
					case "duplicate-env":
						values = append(values, "XDG_CONFIG_HOME="+p.ConfigHome)
					}
				}
				return json.Marshal(map[string]any{"type": "as", "data": values})
			}
			err := requireBackupManagerLayout(context.Background(), p, run)
			if scenario == "match" || scenario == "quadlet-included" {
				if err != nil || calls != 2 {
					t.Fatal(err, calls)
				}
			} else if err == nil {
				t.Fatal("accepted differing manager layout")
			}
			if err != nil && (strings.Contains(err.Error(), "must-not-escape") || strings.Contains(err.Error(), "private response")) {
				t.Fatal("exposed manager environment", err)
			}
		})
	}
}
