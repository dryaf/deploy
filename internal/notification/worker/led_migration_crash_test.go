package worker

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestLEDMappingMigrationCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_LED_MAPPING_STATE")
	if path == "" {
		return
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var before, after Config
	must(t, d.ReadJSON("test-before.json", &before))
	must(t, d.ReadJSON("test-after.json", &after))
	api := &fakeLED{}
	must(t, d.ReadJSON("test-daemon.json", &api.states))
	save := func() { must(t, d.WriteJSON("test-daemon.json", api.states, true)) }
	api.onClear = func(string) { save() }
	api.onSet = func(led.State) { save() }
	phase, kind := os.Getenv("DEPLOY_LED_MAPPING_PHASE"), os.Getenv("DEPLOY_LED_MAPPING_KIND")
	_, err = migrateLEDBindings(context.Background(), path, before, after, migrationSocket, api, func(stage string) error {
		if stage != phase {
			return nil
		}
		if kind != "" {
			var intent ledMappingIntent
			must(t, d.ReadJSON("led-migration.json", &intent))
			db, err := snapshotDB(d, "state.db", true)
			must(t, err)
			record, err := readLEDMapping(context.Background(), db, intent.ID)
			must(t, err)
			must(t, db.Close())
			index := record.Next
			if stage == "mapping_rpc_ack" {
				index--
			}
			if record.Intent.Steps[index].Kind != kind {
				return nil
			}
		}
		fmt.Println("READY")
		select {}
	})
	must(t, err)
	t.Fatal("kill boundary not reached")
}

func TestLEDMappingMigrationSIGKILL(t *testing.T) {
	boundaries := []struct{ phase, kind string }{{"mapping_intent", ""}, {"mapping_prepared", ""}, {"mapping_before_commit", ""}, {"mapping_committed", ""}, {"mapping_unfenced", ""}}
	for _, phase := range []string{"mapping_rpc_intent", "mapping_rpc_returned", "mapping_rpc_ack"} {
		for _, kind := range []string{"clear", "set"} {
			boundaries = append(boundaries, struct{ phase, kind string }{phase, kind})
		}
	}
	for _, boundary := range boundaries {
		t.Run(boundary.phase+"/"+boundary.kind, func(t *testing.T) {
			path, before, after, api := mappingFixture(t)
			d, err := safefs.Open(path)
			must(t, err)
			defer d.Close()
			must(t, d.WriteJSON("test-before.json", before, false))
			must(t, d.WriteJSON("test-after.json", after, false))
			must(t, d.WriteJSON("test-daemon.json", api.states, false))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLEDMappingMigrationCrashChild$")
			cmd.Env = append(os.Environ(), "DEPLOY_LED_MAPPING_STATE="+path, "DEPLOY_LED_MAPPING_PHASE="+boundary.phase, "DEPLOY_LED_MAPPING_KIND="+boundary.kind)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			must(t, cmd.Start())
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			scanner := bufio.NewScanner(pipe)
			if !scanner.Scan() || scanner.Text() != "READY" {
				t.Fatalf("boundary not reached: %s %s", scanner.Text(), stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			if boundary.phase != "mapping_unfenced" {
				for _, c := range []Config{before, after} {
					if s, err := Open(path, c, false); err == nil {
						s.Close()
						t.Fatal("interrupted migration admitted worker")
					}
				}
			}
			api.states = nil
			must(t, d.ReadJSON("test-daemon.json", &api.states))
			api.onClear = func(id string) {
				if !strings.HasSuffix(id, ".baseline") && !strings.HasSuffix(id, ".healthy") {
					t.Error("migration cleared a fault incarnation")
				}
			}
			result, err := MigrateLEDBindings(context.Background(), path, before, after, migrationSocket, api)
			must(t, err)
			s, err := Open(path, after, false)
			must(t, err)
			defer s.Close()
			record := latestMapping(t, s)
			if !record.Completed || record.Intent.ID != result.Operation {
				t.Fatal("completion identity changed")
			}
			must(t, verifyLEDMappingOutput(context.Background(), api, record.Intent))
		})
	}
}
