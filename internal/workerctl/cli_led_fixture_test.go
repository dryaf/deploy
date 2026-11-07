//go:build linux

package workerctl

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
)

// Only used within the read-only, network-isolated CLI namespace. This registry
// adapter has no hardware driver and cannot constitute physical qualification.
func startBackupCLILED(t *testing.T, path string) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		foreign := led.Entry{State: led.State{ID: "foreign-fault", LED: "green", Pattern: "blink-fast", Priority: 100, Persistent: true}, OwnerUID: os.Getuid(), Winner: true}
		states := map[string]led.Entry{foreign.ID: foreign}
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				var r struct {
					Version   int        `json:"v"`
					Operation string     `json:"op"`
					State     *led.State `json:"state"`
					ID        string     `json:"id"`
					Offset    int        `json:"offset"`
					Limit     int        `json:"limit"`
				}
				raw, err := bufio.NewReaderSize(conn, 4096).ReadSlice('\n')
				if err != nil || json.Unmarshal(raw, &r) != nil {
					return
				}
				var result any = map[string]any{}
				ok := true
				switch r.Operation {
				case "system.ping":
				case "status.get":
					patterns := map[string]any{}
					for _, name := range []string{"blink-slow", "blink-fast", "heartbeat", "blink-1", "blink-2"} {
						patterns[name] = []any{}
					}
					result = map[string]any{"daemon": "pistatusd", "version": "0.1.0", "degraded": false, "patterns": patterns, "leds": map[string]any{"green": map[string]any{"device": "ACT", "enabled": true, "available": true, "default_pattern": "off", "selected_state": foreign}}}
				case "state.set":
					if r.State == nil || r.State.ID == foreign.ID {
						ok = false
					} else {
						states[r.State.ID] = led.Entry{State: *r.State, OwnerUID: os.Getuid()}
					}
				case "state.clear":
					if r.ID == foreign.ID {
						ok = false
					} else {
						delete(states, r.ID)
					}
				case "state.list":
					keys := make([]string, 0, len(states))
					for key := range states {
						keys = append(keys, key)
					}
					sort.Strings(keys)
					entries := []led.Entry{}
					end := min(len(keys), r.Offset+r.Limit)
					for i := r.Offset; i < end; i++ {
						entries = append(entries, states[keys[i]])
					}
					var next *int
					if end < len(keys) {
						next = &end
					}
					result = map[string]any{"states": entries, "total": len(keys), "next_offset": next}
				default:
					ok = false
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{"v": 1, "ok": ok, "result": result})
			}()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
}
