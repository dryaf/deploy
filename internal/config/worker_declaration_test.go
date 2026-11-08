package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/testutil"
)

const minimalWorkers = `version: 3
servers:
  manager:
    host: raspberrypi.local
    user: deploy
    notification_worker:
      providers: [telegram]
    backup_worker:
      sources: [vps-main]
  vps-main:
    host: vps.example.com
    user: deploy
`

func readDesiredFixture(t *testing.T, s string) (ServerInventory, error) {
	t.Helper()
	return ReadInventory(testutil.WriteFixture(t, t.TempDir(), "server.yaml", s))
}

func TestWorkerSourceRetirementDeclaration(t *testing.T) {
	for _, value := range []string{"{old-source: decommissioned}", "{vps-main: still-active}", "{old-source: ''}", "{../old: invalid}", "{old-source: [invalid]}", "{old-source: \"line\\nbreak\"}"} {
		yaml := strings.Replace(minimalWorkers, "sources: [vps-main]", "sources: [vps-main]\n      retire_sources: "+value, 1)
		inv, err := readDesiredFixture(t, yaml)
		if value == "{old-source: decommissioned}" {
			if err != nil || inv.Servers["manager"].Desired.Backup.RetireSources["old-source"] != "decommissioned" {
				t.Fatal(inv, err)
			}
		} else if err == nil {
			t.Fatal("invalid retirement policy accepted", value)
		}
	}
}

func TestBackupRouteMustBeAllowedByExplicitProducer(t *testing.T) {
	yaml := strings.Replace(minimalWorkers, "providers: [telegram]", "providers: [telegram]\n      routes:\n        alerts: {primary: telegram}\n        other: {primary: telegram}\n      producers:\n        backup-worker: {routes: [other]}", 1)
	if _, err := ParseInventory("server.yaml", []byte(yaml)); err == nil || !strings.Contains(err.Error(), "notifications.route") {
		t.Fatal("backup can publish to an unauthorized route", err)
	}
	yaml = strings.Replace(yaml, "routes: [other]", "routes: [alerts, other]", 1)
	if _, err := ParseInventory("server.yaml", []byte(yaml)); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerDeclarationExamplesAndIsolation(t *testing.T) {
	for _, name := range []string{"telegram", "signal-leds", "customized", "backup-only", "producer-policy"} {
		t.Run(name, func(t *testing.T) {
			inv, err := ReadInventory(filepath.Join("../../examples/workers", name+".yaml"))
			if err != nil {
				t.Fatal(err)
			}
			s := inv.Servers["manager"]
			if inv.Version != 3 || s.BackupWorker != nil || s.Desired == nil {
				t.Fatal("unresolved declaration escaped into runtime policy")
			}
			expectedDependency := "manager"
			if name == "backup-only" {
				expectedDependency = ""
				if WorkerEnabled(s.Desired.Backup.Notifications.Enabled) || len(s.Desired.MissingInputs()) != 0 {
					t.Fatal("standalone declaration unexpectedly requires reporting")
				}
			}
			if s.Desired.Backup.Notifications.Worker != expectedDependency || s.Desired.Backup.Storage.Mode == "" {
				t.Fatal("missing shared dependency/storage defaults")
			}
		})
	}
	inv, err := readDesiredFixture(t, minimalWorkers)
	if err != nil {
		t.Fatal(err)
	}
	d := inv.Servers["manager"].Desired
	if d.Backup.Timezone != "UTC" || d.Backup.Retention != nil || d.Backup.Storage.Path != "" {
		t.Fatal("incorrect v3 defaults or premature path expansion")
	}
	if got := strings.Join(d.MissingInputs(), ","); got != "notification_worker.providers.telegram.chat_id_file,notification_worker.providers.telegram.token_file" {
		t.Fatal(got)
	}
	expanded := strings.Replace(minimalWorkers, "providers: [telegram]", "providers: {telegram: {}}", 1)
	other, err := readDesiredFixture(t, expanded)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := d.Fingerprint()
	b, _ := other.Servers["manager"].Desired.Fingerprint()
	if a != b {
		t.Fatal("shorthand changed semantic policy")
	}
}

func TestWorkerDeclarationRejectsUnsafeOrAmbiguousPolicy(t *testing.T) {
	cases := map[string]string{
		"unknown":                  strings.Replace(minimalWorkers, "providers: [telegram]", "providers: {telegram: {typo: secret-value}}", 1),
		"duplicate provider":       strings.Replace(minimalWorkers, "[telegram]", "[telegram, telegram]", 1),
		"duplicate key":            strings.Replace(minimalWorkers, "user: deploy", "user: deploy\n    user: other", 1),
		"null worker":              strings.Replace(minimalWorkers, "notification_worker:\n      providers: [telegram]", "notification_worker: null", 1),
		"merge":                    strings.Replace(minimalWorkers, "notification_worker:", "notification_worker: &worker", 1),
		"local remote conflict":    strings.Replace(minimalWorkers, "[telegram]", "{telegram: {token_file: ~/.config/token, token_input: ./token}}", 1),
		"negative quota":           strings.Replace(minimalWorkers, "providers: [telegram]", "providers: [telegram]\n      limits: {max_messages: -1}", 1),
		"zero quota":               strings.Replace(minimalWorkers, "providers: [telegram]", "providers: [telegram]\n      limits: {max_messages: 0}", 1),
		"mount omission":           strings.Replace(minimalWorkers, "sources: [vps-main]", "sources: [vps-main]\n      storage: {mode: mount}", 1),
		"home mount conflict":      strings.Replace(minimalWorkers, "sources: [vps-main]", "sources: [vps-main]\n      storage: {mode: home, require_mount: /mnt/backup}", 1),
		"path injection":           strings.Replace(minimalWorkers, "sources: [vps-main]", "sources: [vps-main]\n      storage: {path: '~/$HOME/archive'}", 1),
		"unknown source":           strings.Replace(minimalWorkers, "sources: [vps-main]", "sources: [missing]", 1),
		"root worker":              strings.Replace(minimalWorkers, "user: deploy", "user: root", 1),
		"notification image":       strings.Replace(minimalWorkers, "providers: [telegram]", "providers: [telegram]\n      artifacts: {image: example.invalid/unused@sha256:"+strings.Repeat("a", 64)+"}", 1),
		"notification relay image": strings.Replace(minimalWorkers, "providers: [telegram]", "providers: [telegram]\n      artifacts: {relay_image: example.invalid/unused@sha256:"+strings.Repeat("a", 64)+"}", 1),
	}

	for name, s := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := readDesiredFixture(t, s)
			if err == nil {
				t.Fatal("accepted invalid policy")
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatal("secret leaked in diagnostic")
			}
		})
	}
}

