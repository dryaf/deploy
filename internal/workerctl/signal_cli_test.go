package workerctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestShortSignalCLIReachesControllerWithDeclaredNativePins(t *testing.T) {
	dir := t.TempDir()
	helper := elfFixture(t, "arm64")
	raw, err := os.ReadFile(helper.File)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(dir, "deploy"), raw, 0700))
	helper.File = "deploy"
	catalog := Catalog{Version: 1, Release: "fixture", Linux: map[string]ArchitectureArtifacts{"arm64": {Helper: helper}}}
	raw, err = json.Marshal(catalog)
	testutil.Must(t, err)
	catalogFile := filepath.Join(dir, "catalog.json")
	testutil.Must(t, os.WriteFile(catalogFile, raw, 0600))
	declared := strings.Replace(cliInventory, "providers: [telegram]", `providers:
        signal:
          account_file: ~/.config/private/account
          recipient_file: ~/.config/private/recipient
          executable: ~/.local/bin/signal-cli
          executable_sha256: `+strings.Repeat("a", 64), 1)
	inventory := filepath.Join(dir, "server.yaml")
	testutil.Must(t, os.WriteFile(inventory, []byte(declared), 0600))
	calls := 0
	controller := qualificationController(func(r ControlRequest) (ControlResult, error) {
		calls++
		if r.SignalSHA256 != strings.Repeat("a", 64) || r.LocalSignal != nil || len(r.Inputs) != 0 || r.InteractiveLink {
			t.Fatal("target native/credential references lost or read locally")
		}
		return ControlResult{Version: 1, State: "needs_action", Code: "signal_link_required", Next: "deploy notification-worker resume manager"}, nil
	})
	var output bytes.Buffer
	c := CLI{Inspector: &countingInspector{}, Controller: controller, Out: &output}
	for _, args := range [][]string{{"manager"}, {"resume", "manager"}} {
		output.Reset()
		testutil.Must(t, c.Run(context.Background(), "notification-worker", args, Options{Inventory: inventory, Catalog: catalogFile, JSON: true, NonInteractive: true}))
		var result ControlResult
		testutil.Must(t, json.Unmarshal(output.Bytes(), &result))
		if result.Code != "signal_link_required" {
			t.Fatal("private interaction requirement lost", output.String())
		}
	}
	if calls != 2 {
		t.Fatal("short Signal blocked before target coordinator", calls)
	}
}

func TestSignalReviewRequiresExplicitAnswerAndNeverLoops(t *testing.T) {
	for _, choice := range []string{"yes", "no", "json", "unattended", "eof"} {
		t.Run(choice, func(t *testing.T) {
			r, _ := bootstrapControlFixture(t)
			r.Desired.Notification.Providers = config.ProviderDeclarations{"signal": {}}
			op := testutil.ID(t, "n")
			calls := 0
			c := CLI{Controller: qualificationController(func(request ControlRequest) (ControlResult, error) {
				calls++
				if calls > 2 {
					t.Fatal("review prompt retried indefinitely")
				}
				if calls == 2 && request.SignalReview != op {
					t.Fatal("review lost exact attempt")
				}
				return ControlResult{Version: 1, State: "needs_action", Code: "signal_link_review_required", SignalReview: op}, nil
			})}
			answer := choice + "\n"
			if choice == "eof" {
				answer = ""
			}
			var promptOutput bytes.Buffer
			c.Prompt = StreamPrompt{Input: bufio.NewReader(strings.NewReader(answer)), Output: &promptOutput}
			_, err := c.applyNotificationRequest(context.Background(), config.ServerConfig{}, Artifact{}, r, Options{JSON: choice == "json", NonInteractive: choice == "unattended"})
			want := 1
			if choice == "yes" {
				want = 2
			}
			if calls != want {
				t.Fatal("wrong review count", calls)
			}
			if (choice == "no" || choice == "eof") && err == nil {
				t.Fatal("declined/cancelled review hidden")
			}
			if (choice == "json" || choice == "unattended") && promptOutput.Len() != 0 {
				t.Fatal("unattended review prompted")
			}
		})
	}
}
