package workerctl

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOnlyStatusChecksPathsWithoutReadingSecrets(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	root := t.TempDir()
	if err = os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	if err = os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "runtime.json")
	if err = os.WriteFile(secret, []byte("do-not-disclose-this"), 0600); err != nil {
		t.Fatal(err)
	}
	p := ResolvedPaths{Config: root, State: state, Runtime: secret, Control: filepath.Join(root, "control"), Installation: filepath.Join(root, "installation")}
	command := exec.Command(python, "-I", "-c", statusScript)
	command.Stdin = bytes.NewReader(statusRequest("notification-worker", p))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(output))
	}
	if bytes.Contains(output, []byte("do-not-disclose")) {
		t.Fatal("status read private content")
	}
	var status TargetStatus
	if err = json.Unmarshal(output, &status); err != nil {
		t.Fatal(err)
	}
	plan := ApplyStatus(Plan{}, status)
	if plan.State != "recovery_required" || plan.Code != "missing_manifest" {
		t.Fatalf("%+v: %s", plan, output)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 2 {
		t.Fatal("status created files")
	}
	link := filepath.Join(root, "installation")
	os.Symlink(state, link)
	command = exec.Command(python, "-I", "-c", statusScript)
	command.Stdin = bytes.NewReader(statusRequest("notification-worker", p))
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(output))
	}
	if strings.Contains(string(output), "do-not-disclose") {
		t.Fatal("secret disclosed")
	}
	json.Unmarshal(output, &status)
	if got := ApplyStatus(Plan{}, status); got.Code != "unsafe_path" {
		t.Fatal("symlink accepted", got)
	}
}

func TestStatusUsesCanonicalBackupUnit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	root := t.TempDir()
	os.Chmod(root, 0700)
	script := "#!/bin/sh\n[ \"$3\" = deploy-backup.service ] || exit 1\nprintf 'LoadState=loaded\\nActiveState=inactive\\n'\n"
	if err = os.WriteFile(filepath.Join(root, "systemctl"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	p := ResolvedPaths{Config: filepath.Join(root, "config"), State: filepath.Join(root, "state"), Runtime: filepath.Join(root, "runtime.json"), Control: filepath.Join(root, "control"), Archive: filepath.Join(root, "archive")}
	cmd := exec.Command(python, "-I", "-c", statusScript)
	cmd.Env = append(os.Environ(), "PATH="+root)
	cmd.Stdin = bytes.NewReader(statusRequest("backup-worker", p))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(output))
	}
	var status TargetStatus
	if err = json.Unmarshal(output, &status); err != nil {
		t.Fatal(err)
	}
	if status.LoadState != "loaded" || status.ActiveState != "inactive" {
		t.Fatal("wrong unit inspected", status)
	}
}
