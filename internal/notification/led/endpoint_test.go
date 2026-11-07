package led

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func endpointFixture(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	dir, err := os.MkdirTemp("", "led-endpoint-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	if err = os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			line, err := bufio.NewReader(conn).ReadBytes('\n')
			if err == nil {
				calls.Add(1)
				var r request
				_ = json.Unmarshal(line, &r)
				result := `{}`
				switch r.Operation {
				case "status.get":
					result = `{"degraded":false,"leds":{"green":{"enabled":true,"available":true,"default_pattern":"off"}}}`
				case "state.list":
					result = `{"states":[],"total":0,"next_offset":null}`
				}
				_, _ = conn.Write([]byte(`{"v":1,"ok":true,"result":` + result + "}\n"))
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done; os.RemoveAll(dir) })
	return socket, &calls
}

func TestEndpointInspectionAndPinnedClient(t *testing.T) {
	socket, calls := endpointFixture(t)
	p, status, err := InspectEndpoint(context.Background(), socket)
	if err != nil || p.PeerUID != os.Getuid() || p.OwnerUID != os.Getuid() || status.Degraded || calls.Load() != 3 {
		t.Fatal(p, status, calls.Load(), err)
	}
	if err = (Client{Socket: socket, Budget: time.Second, Endpoint: &p}).Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"peer", "owner", "socket"} {
		bad := p
		switch kind {
		case "peer":
			bad.PeerUID++
		case "owner":
			bad.OwnerUID++
		case "socket":
			bad.Socket += ".other"
		}
		before := calls.Load()
		if err = (Client{Socket: socket, Budget: time.Second, Endpoint: &bad}).Set(context.Background(), State{ID: "test", LED: "green", Pattern: "off"}); err == nil {
			t.Fatal("changed endpoint accepted", kind)
		}
		if calls.Load() != before {
			t.Fatal("unauthenticated request sent", kind)
		}
	}
}

func TestEndpointRefusesUnsafePaths(t *testing.T) {
	socket, _ := endpointFixture(t)
	link := filepath.Join(filepath.Dir(socket), "link.sock")
	if err := os.Symlink(socket, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InspectEndpoint(context.Background(), link); err == nil {
		t.Fatal("followed socket symlink")
	}
	if err := os.Chmod(socket, 0666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InspectEndpoint(context.Background(), socket); err == nil {
		t.Fatal("accepted world-writable socket")
	}
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(socket), 0777); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InspectEndpoint(context.Background(), socket); err == nil {
		t.Fatal("accepted replaceable parent")
	}
	if err := os.Chmod(filepath.Dir(socket), 0700); err != nil {
		t.Fatal(err)
	}
}
