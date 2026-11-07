package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/ui"
)

func qualificationFixture(t *testing.T, configure ...func(*Runtime)) (ManagedSpec, managedInstaller, Runtime, QualificationReport) {
	t.Helper()
	spec, installer, _, _ := managedFixture(t)
	qualificationFixtureInputs(t, spec.RuntimeFile)
	if len(configure) > 0 {
		runtime, err := ReadRuntime(spec.RuntimeFile)
		if err != nil {
			t.Fatal(err)
		}
		for _, apply := range configure {
			apply(&runtime)
		}
		writeRuntime(t, spec.RuntimeFile, runtime)
	}
	if _, err := installer.prepare(context.Background(), "setup", spec); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runQualificationCLI([]string{"qualification-template", "--local", "--root", spec.Root}, &out); err != nil {
		t.Fatal(err)
	}
	var report QualificationReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	return spec, installer, c, report
}

func qualificationFixtureInputs(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c Runtime
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	p := c.Providers["telegram"]
	p.TokenFile = filepath.Join(filepath.Dir(path), "token")
	p.ChatFile = filepath.Join(filepath.Dir(path), "chat")
	for path, data := range map[string]string{p.TokenFile: "123:secret", p.ChatFile: "chat-one"} {
		if err = os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	c.Providers["telegram"] = p
	writeRuntime(t, path, c)
}

func qualificationPass(report *QualificationReport) {
	report.Operator = "fixture-operator"
	for i := range report.Checks {
		report.Checks[i].Result = "passed"
		report.Checks[i].ObservedAt = report.CreatedAt
		report.Checks[i].EvidenceHash = managedDigest([]byte("disposable synthetic observation"))
	}
}

func writeQualificationReport(t *testing.T, report QualificationReport) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "report.json")
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func qualifyFixture(t *testing.T, root string, report QualificationReport) (QualificationStatus, error) {
	t.Helper()
	var out bytes.Buffer
	err := runQualificationCLI([]string{"qualify", "--local", "--root", root, "--report", writeQualificationReport(t, report)}, &out)
	var status QualificationStatus
	if err == nil {
		err = json.Unmarshal(out.Bytes(), &status)
	}
	return status, err
}

func TestQualificationCLIRecordsEvidenceWithoutActivation(t *testing.T) {
	spec, _, _, report := qualificationFixture(t)
	if len(report.Checks) != 13 {
		t.Fatal("unexpected fixture coverage", len(report.Checks))
	}
	if _, err := os.Stat(filepath.Join(spec.Root, "qualifications")); !os.IsNotExist(err) {
		t.Fatal("template wrote audit", err)
	}
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil || !status.BindingMatches || status.EvidenceComplete || status.Qualified || status.ActivationAllowed || status.Record.Sequence != 1 {
		t.Fatal(status, err)
	}
	// A report is immutable. Completing a partial report requires a fresh ID.
	qualificationPass(&report)
	if _, err = qualifyFixture(t, spec.Root, report); err == nil {
		t.Fatal("changed report ID accepted")
	}
	report.ID, _ = safefs.NewID("n")
	status, err = qualifyFixture(t, spec.Root, report)
	if err != nil || !status.EvidenceComplete || status.Qualified || status.ActivationAllowed || status.Record.Sequence != 2 {
		t.Fatal(status, err)
	}
	before := protocol.Hash(status.Record)
	status, err = qualifyFixture(t, spec.Root, report)
	if err != nil || protocol.Hash(status.Record) != before {
		t.Fatal("retry appended/renewed evidence", status, err)
	}
	// A later failure supersedes the successful report in status; never cherry-pick.
	report.ID, _ = safefs.NewID("n")
	report.Checks[0].Result = "failed"
	status, err = qualifyFixture(t, spec.Root, report)
	if err != nil || status.EvidenceComplete || status.Record.Sequence != 3 {
		t.Fatal(status, err)
	}
	if err = RunCLI([]string{"activate", "--local", "--root", spec.Root}); err == nil {
		t.Fatal("evidence enabled activation")
	}
}