func TestWorkerDeclarationMigrationPreservesVersionTwo(t *testing.T) {
	dir := t.TempDir()
	file := testutil.WriteFixture(t, dir, "server.yaml", testutil.ServerInventory)
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	original, err := ReadInventory(file)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = Version3Candidate(file, &out); err != nil {
		t.Fatal(err)
	}
	candidate := testutil.WriteFixture(t, dir, "candidate.yaml", out.String())
	converted, err := ReadInventory(candidate)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range original.Servers {
		if b := s.BackupWorker; b != nil {
			d := converted.Servers[name].Desired.Backup
			if d.Timezone != b.Timezone || d.Storage.Path != b.Storage.Path || d.Storage.RequireMount != b.Storage.RequireMount || d.Storage.Mode != "mount" || d.Notifications.Primary != b.Notifications.Primary {
				t.Fatal("migration changed effective policy")
			}
			if d.Paths.State != "~/.local/state/deploy/backup-worker/state" {
				t.Fatal("old paths not pinned")
			}
		}
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("migration overwrote original")
	}
	if _, err = readDesiredFixture(t, strings.Replace(minimalWorkers, "version: 3", "version: 2", 1)); err == nil {
		t.Fatal("v3 fields accepted as version 2")
	}
}

func TestWorkerDeclarationAdvancedBoundsAndCadence(t *testing.T) {
	for _, cadence := range []string{"daily", "weekly"} {
		inv, err := readDesiredFixture(t, strings.Replace(minimalWorkers, "sources: [vps-main]", "sources: [vps-main]\n      schedule: "+cadence, 1))
		if err != nil {
			t.Fatal(err)
		}
		if inv.Servers["manager"].Desired.Backup.Freshness == "2h" {
			t.Fatal("freshness does not accommodate cadence")
		}
	}
	for _, extra := range []string{
		"limits: {max_messages: 0x0}",
		"status_leds: {bindings: {backup: {producer: backup-worker, subjects: ['*', backup], led: green}}}",
		"status_leds: {bindings: {backup: {producer: backup-worker, subjects: [backup], led: green, styles: {critical: {pattern: heartbeat, priority: 1}}}}}",
	} {
		_, err := readDesiredFixture(t, strings.Replace(minimalWorkers, "providers: [telegram]", "providers: [telegram]\n      "+extra, 1))
		if err == nil {
			t.Fatal("accepted invalid advanced policy", extra)
		}
	}
}

func TestWorkerDeclarationDisablingBothRetainsDependency(t *testing.T) {
	input := strings.Replace(minimalWorkers, "providers: [telegram]", "enabled: false\n      providers: [telegram]", 1)
	single, err := readDesiredFixture(t, input)
	if err != nil || single.Servers["manager"].Desired.Backup.Notifications.Worker != "manager" {
		t.Fatal("shared gateway disable lost dependency", err)
	}
	input = strings.Replace(input, "sources: [vps-main]", "enabled: false\n      sources: [vps-main]", 1)
	inv, err := readDesiredFixture(t, input)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Servers["manager"].Desired.Backup.Notifications.Worker != "manager" {
		t.Fatal("disabled dependency discarded")
	}
}

func TestWorkerDeclarationExplicitDefaultsHaveStableFingerprint(t *testing.T) {
	a, err := readDesiredFixture(t, minimalWorkers)
	if err != nil {
		t.Fatal(err)
	}
	explicit := strings.Replace(minimalWorkers, "sources: [vps-main]", "enabled: true\n      sources: [vps-main]\n      notifications: {enabled: true, worker: manager, route: alerts}", 1)
	b, err := readDesiredFixture(t, explicit)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := a.Servers["manager"].Desired.Fingerprint()
	second, _ := b.Servers["manager"].Desired.Fingerprint()
	if first != second {
		t.Fatal("explicit default changed policy fingerprint")
	}
}

func TestWorkerDeclarationRetainsDomainSubjectTokens(t *testing.T) {
	extra := `providers: [telegram]
      commands:
        workers: {backup: backup-worker}
        conversations:
          operations: {provider: telegram, account: bot, native_id: '12345'}
        permissions:
          - actor: operator
            conversation: operations
            worker: backup
            permissions: [backup:read]
            scope: {subjects: [source/database]}
      status_leds:
        bindings:
          backup: {producer: backup-worker, subjects: [source/database], led: green}`
	if _, err := readDesiredFixture(t, strings.Replace(minimalWorkers, "providers: [telegram]", extra, 1)); err != nil {
		t.Fatal(err)
	}
}
