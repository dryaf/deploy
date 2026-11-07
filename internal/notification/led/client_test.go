package led

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestManagedRemovalRPCBindsExactPlan(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for _, kind := range []string{"ok", "changed", "busy"} {
		t.Run(kind, func(t *testing.T) {
			requestSeen := make(chan request, 1)
			c := Client{Budget: time.Second, dial: func(context.Context) (net.Conn, error) {
				client, server := net.Pipe()
				go func() {
					defer server.Close()
					raw, _ := bufio.NewReader(server).ReadBytes('\n')
					var r request
					_ = json.Unmarshal(raw, &r)
					requestSeen <- r
					if kind == "busy" {
						_, _ = server.Write([]byte("{\"v\":1,\"ok\":false,\"error\":{\"code\":\"states_remaining\",\"message\":\"retained obligations\"}}\n"))
						return
					}
					actual := hash
					if kind == "changed" {
						actual = strings.Repeat("b", 64)
					}
					_ = json.NewEncoder(server).Encode(map[string]any{"v": 1, "ok": true, "result": map[string]any{"version": 1, "plan_hash": actual}})
				}()
				return client, nil
			}}
			err := c.PrepareRemoval(context.Background(), hash)
			if (err == nil) != (kind == "ok") {
				t.Fatal(kind, err)
			}
			r := <-requestSeen
			if r.Version != 1 || r.Operation != "deploy.prepare-removal" || r.PlanHash != hash {
				t.Fatal("removal plan not transmitted exactly", r)
			}
		})
	}
}

func TestHealthyDeadlineClamping(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		remaining time.Duration
		want      int
	}{{5 * time.Second, 2}, {2500 * time.Millisecond, 0}, {time.Minute, 57}, {time.Hour, 90}, {-time.Second, 0}} {
		if got := HealthyTTL(now, now.Add(tt.remaining), 2*time.Second, 500*time.Millisecond); got != tt.want {
			t.Errorf("%s: got %d want %d", tt.remaining, got, tt.want)
		}
	}
}
