package workerctl

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestSignalQualificationPinsArtifactWithoutTransferringPrivateInputs(t *testing.T) {
	for _, source := range []string{"target", "catalog", "missing", "invalid"} {
		t.Run(source, func(t *testing.T) {
			r, _ := bootstrapControlFixture(t)
			digest := inputDigest([]byte("retained native CLI"))
			signal := config.ProviderDeclaration{AccountFile: "~/.config/private/account", RecipientFile: "~/.config/private/recipient"}
			artifacts := &ResolvedArtifacts{}
			switch source {
			case "target":
				signal.Executable, signal.ExecutableSHA256 = "~/.local/bin/signal-cli", digest
			case "catalog":
				artifacts.Signal = &Artifact{File: "/not-read-or-transferred/native", SHA256: digest}
			case "invalid":
				signal.Executable, signal.ExecutableSHA256 = "~/.local/bin/signal-cli", "invalid"
			}
			r.Desired.Notification.Providers = config.ProviderDeclarations{"signal": signal}
			r.Desired.Notification.Routes = map[string]config.NotificationRoute{"alerts": {Primary: "signal"}}
			r.DesiredHash, _ = r.Desired.Fingerprint()
			inv := config.ServerInventory{Servers: map[string]config.ServerConfig{r.Server: {Desired: &r.Desired}}}
			session := install.QualificationSession{ID: testutil.ID(t, "n"), Checks: []install.QualificationCheckSummary{{Scope: "provider/signal", ID: "destination-receipt", Result: "pending"}}}
			calls := 0
			var out, errout bytes.Buffer
			cli := CLI{Out: &out, Err: &errout, Prompt: StreamPrompt{Input: bufio.NewReader(strings.NewReader("pending\n"))}, Controller: qualificationController(func(got ControlRequest) (ControlResult, error) {
				calls++
				if got.SignalSHA256 != digest || got.LocalSignal != nil || len(got.Inputs) != 0 || got.InteractiveLink {
					t.Fatal("qualification changed native authority or carried credentials")
				}
				if got.Action == "qualify-commit" {
					session.Recorded = true
				}
				return ControlResult{Version: 1, Code: "ok", QualificationSession: &session}, nil
			})}
			err := cli.qualifyNotification(context.Background(), Invocation{Server: r.Server, Options: Options{JSON: true}}, inv, r.Facts, Plan{DesiredHash: r.DesiredHash, Artifacts: artifacts})
			if source == "missing" || source == "invalid" {
				if err == nil || calls != 0 {
					t.Fatal("missing native pin reached target", err, calls)
				}
				return
			}
			testutil.Must(t, err)
			if calls != 2 || !safefs.ValidID(session.ID, "n") {
				t.Fatal("unexpected qualification flow", calls)
			}
		})
	}
}
