package workerctl

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDiagnosticProviderSelectionParsingAndOfflinePlan(t *testing.T) {
	for _, args := range [][]string{{"test", "manager", "--provider", "signal"}, {"--provider=signal", "test", "manager"}} {
		v, err := Parse(args, Options{})
		if err != nil || v.Operation != "test" || v.Options.TestProvider != "signal" || !Matches(args) {
			t.Fatal(v, err)
		}
	}
	for _, args := range [][]string{{"apply", "manager", "--provider", "signal"}, {"test", "manager", "--provider"}, {"test", "manager", "--provider="}, {"test", "manager", "--provider=../signal"}, {"test", "manager", "--provider=signal", "--provider=telegram"}} {
		if _, err := Parse(args, Options{}); err == nil {
			t.Fatal("invalid diagnostic selection accepted", args)
		}
	}
	file := filepath.Join(t.TempDir(), "server.yaml")
	testutil.Must(t, os.WriteFile(file, []byte(cliInventory), 0600))
	var out bytes.Buffer
	cli := CLI{Out: &out}
	err := cli.Run(context.Background(), "notification-worker", []string{"test", "manager", "--provider", "telegram", "--servers", file, "--dry-run", "--json"}, Options{})
	testutil.Must(t, err)
	if !strings.Contains(out.String(), "one explicit diagnostic for telegram") {
		t.Fatal("offline plan dropped selected destination", out.String())
	}
	if err = cli.Run(context.Background(), "notification-worker", []string{"test", "manager", "--provider", "unknown", "--servers", file, "--dry-run"}, Options{}); err == nil {
		t.Fatal("unknown provider passed preview")
	}
}

func TestSelectedDiagnosticChecksDesiredProviderAndResponse(t *testing.T) {
	r, paths, _ := notificationOutputFixture(t)
	calls := 0
	r.DiagnosticProvider = "unknown"
	send := func(context.Context, string, string) (install.DiagnosticResult, error) {
		calls++
		return install.DiagnosticResult{Status: "provider_accepted", Provider: "signal"}, nil
	}
	if _, err := testNotificationControl(context.Background(), r, paths, send); err == nil || calls != 0 {
		t.Fatal("undeclared destination reached send", err, calls)
	}
	r.DiagnosticProvider = "telegram"
	if _, err := testNotificationControl(context.Background(), r, paths, send); err == nil || calls != 1 {
		t.Fatal("wrong-provider result was accepted or retried", err, calls)
	}
}

func TestSelectedDiagnosticCLIForwardsOnlyOperatorSelection(t *testing.T) {
	artifact := elfFixture(t, "arm64")
	dir := filepath.Dir(artifact.File)
	artifact.File = filepath.Base(artifact.File)
	catalog, err := json.Marshal(Catalog{Version: 1, Release: "fixture", Linux: map[string]ArchitectureArtifacts{"arm64": {Helper: artifact}}})
	testutil.Must(t, err)
	file := filepath.Join(dir, "catalog.json")
	testutil.Must(t, os.WriteFile(file, catalog, 0600))
	inventory := filepath.Join(dir, "server.yaml")
	testutil.Must(t, os.WriteFile(inventory, []byte(cliInventory), 0600))
	var out bytes.Buffer
	calls := 0
	cli := CLI{Out: &out, Inspector: backupTestInspector{}, Controller: qualificationController(func(r ControlRequest) (ControlResult, error) {
		calls++
		if r.Action != "test" || r.DiagnosticProvider != "telegram" || len(r.Inputs) != 0 || r.Observation != nil {
			t.Fatal("diagnostic selection changed operation or carried credentials", r.Action)
		}
		return ControlResult{Version: 1, State: "delivery_unknown", Code: "delivery_unknown", Diagnostic: &install.DiagnosticResult{Provider: "telegram", Status: "delivery_unknown"}}, nil
	})}
	testutil.Must(t, cli.Run(context.Background(), "notification-worker", []string{"test", "manager", "--provider=telegram", "--json", "--non-interactive", "--servers", inventory}, Options{Catalog: file}))
	var got ControlResult
	testutil.Must(t, json.Unmarshal(out.Bytes(), &got))
	if calls != 1 || got.State != "delivery_unknown" || got.Diagnostic == nil || got.Diagnostic.Provider != "telegram" || got.Diagnostic.Qualified {
		t.Fatal("diagnostic selection was lost or replayed", got, calls)
	}
}
