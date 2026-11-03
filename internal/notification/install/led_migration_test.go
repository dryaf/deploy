package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func TestLEDMappingCLIDryRunAndScope(t *testing.T) {
	for _, change := range []string{"bindings", "socket", "provider", "enrollment", "limit"} {
		t.Run(change, func(t *testing.T) {
			c, before := runtimeFixture(t)
			c.LEDSocket = "/private/test-led.sock"
			writeRuntime(t, before, c)
			after := filepath.Join(t.TempDir(), "next.json")
			c.Worker.LEDBindings = []worker.LEDBinding{{ID: "selected", Producer: c.Worker.Producers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
			switch change {
			case "socket":
				c.LEDSocket += ".other"
			case "provider":
				p := c.Providers["telegram"]
				p.ChatFile = "/private/other-chat"
				c.Providers["telegram"] = p
			case "enrollment":
				c.Worker.Producers[0].Required = false
			case "limit":
				c.Worker.MaxDatabaseBytes *= 2
			}
			writeRuntime(t, after, c)
			state := filepath.Join(t.TempDir(), "absent-state")
			prior := ui.DryRun
			ui.DryRun = true
			defer func() { ui.DryRun = prior }()
			var out bytes.Buffer
			args := []string{"led-migrate", "--local", "--config", before, "--next-config", after, "--state-dir", state}
			err := runLEDMigrationCLI(args, &out)
			if change == "bindings" {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), "have not been inspected or changed") {
					t.Fatal(out.String())
				}
				if err = RunCLI(args); err != nil {
					t.Fatal("CLI dispatch missing", err)
				}
			} else if err == nil {
				t.Fatal("unrelated runtime change accepted")
			}
			if _, err = os.Stat(state); !os.IsNotExist(err) {
				t.Fatal("dry-run touched worker state", err)
			}
		})
	}
}

func TestLEDMappingCLIRequiresLocalAndStoppedWorker(t *testing.T) {
	c, before := runtimeFixture(t)
	c.LEDSocket = "/private/unused-led.sock"
	writeRuntime(t, before, c)
	after := filepath.Join(t.TempDir(), "next.json")
	c.Worker.LEDBindings = []worker.LEDBinding{{ID: "selected", Producer: c.Worker.Producers[0].Producer, Subjects: []string{"*"}, LED: "green"}}
	writeRuntime(t, after, c)
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	runtime, err := ReadRuntime(before)
	if err != nil {
		t.Fatal(err)
	}
	s, err := worker.Open(state, runtime.Worker, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	args := []string{"led-migrate", "--config", before, "--next-config", after, "--state-dir", state}
	var out bytes.Buffer
	if err = runLEDMigrationCLI(args, &out); err == nil {
		t.Fatal("missing --local accepted")
	}
	if err = runLEDMigrationCLI(append(args, "--local"), &out); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatal("live worker not protected by lease", err)
	}
	if _, err = os.Stat(filepath.Join(state, "led-migration.json")); !os.IsNotExist(err) {
		t.Fatal("live worker was fenced", err)
	}
}
