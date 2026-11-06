package notification

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/interaction"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/testutil"
)

type signalMigrationTransport func(*http.Request) (*http.Response, error)

func (f signalMigrationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestPinnedLegacySignalSender(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dest := interaction.Destination{ID: "private", Generation: "one", Provider: "signal", Account: "11111111-1111-4111-8111-111111111111", NativeID: "22222222-2222-4222-8222-222222222222"}
	p := provider.Profile{Kind: "signal", Conversation: &dest, Destination: dest.Fingerprint(), Socket: filepath.Join(dir, "relay.sock"), RelayIdentity: notify.Identity{Worker: testutil.ID(t, "w"), Recovery: testutil.ID(t, "r")}}
	policy := config.WorkerNotifications{Primary: "signal", Signal: &config.WorkerSignal{AccountFile: filepath.Join(dir, "account"), RecipientFile: filepath.Join(dir, "recipient")}}
	testutil.Must(t, os.WriteFile(policy.Signal.AccountFile, []byte(dest.Account), 0600))
	testutil.Must(t, os.WriteFile(policy.Signal.RecipientFile, []byte(dest.NativeID), 0600))
	pin, err := LegacyCredentialHash(policy, &p)
	testutil.Must(t, err)
	calls, status := 0, 200
	transport := signalMigrationTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		var payload struct {
			Identity                                notify.Identity
			Account, Destination, Fingerprint, Text string
		}
		testutil.Must(t, json.NewDecoder(r.Body).Decode(&payload))
		if r.URL.Path != "/reply" || payload.Identity != p.RelayIdentity || payload.Account != dest.Account || payload.Destination != dest.ID || payload.Fingerprint != p.Destination || payload.Text != "fixture" {
			t.Fatal("redirected legacy send", payload)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"accepted":true,"native_id":"123"}`)), Header: http.Header{}}, nil
	})
	sender := pinnedLegacySender(pin, &p, transport)
	result, err := sender(ctx, "signal", policy, NotificationEvent{ID: "original", Message: "fixture"})
	testutil.Must(t, err)
	if !result.Accepted || result.Ambiguous || calls != 1 {
		t.Fatal(result, calls)
	}
	status = 502
	result, err = sender(ctx, "signal", policy, NotificationEvent{ID: "uncertain", Message: "fixture"})
	if err == nil || result.Accepted || !result.Ambiguous {
		t.Fatal(result, err)
	}
	testutil.Must(t, os.WriteFile(policy.Signal.RecipientFile, []byte("33333333-3333-4333-8333-333333333333"), 0600))
	result, err = sender(ctx, "signal", policy, NotificationEvent{ID: "redirect", Message: "fixture"})
	if err == nil || result.Accepted || result.Ambiguous || calls != 2 {
		t.Fatal("changed recipient reached transport", result, err, calls)
	}
}
