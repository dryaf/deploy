package config

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dryaf/deploy/internal/testutil"
)

const referenceApp = `app_name: foodlog
deployment: {mode: image}
environments:
  prod:
    server: vps-main
    target_dir: /home/deploy/apps/foodlog-prod
    database:
      driver: sqlite
      source: data/app.db
      backup: {enabled: true}
    quadlet:
      service_name: foodlog-prod
      image: docker.io/library/alpine:3.21
      volumes: ["./data:/data"]
`

func TestServerReferenceResolution(t *testing.T) {
	dir := t.TempDir()
	app := testutil.WriteFixture(t, dir, "deploy.yaml", referenceApp)
	testutil.WriteFixture(t, dir, "server.yaml", testutil.ServerInventory)
	cfg, err := Read(app, "")
	if err != nil {
		t.Fatal(err)
	}
	n, err := Normalize(cfg, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if n.Env.Host != "vps.example.com" || n.Env.User != "deploy" || n.Env.Port != 2222 || n.Env.SSHKey != filepath.Join(dir, "keys/admin") || n.Env.KnownHostsFile != filepath.Join(dir, "keys/known_hosts") {
		t.Fatalf("bad resolved connection: %+v", n.Env)
	}
	if n.Env.Dir != "/home/deploy/apps/foodlog-prod" || !n.Env.Database.Backup.Enabled {
		t.Fatal("application settings changed")
	}
	inv, err := ReadInventory(filepath.Join(dir, "server.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	worker := inv.Servers["home-backup"].BackupWorker
	if worker.Schedule != "hourly" || worker.Timezone != "Europe/Vienna" || worker.MaxSnapshotBytes != 10<<30 || worker.Storage.Path != "/mnt/backup/deploy" {
		t.Fatalf("bad defaults: %+v", worker)
	}
	if worker.Retention.KeepLast != 48 || worker.Retention.MaxAge != "720h" || worker.Notifications.Recovery == nil || !*worker.Notifications.Recovery {
		t.Fatal("incorrect policy defaults")
	}
}

func TestServerReferencePresenceRejectsEmptyAndInheritedConnections(t *testing.T) {
	for _, field := range []string{"host: ''", "user: null", "ssh_port: 0", "ssh_key: ''", "ssh_known_hosts: ''", "<<: {host: ''}"} {
		t.Run(field, func(t *testing.T) {
			dir := t.TempDir()
			testutil.WriteFixture(t, dir, "server.yaml", testutil.ServerInventory)
			app := testutil.WriteFixture(t, dir, "deploy.yaml", strings.Replace(referenceApp, "    server: vps-main", "    server: vps-main\n    "+field, 1))
			if _, err := Read(app, ""); err == nil || !strings.Contains(err.Error(), "inline") {
				t.Fatalf("accepted mixed connection: %v", err)
			}
		})
	}
}

func TestServerReferenceAliasPresence(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteFixture(t, dir, "server.yaml", testutil.ServerInventory)
	app := testutil.WriteFixture(t, dir, "deploy.yaml", `app_name: x
environments:
  first: &base
    server: vps-main
    host: ''
  second: *base
`)
	if _, err := Read(app, ""); err == nil {
		t.Fatal("alias bypassed presence check")
	}
}

func TestServerInventoryStrictness(t *testing.T) {
	for name, input := range map[string]string{
		"version":         "version: 3\nservers: {vps: {host: example.com, user: deploy}}",
		"missing version": "servers: {vps: {host: example.com, user: deploy}}",
		"user":            "version: 2\nservers: {vps: {host: example.com}}",
		"unknown":         "version: 2\nservers: {vps: {host: example.com, user: deploy, token: secret}}",
		"duplicate":       "version: 2\nversion: 2\nservers: {}",
		"trailing":        testutil.ServerInventory + "\n---\nother: true",
		"unknown source":  strings.Replace(testutil.ServerInventory, "sources: [vps-main]", "sources: [missing]", 1),
		"root worker":     strings.Replace(testutil.ServerInventory, "user: backup", "user: root", 1),
		"root source":     strings.Replace(testutil.ServerInventory, "user: deploy", "user: root", 1),
		"same endpoint":   strings.Replace(testutil.ServerInventory, "sources: [vps-main]", "sources: [vps-main, alias]", 1) + "  alias: {host: vps.example.com, user: deploy, ssh_port: 2222}\n",
	} {
		t.Run(name, func(t *testing.T) {
			file := testutil.WriteFixture(t, t.TempDir(), "server.yaml", input)
			if _, err := ReadInventory(file); err == nil {
				t.Fatal("accepted invalid inventory")
			}
		})
	}
}

func TestInventorySelectionMigrationAndOverride(t *testing.T) {
	dir := t.TempDir()
	file := testutil.WriteFixture(t, dir, "legacy.yaml", "host: example.com\nssh_key: key\nstack: {traefik: {version: v3.0, email: admin@example.com}}\n")
	legacy, err := SelectServer(file, "")
	if err != nil || legacy.User != "root" || legacy.SSHPort != 22 {
		t.Fatal(legacy, err)
	}
	var migrated bytes.Buffer
	if err := MigrateServer(file, "vps", &migrated); err != nil {
		t.Fatal(err)
	}
	candidate := testutil.WriteFixture(t, dir, "server.yaml", migrated.String())
	s, err := SelectServer(candidate, "vps")
	if err != nil || s.Stack != legacy.Stack || s.Host != legacy.Host || s.SSHKey != filepath.Join(dir, "key") {
		t.Fatal(s, err)
	}
	if _, err = SelectServer(candidate, ""); err == nil {
		t.Fatal("v2 selection did not require name")
	}
	if _, err = SelectServer(file, "vps"); err == nil {
		t.Fatal("legacy accepted a name")
	}
	app := testutil.WriteFixture(t, dir, "deploy.yaml", referenceApp)
	other := testutil.WriteFixture(t, t.TempDir(), "custom.yaml", testutil.ServerInventory)
	if _, err = Read(app, other); err != nil {
		t.Fatal("explicit override ignored", err)
	}
	if _, err = Read(app, file); err == nil {
		t.Fatal("reference accepted legacy inventory")
	}
}

func TestInlineAndMixedApplicationCompatibility(t *testing.T) {
	dir := t.TempDir()
	inline := strings.Replace(referenceApp, "    server: vps-main", "    host: vps.example.com\n    user: deploy", 1)
	app := testutil.WriteFixture(t, dir, "deploy.yaml", inline)
	if _, err := Read(app, ""); err != nil {
		t.Fatal("inline unexpectedly needs inventory", err)
	}
	mixed := referenceApp + `  test:
    host: vps.example.com
    user: deploy
    ssh_port: 2222
    target_dir: /home/deploy/apps/foodlog-test
    quadlet:
      service_name: foodlog-test
      image: docker.io/library/alpine:3.21
`
	testutil.WriteFixture(t, dir, "server.yaml", testutil.ServerInventory)
	testutil.WriteFixture(t, dir, "deploy.yaml", mixed)
	if _, err := Read(app, ""); err != nil {
		t.Fatal(err)
	}
	for _, replace := range [][2]string{{"service_name: foodlog-test", "service_name: foodlog-prod"}, {"target_dir: /home/deploy/apps/foodlog-test", "target_dir: /home/deploy/apps/foodlog-prod"}} {
		testutil.WriteFixture(t, dir, "deploy.yaml", strings.Replace(mixed, replace[0], replace[1], 1))
		if _, err := Read(app, ""); err == nil {
			t.Fatal("same-account collision accepted")
		}
	}
}

func TestBackupWorkerPolicyValidation(t *testing.T) {
	for _, replacement := range [][2]string{
		{"sources: [vps-main]", "sources: []"}, {"sources: [vps-main]", "sources: [vps-main, vps-main]"},
		{"path: /mnt/backup/deploy", "path: ~/backup"}, {"require_mount: /mnt/backup", "require_mount: /other"},
		{"sources: [vps-main]", "sources: [vps-main]\n      freshness: 1h"}, {"sources: [vps-main]", "sources: [vps-main]\n      max_run_time: 59m"},
		{"sources: [vps-main]", "sources: [vps-main]\n      timezone: Local"}, {"sources: [vps-main]", "sources: [vps-main]\n      timeouts: {transfer: 0s}"},
		{"primary: signal", "primary: telegram"}, {"primary: signal", "primary: signal\n        fallback: signal"},
		{"primary: signal", "primary: signal\n        daily_at: '25:00'"}, {"account_file: /home/backup/secrets/account", "account_file: relative"},
	} {
		t.Run(replacement[1], func(t *testing.T) {
			file := testutil.WriteFixture(t, t.TempDir(), "server.yaml", strings.Replace(testutil.ServerInventory, replacement[0], replacement[1], 1))
			if _, err := ReadInventory(file); err == nil {
				t.Fatal("invalid worker policy accepted")
			}
		})
	}
}

func TestBackupBindingAndReservedLabels(t *testing.T) {
	for name, edit := range map[string]func(*Environment){
		"no source": func(e *Environment) { e.Database.Source = "" }, "no bind": func(e *Environment) { e.Quadlet.Volumes = nil },
		"root": func(e *Environment) { e.User = "root" }, "override": func(e *Environment) { e.Quadlet.PodmanArgs = []string{"--userns=auto"} },
		"label":           func(e *Environment) { e.Quadlet.Labels = []string{"io.deploy.release=forged"} },
		"duplicate mount": func(e *Environment) { e.Quadlet.Volumes = append(e.Quadlet.Volumes, "./other:/data") },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := imageConfig()
			changeEnv(&cfg, func(e *Environment) {
				e.Database = DatabaseConfig{Driver: "sqlite", Source: "data/app.db", Backup: DatabaseBackupConfig{Enabled: true}}
				e.Quadlet.Volumes = []string{"./data:/data"}
				edit(e)
			})
			if _, err := Normalize(cfg, "prod"); err == nil {
				t.Fatal("unsafe backup binding accepted")
			}
		})
	}
}
