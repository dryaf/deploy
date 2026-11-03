package install

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func TestManagedSignalHealthPins(t *testing.T) {
	spec, _, _, p, _ := managedSignalFixture(t)
	m := ManagedManifest{SignalVerification: &ManagedSignalBinding{SignalVerificationReference: *spec.SignalVerification}}
	raw, err := p.gateway()
	if err != nil {
		t.Fatal(err)
	}
	c, err := decodeRuntime(raw)
	if err != nil {
		t.Fatal(err)
	}
	good := SignalVerificationHealth{Version: 1, VerificationOnly: true, ManifestHash: p.ManifestHash, InstallationHash: p.Installation.InstallationHash, RecoveryOperation: p.Spec.Recovery.Operation, CommitHash: p.Spec.CommitHash, Signal: *c.Signal}
	for _, tc := range []struct {
		name   string
		change func(*SignalVerificationHealth)
		status int
		want   bool
	}{
		{"exact", nil, 200, true},
		{"busy_then_exact", nil, 503, true},
		{"manifest", func(h *SignalVerificationHealth) { h.ManifestHash = protocol.Hash("wrong") }, 200, false},
		{"installation", func(h *SignalVerificationHealth) { h.InstallationHash = protocol.Hash("wrong") }, 200, false},
		{"identity", func(h *SignalVerificationHealth) { h.Signal.InboxIdentity.Recovery = "other" }, 200, false},
		{"intake", func(h *SignalVerificationHealth) { h.IntakeEnabled = true }, 200, false},
		{"runtime", func(h *SignalVerificationHealth) { h.VerificationOnly = false }, 200, false},
		{"commit", func(h *SignalVerificationHealth) { h.CommitHash = protocol.Hash("wrong") }, 200, false},
		{"redirect", nil, 302, false},
		{"not_found", nil, 404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := good
			if tc.change != nil {
				tc.change(&h)
			}
			socket := filepath.Join(p.Spec.Root, "api.sock")
			l, e := net.Listen("unix", socket)
			if e != nil {
				t.Fatal(e)
			}
			defer l.Close()
			if e = os.Chmod(socket, 0600); e != nil {
				t.Fatal(e)
			}
			var calls atomic.Int32
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/health" || r.Method != "GET" {
					t.Error("unexpected request")
				}
				n := calls.Add(1)
				status := tc.status
				if status == 503 && n > 1 {
					status = 200
				}
				if status == 302 {
					w.Header().Set("Location", "http://elsewhere.invalid/health")
				}
				w.WriteHeader(status)
				_ = json.NewEncoder(w).Encode(h)
			})}
			done := make(chan error, 1)
			go func() { done <- server.Serve(l) }()
			defer func() { server.Close(); <-done }()
			e = checkManagedSignalHealth(context.Background(), m)
			if (e == nil) != tc.want {
				t.Fatal("unexpected health result", e)
			}
		})
	}
	if err = checkManagedSignalHealth(context.Background(), m); err == nil {
		t.Fatal("missing endpoint accepted")
	}
}

func TestManagedSignalHealthReferenceRequired(t *testing.T) {
	err := checkManagedSignalHealth(context.Background(), ManagedManifest{SignalVerification: &ManagedSignalBinding{SignalVerificationReference: SignalVerificationReference{Root: "relative", ManifestHash: fmt.Sprint(1)}}})
	if err == nil {
		t.Fatal("invalid reference accepted")
	}
}