func TestQualificationDryRunLeaseAndInputValidation(t *testing.T) {
	spec, _, _, report := qualificationFixture(t)
	old := ui.DryRun
	t.Cleanup(func() { ui.DryRun = old })
	ui.DryRun = true
	args := []string{"qualify", "--local", "--root", spec.Root, "--report", writeQualificationReport(t, report)}
	var out bytes.Buffer
	if err := runQualificationCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"dry_run":true`) {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(spec.Root, "qualifications")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote audit", err)
	}
	ui.DryRun = false
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = runQualificationCLI(args, &out); err == nil {
		t.Fatal("ignored operation lease")
	}
	lock.Close()
	for _, invalid := range [][]string{
		{"qualify", "--local", "--root", spec.Root},
		{"qualification-template", "--local", "--root", spec.Root, "--report", args[len(args)-1]},
		{"qualification-status", "--local", "--root", spec.Root, "--channel", "telegram"},
		{"qualification-template", "--root", spec.Root},
	} {
		if err = runQualificationCLI(invalid, &out); err == nil {
			t.Fatal("invalid args accepted", invalid)
		}
	}
}

func TestQualificationRejectsEvidenceAndBindingDrift(t *testing.T) {
	spec, installer, c, report := qualificationFixture(t)
	qualificationPass(&report)
	for name, edit := range map[string]func(*QualificationReport){
		"missing-check":         func(r *QualificationReport) { r.Checks = r.Checks[1:] },
		"duplicate-check":       func(r *QualificationReport) { r.Checks[1] = r.Checks[0] },
		"unknown-check":         func(r *QualificationReport) { r.Checks[0].ID = "made-up" },
		"missing-digest":        func(r *QualificationReport) { r.Checks[0].EvidenceHash = "" },
		"future":                func(r *QualificationReport) { r.Checks[0].ObservedAt = time.Now().Add(time.Hour) },
		"pending-with-evidence": func(r *QualificationReport) { r.Checks[0].Result = "pending" },
		"unsupported-waiver":    func(r *QualificationReport) { r.Checks[0].Result = "waived" },
		"expired": func(r *QualificationReport) {
			r.CreatedAt = time.Now().Add(-31 * 24 * time.Hour)
			for i := range r.Checks {
				r.Checks[i].ObservedAt = r.CreatedAt
			}
		},
		"uid": func(r *QualificationReport) { r.Binding.UID++ },
		"architecture": func(r *QualificationReport) {
			if r.Binding.Architecture == "amd64" {
				r.Binding.Architecture = "arm64"
			} else {
				r.Binding.Architecture = "amd64"
			}
		},
		"credentials":          func(r *QualificationReport) { r.Binding.Inputs = map[string]string{} },
		"image-not-configured": func(r *QualificationReport) { r.Environment.SignalImageDigest = "sha256:" + strings.Repeat("a", 64) },
		"invalid-image":        func(r *QualificationReport) { r.Environment.SignalImageDigest = "latest" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := report
			copy.Checks = append([]QualificationCheck(nil), report.Checks...)
			edit(&copy)
			if _, err := qualifyFixture(t, spec.Root, copy); err == nil {
				t.Fatal("invalid evidence recorded")
			}
		})
	}
	if err := os.WriteFile(c.Providers["telegram"].TokenFile, []byte("changed-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := qualifyFixture(t, spec.Root, report); err == nil {
		t.Fatal("changed credential accepted")
	}
	if err := os.WriteFile(c.Providers["telegram"].TokenFile, []byte("123:secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := qualifyFixture(t, spec.Root, report); err != nil {
		t.Fatal(err)
	}
	if _, err := installer.prepare(context.Background(), "update", spec); err != nil {
		t.Fatal(err)
	}
	// Even identical executable bytes at a new revision require a new report.
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil || status.BindingMatches || status.EvidenceComplete {
		t.Fatal("historical retry acquired current authority", status, err)
	}
	report.ID, _ = safefs.NewID("n")
	if _, err = qualifyFixture(t, spec.Root, report); err == nil {
		t.Fatal("old revision evidence admitted")
	}
}

func TestQualificationJournalInterruption(t *testing.T) {
	for _, point := range []string{"qualification_record", "qualification_head"} {
		t.Run(point, func(t *testing.T) {
			spec, _, _, report := qualificationFixture(t)
			d, err := privateManagedDir(spec.Root, true)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err = appendQualification(d, report, time.Now(), func(p string) error {
				if p == point {
					return errors.New("interruption")
				}
				return nil
			}); err == nil {
				t.Fatal("missing boundary")
			}
			h, err := readQualificationHistory(d, report.Binding.Manifest.Installation)
			if err != nil || len(h.Records) != 1 || h.Pending != (point == "qualification_record") {
				t.Fatal(h, err)
			}
			original := protocol.Hash(h.Records[0])
			if h.Pending {
				other := report
				other.ID, _ = safefs.NewID("n")
				if _, err = appendQualification(d, other, time.Now(), nil); err == nil {
					t.Fatal("superseded pending audit")
				}
			}
			// Acknowledging historical evidence after expiry never renews it.
			if _, err = appendQualification(d, report, time.Now().Add(31*24*time.Hour), nil); err != nil {
				t.Fatal(err)
			}
			h, err = readQualificationHistory(d, report.Binding.Manifest.Installation)
			if err != nil || h.Pending || len(h.Records) != 1 || protocol.Hash(h.Records[0]) != original {
				t.Fatal(h, err)
			}
		})
	}
}

func TestQualificationHistoryDetectsDamage(t *testing.T) {
	for _, damage := range []string{"missing-tail", "changed-report", "changed-head", "foreign-record", "too-many-temporaries"} {
		t.Run(damage, func(t *testing.T) {
			spec, _, _, report := qualificationFixture(t)
			if _, err := qualifyFixture(t, spec.Root, report); err != nil {
				t.Fatal(err)
			}
			d, err := privateManagedDir(spec.Root, true)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			h, err := readQualificationHistory(d, report.Binding.Manifest.Installation)
			if err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "missing-tail":
				err = os.Remove(filepath.Join(spec.Root, "qualifications", "0001.json"))
			case "changed-report":
				h.Records[0].Report.Operator = "changed"
				err = d.WriteJSON("qualifications/0001.json", h.Records[0], true)
			case "changed-head":
				h.Head.Hash = strings.Repeat("a", 64)
				err = d.WriteJSON("qualification-head.json", h.Head, true)
			case "foreign-record":
				err = d.WriteJSON("qualifications/foreign.json", h.Records[0], false)
			case "too-many-temporaries":
				for i := 0; i < 33; i++ {
					id, _ := safefs.NewID("n")
					err = os.WriteFile(filepath.Join(spec.Root, "qualifications", "."+id+".json"), []byte("partial"), 0600)
					if err != nil {
						break
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = readQualificationHistory(d, report.Binding.Manifest.Installation); err == nil {
				t.Fatal("damaged audit accepted")
			}
		})
	}
}

func TestQualificationConfiguredCoverage(t *testing.T) {
	_, _, c, report := qualificationFixture(t)
	c.Providers["signal"] = provider.Profile{Kind: "signal"}
	route := c.Worker.Routes["ops"]
	route.Fallback = "signal"
	c.Worker.Routes["ops"] = route
	c.LEDSocket = "/private/led.sock"
	c.Worker.Interaction = &interaction.Config{
		Destinations: []interaction.Destination{{ID: "conversation"}},
		Workers: []interaction.Worker{{Producer: c.Worker.Producers[0].Producer, Commands: map[string]interaction.Descriptor{
			"status": {Effect: "read"}, "request": {Effect: "job"},
		}}},
	}
	report.Checks = qualificationChecks(c)
	qualificationPass(&report)
	for _, want := range []struct{ scope, id string }{
		{"route/ops", "fallback-receipt"}, {"route/ops", "both-providers-fail"},
		{"signal", "account-continuity"}, {"led", "healthy-ttl-expiry"},
		{"conversation/conversation", "reply-identity"}, {"command/fixture/request", "idempotency"},
	} {
		found := false
		for _, check := range report.Checks {
			if check.Scope == want.scope && check.ID == want.id {
				found = true
			}
		}
		if !found {
			t.Fatal("missing configured coverage", want)
		}
	}
	if report.evidenceComplete(c) {
		t.Fatal("missing environment qualified")
	}
	report.Environment = QualificationEnvironment{SignalImageDigest: "sha256:" + strings.Repeat("b", 64), LEDVersion: "1.0.0", LEDConfigHash: strings.Repeat("c", 64)}
	if err := report.matches(report.Binding, c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !report.evidenceComplete(c) {
		t.Fatal("complete synthetic evidence not recognized")
	}
	for _, check := range report.Checks {
		if check.Scope == "command/fixture/status" {
			t.Fatal("read command given mutation checklist")
		}
	}
}

func TestQualificationAuditCapacityAndClock(t *testing.T) {
	spec, _, _, report := qualificationFixture(t)
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	var first QualificationReport
	for i := 0; i < maxQualificationRecords; i++ {
		report.ID, _ = safefs.NewID("n")
		if i == 0 {
			first = report
		}
		if _, err = appendQualification(d, report, now, nil); err != nil {
			t.Fatal(i, err)
		}
	}
	report.ID, _ = safefs.NewID("n")
	if _, err = appendQualification(d, report, now, nil); err == nil {
		t.Fatal("history overflow accepted")
	}
	if _, err = appendQualification(d, first, now, nil); err != nil {
		t.Fatal("exact retry at capacity failed", err)
	}
	h, err := readQualificationHistory(d, report.Binding.Manifest.Installation)
	if err != nil || len(h.Records) != maxQualificationRecords {
		t.Fatal(h, err)
	}
}

func TestQualificationPendingSurvivesUpdate(t *testing.T) {
	spec, installer, _, report := qualificationFixture(t)
	qualificationPass(&report)
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err = appendQualification(d, report, time.Now(), func(string) error { return errors.New("stop") }); err == nil {
		t.Fatal("missing interruption")
	}
	if _, err = installer.prepare(context.Background(), "update", spec); err != nil {
		t.Fatal(err)
	}
	status, err := qualifyFixture(t, spec.Root, report)
	if err != nil || status.Pending || status.BindingMatches || status.EvidenceComplete {
		t.Fatal("historical acknowledgement changed binding", status, err)
	}
	var out bytes.Buffer
	if err = runQualificationCLI([]string{"qualification-template", "--local", "--root", spec.Root}, &out); err != nil {
		t.Fatal(err)
	}
	report = QualificationReport{}
	if err = json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	status, err = qualifyFixture(t, spec.Root, report)
	if err != nil || status.Record.Sequence != 2 || !status.BindingMatches {
		t.Fatal(status, err)
	}
}

func TestQualificationClockAndInstallationGuards(t *testing.T) {
	spec, installer, _, report := qualificationFixture(t)
	d, err := privateManagedDir(spec.Root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now()
	if _, err = appendQualification(d, report, now, nil); err != nil {
		t.Fatal(err)
	}
	report.ID, _ = safefs.NewID("n")
	if _, err = appendQualification(d, report, report.CreatedAt.Add(-time.Second), nil); err == nil {
		t.Fatal("backward clock accepted")
	}
	if _, err = appendQualification(d, report, report.CreatedAt.Add(qualificationWindow), nil); err == nil {
		t.Fatal("expired report accepted")
	}
	h, err := readQualificationHistory(d, report.Binding.Manifest.Installation)
	if err != nil || len(h.Records) != 1 {
		t.Fatal(h, err)
	}
	unit := filepath.Join(spec.UnitDirectory, managedUnit)
	before, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(unit, append(before, []byte("\n# changed\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = runQualificationCLI([]string{"qualification-status", "--local", "--root", spec.Root}, &out); err == nil {
		t.Fatal("changed unit accepted")
	}
	if err = os.WriteFile(unit, before, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = installer.uninstall(context.Background(), spec.Root); err != nil {
		t.Fatal(err)
	}
	if err = runQualificationCLI([]string{"qualification-template", "--local", "--root", spec.Root}, &out); err == nil {
		t.Fatal("uninstalled revision accepted")
	}
}
