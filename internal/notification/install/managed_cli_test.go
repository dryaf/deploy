package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/ui"
)

func TestManagedCLIDryRunValidatesPinsWithoutCreatingInstallation(t *testing.T) {
	spec, _, _, _ := managedFixture(t)
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	spec.UnitDirectory = filepath.Join(configHome, "systemd", "user")
	path := filepath.Join(t.TempDir(), "installation.json")
	write := func() {
		t.Helper()
		b, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	args := []string{"setup", "--local", "--spec", path}
	if err := RunCLI(args); err != nil {
		t.Fatal(err)
	}
	if err := RunCLI([]string{"migrate-schema", "--local", "--spec", path}); err != nil {
		t.Fatal(err)
	}
	if err := RunCLI([]string{"migrate-led", "--local", "--spec", path}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spec.Root); !os.IsNotExist(err) {
		t.Fatal("dry-run created root", err)
	}
	if _, err := os.Stat(spec.UnitDirectory); !os.IsNotExist(err) {
		t.Fatal("dry-run created unit directory", err)
	}
	spec.BinarySHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	write()
	if err := RunCLI(args); err == nil {
		t.Fatal("dry-run accepted wrong binary digest")
	}
	if err := RunCLI([]string{"resume", "--local", "--root", spec.Root, "--spec", path}); err == nil {
		t.Fatal("resume accepted replacement inputs")
	}
	if err := RunCLI([]string{"__managed-probe", "--local"}); err == nil {
		t.Fatal("probe accepted unexpected args")
	}
}

func TestInspectionCLIDryRunRejectsMutationFlags(t *testing.T) {
	spec, _, _, _ := managedFixture(t)
	args := []string{"receipts-inspect", "--local", "--state-dir", spec.State, "--socket", filepath.Join(spec.State, "api.sock"), "--producer", "fixture", "--producer-epoch", "one"}
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	var out bytes.Buffer
	if err := runInspectionCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("no API calls")) {
		t.Fatal(out.String())
	}
	for _, extra := range [][]string{{"--limit", "129"}, {"--after", "-1"}, {"--action", "retry"}, {"--accept-duplicate-risk"}} {
		if err := runInspectionCLI(append(append([]string{}, args...), extra...), &out); err == nil {
			t.Fatal("invalid inspection accepted", extra)
		}
	}
}

func TestUserManagerInspectionParsesMissingUnitAndRejectsAliases(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "properties")
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\ncat \"$DEPLOY_TEST_MANAGER_PROPERTIES\"\nexit \"${DEPLOY_TEST_MANAGER_EXIT:-0}\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEPLOY_TEST_MANAGER_PROPERTIES", state)
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(state, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("LoadState=not-found\nActiveState=inactive\nFragmentPath=\nDropInPaths=\nUnitFileState=\nNames=" + managedUnit + "\n")
	t.Setenv("DEPLOY_TEST_MANAGER_EXIT", "1")
	u := userManagedServices{}
	s, err := u.Inspect(context.Background())
	if err != nil || s.Load != "not-found" {
		t.Fatal(s, err)
	}
	t.Setenv("DEPLOY_TEST_MANAGER_EXIT", "0")
	m := ManagedManifest{UnitDirectory: dir}
	write("LoadState=loaded\nActiveState=inactive\nFragmentPath=" + filepath.Join(dir, managedUnit) + "\nDropInPaths=\nUnitFileState=disabled\nNames=" + managedUnit + " alias.service\n")
	if _, err = inspectManagedService(context.Background(), u, m, false); err == nil {
		t.Fatal("alias accepted")
	}
	write("LoadState=not-found\nActiveState=inactive\nFragmentPath=\nDropInPaths=\nUnitFileState=\nNames=" + managedUnit + "\nNames=alias.service\n")
	if _, err = u.Inspect(context.Background()); err == nil {
		t.Fatal("duplicate manager property accepted")
	}
	write("Failed to connect to bus\n")
	if _, err = u.Inspect(context.Background()); err == nil {
		t.Fatal("manager unavailable treated as absence")
	}
}
