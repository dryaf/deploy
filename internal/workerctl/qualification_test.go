package workerctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/safefs"
)

type qualificationController func(ControlRequest) (ControlResult, error)

func (f qualificationController) Apply(_ context.Context, _ config.ServerConfig, _ Artifact, r ControlRequest) (ControlResult, error) {
	return f(r)
}

func TestQualificationWizardResumesAfterInputEOF(t *testing.T) {
	r, _ := bootstrapControlFixture(t)
	id, _ := safefs.NewID("n")
	session := install.QualificationSession{ID: id, Checks: []install.QualificationCheckSummary{{Scope: "installation", ID: "native-runtime", Result: "pending"}, {Scope: "provider/telegram", ID: "destination-receipt", Result: "pending"}}}
	observations, commits := 0, 0
	file := filepath.Join(t.TempDir(), "evidence.log")
	if err := os.WriteFile(file, []byte("synthetic test log, never live provider evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := qualificationEvidenceDigest(file)
	if err != nil {
		t.Fatal(err)
	}
	controller := qualificationController(func(got ControlRequest) (ControlResult, error) {
		if got.DesiredHash != r.DesiredHash || got.Server != r.Server || got.Worker != "notification-worker" || len(got.Inputs) != 0 {
			t.Fatal("qualification changed policy or carried secrets")
		}
		switch got.Action {
		case "qualify-begin":
			if got.Observation != nil {
				t.Fatal("begin carried observation")
			}
		case "qualify-observe":
			observations++
			o := got.Observation
			if o == nil || o.Session != id || o.Result != "passed" || o.EvidenceHash != digest || o.ID != session.Checks[0].ID {
				t.Fatal("observation lost evidence", o)
			}
			session.Checks[0].Result = o.Result
		case "qualify-commit":
			commits++
			if got.Observation == nil || got.Observation.Session != id {
				t.Fatal("commit changed session")
			}
			session.Recorded = true
		default:
			t.Fatal("qualification performed unrelated action", got.Action)
		}
		copy := session
		copy.Checks = append([]install.QualificationCheckSummary(nil), session.Checks...)
		return ControlResult{Version: 1, State: "qualification_recorded", Code: "ok", QualificationSession: &copy, Next: "remaining observations required"}, nil
	})
	inv := config.ServerInventory{Servers: map[string]config.ServerConfig{r.Server: {Desired: &r.Desired}}}
	plan := Plan{DesiredHash: r.DesiredHash, Artifacts: &ResolvedArtifacts{}}
	var out, prompts bytes.Buffer
	cli := CLI{Controller: controller, Out: &out, Err: &prompts, Prompt: StreamPrompt{Input: bufio.NewReader(strings.NewReader("passed\n" + file + "\n"))}}
	v := Invocation{Server: r.Server, Options: Options{JSON: true}}
	if err = cli.qualifyNotification(context.Background(), v, inv, r.Facts, plan); err == nil || observations != 1 || commits != 0 {
		t.Fatal("EOF lost draft or committed unfinished session", observations, commits, err)
	}
	cli.Prompt = StreamPrompt{Input: bufio.NewReader(strings.NewReader("pending\n"))}
	if err = cli.qualifyNotification(context.Background(), v, inv, r.Facts, plan); err != nil || observations != 1 || commits != 1 {
		t.Fatal("resume replayed observation", observations, commits, err)
	}
	var result ControlResult
	if err = json.Unmarshal(out.Bytes(), &result); err != nil || result.QualificationSession.EvidenceComplete || result.QualificationSession.Checks[1].Result != "pending" {
		t.Fatal(result, err)
	}
	if strings.Contains(out.String(), digest) || strings.Contains(out.String(), file) {
		t.Fatal("qualification output exposed evidence details")
	}
}

func TestQualificationEvidenceRejectsSpecialAndEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := qualificationEvidenceDigest(path); err == nil {
		t.Fatal("accepted FIFO evidence")
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", empty, dir, filepath.Join(dir, "missing")} {
		if _, err := qualificationEvidenceDigest(p); err == nil {
			t.Fatal("accepted invalid evidence", p)
		}
	}
}

func TestQualificationShortRoutingAndOfflineBehavior(t *testing.T) {
	for _, args := range [][]string{{"qualify", "manager"}, {"qualify", "manager", "--json"}} {
		if !Matches(args) {
			t.Fatal("missing short qualification route")
		}
		if v, err := Parse(args, Options{}); err != nil || v.Operation != "qualify" {
			t.Fatal(v, err)
		}
	}
	for _, args := range [][]string{{"qualify", "manager", "--report", "/private/report.json"}, {"qualify", "--local", "--root", "/root"}} {
		if Matches(args) {
			t.Fatal("intercepted specialist qualification")
		}
	}
	file := filepath.Join(t.TempDir(), "server.yaml")
	yaml := strings.Replace(backupOnlyInventory, "    backup_worker:", "    notification_worker:\n      providers: [telegram]\n    backup_worker:", 1)
	if err := os.WriteFile(file, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cli := CLI{Out: &out, Controller: qualificationController(func(ControlRequest) (ControlResult, error) {
		return ControlResult{}, fmt.Errorf("unexpected network call")
	})}
	if err := cli.Run(context.Background(), "notification-worker", []string{"qualify", "manager", "--dry-run", "--servers", file, "--json"}, Options{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "resumable private draft") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := cli.Run(context.Background(), "notification-worker", []string{"qualify", "manager", "--non-interactive", "--servers", file}, Options{}); err == nil || !strings.Contains(err.Error(), "operator observations") {
		t.Fatal(err)
	}
}

func TestQualificationWizardRecordsLEDEnvironmentAndResumesEOF(t *testing.T) {
	r, _ := bootstrapControlFixture(t)
	id, _ := safefs.NewID("n")
	session := install.QualificationSession{ID: id, LEDEnvironmentRequired: true, Checks: []install.QualificationCheckSummary{{Scope: "led", ID: "visual-patterns", Result: "pending"}}}
	file := filepath.Join(t.TempDir(), "observed-config.json")
	if err := os.WriteFile(file, []byte("synthetic fixture configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	digest, err := qualificationEvidenceDigest(file)
	if err != nil {
		t.Fatal(err)
	}
	environments, checks, commits := 0, 0, 0
	controller := qualificationController(func(got ControlRequest) (ControlResult, error) {
		switch got.Action {
		case "qualify-begin":
		case "qualify-observe":
			if got.Observation.LEDVersion != "" {
				if got.Observation.LEDVersion != "synthetic-v1" || got.Observation.LEDConfigHash != digest || got.Observation.ID != "" || got.Observation.EvidenceHash != "" {
					t.Fatal("environment mixed with check evidence")
				}
				environments++
				session.LEDEnvironmentRequired = false
			} else {
				checks++
				session.Checks[0].Result = "passed"
			}
		case "qualify-commit":
			commits++
			session.Recorded = true
			session.EvidenceComplete = !session.LEDEnvironmentRequired && checks == 1
		default:
			t.Fatal(got.Action)
		}
		copy := session
		copy.Checks = append([]install.QualificationCheckSummary(nil), session.Checks...)
		return ControlResult{Version: 1, Code: "ok", QualificationSession: &copy}, nil
	})
	inv := config.ServerInventory{Servers: map[string]config.ServerConfig{r.Server: {Desired: &r.Desired}}}
	plan := Plan{DesiredHash: r.DesiredHash, Artifacts: &ResolvedArtifacts{}}
	var out, prompts bytes.Buffer
	cli := CLI{Controller: controller, Out: &out, Err: &prompts, Prompt: StreamPrompt{Input: bufio.NewReader(strings.NewReader("synthetic-v1\n"))}}
	v := Invocation{Server: r.Server, Options: Options{JSON: true}}
	if err = cli.qualifyNotification(context.Background(), v, inv, r.Facts, plan); err == nil || environments != 0 || commits != 0 {
		t.Fatal("EOF committed partial environment", err)
	}
	cli.Prompt = StreamPrompt{Input: bufio.NewReader(strings.NewReader("synthetic-v1\n" + file + "\npassed\n" + file + "\n"))}
	if err = cli.qualifyNotification(context.Background(), v, inv, r.Facts, plan); err != nil {
		t.Fatal(err)
	}
	if environments != 1 || checks != 1 || commits != 1 || !session.EvidenceComplete {
		t.Fatal("wizard lost environment or observations")
	}
	if strings.Contains(out.String(), digest) || strings.Contains(out.String(), file) {
		t.Fatal("session exposed private evidence references")
	}
}
