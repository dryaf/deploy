package install

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/dryaf/deploy/internal/notification/provider"
)

func TestManagedSignalRejectsUnpinnedSocketPeer(t *testing.T) {
	s, i, _ := productionSignalFixture(t)
	m, err := i.setup(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	want, err := signalServiceHealth(m)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(s.Endpoint.Root, "api.sock")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(socket, 0600); err != nil {
		l.Close()
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _ = json.NewEncoder(w).Encode(want) })}
	done := make(chan error, 1)
	go func() { done <- server.Serve(l) }()
	defer func() { server.Close(); <-done }()
	gateway := ManagedManifest{SignalVerification: &ManagedSignalBinding{SignalVerificationReference: SignalVerificationReference{Root: s.Endpoint.Root, ManifestHash: s.Endpoint.ManifestHash, ServiceRoot: s.Root, ServiceHash: m.ManifestHash}}}
	if err = checkManagedSignalHealth(context.Background(), gateway); err == nil {
		t.Fatal("forged health from different executable accepted")
	}
	transport, closeTransport, err := managedSignalTransport(gateway, func(context.Context) error { return nil }, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer closeTransport()
	c, err := signalProductionConfig(m)
	if err != nil {
		t.Fatal(err)
	}
	result := (provider.ConversationAdapter{Signal: &want.Signal, SignalTransport: transport}).SendReply(context.Background(), c.Interactive.Destinations[0], "must never reach unpinned peer")
	if result.Accepted || calls.Load() != 0 {
		t.Fatal("request reached unpinned peer", result, calls.Load())
	}
}
