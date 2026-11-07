package cli

import (
	"bytes"
	binarypkg "encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeclarativeWorkerCLIActualArguments(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "deploy")
	// Fixture binaries also build from exported source trees without Git metadata.
	build := exec.Command("go", "build", "-buildvcs=false", "-o", binary, "../../cmd/deploy")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	file := filepath.Join(dir, "server.yaml")
	body := `version: 3
servers:
  manager:
    host: example.test
    user: deploy
    notification_worker: {providers: [telegram]}
    backup_worker:
      sources: [source]
      notifications: {enabled: false}
  source:
    host: source.test
    user: deploy
  status:
    host: example.test
    user: deploy
    notification_worker: {providers: [signal]}
`
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--servers", file, "--dry-run", "notification-worker", "manager", "--json"},
		{"notification-worker", "plan", "manager", "--dry-run", "--servers", file, "--json"},
		{"notification-worker", "qualify", "manager", "--dry-run", "--servers", file, "--json"},
		{"notification-worker", "test", "manager", "--provider", "telegram", "--dry-run", "--servers", file, "--json"},
		{"notification-worker", "--provider=telegram", "test", "manager", "--dry-run", "--servers", file, "--json"},
		{"notification-worker", "apply", "status", "--servers=" + file, "--json", "--dry-run"},
	} {
		command := exec.Command(binary, args...)
		// Any accidental SSH would fail: offline planning needs no external programs.
		command.Env = append(os.Environ(), "PATH="+dir)
		out, err := command.Output()
		if err != nil {
			t.Fatal(args, err)
		}
		var result struct {
			Version int      `json:"version"`
			State   string   `json:"state"`
			Missing []string `json:"missing"`
		}
		if err = json.Unmarshal(out, &result); err != nil || result.Version != 1 || result.State != "needs_input" || len(result.Missing) != 2 {
			t.Fatal(string(out), err)
		}
	}
	for _, op := range []string{"qualify", "activate", "run"} {
		command := exec.Command(binary, "backup-worker", op, "manager", "--dry-run", "--servers", file, "--json")
		command.Env = append(os.Environ(), "PATH="+dir)
		out, err := command.Output()
		if err != nil {
			t.Fatal(op, err)
		}
		var plan struct {
			Worker, Operation string
			Version           int
		}
		if err = json.Unmarshal(out, &plan); err != nil || plan.Version != 1 || plan.Worker != "backup-worker" || plan.Operation != op {
			t.Fatal(string(out), err)
		}
	}
	for _, kind := range []string{"backup-worker", "notification-worker"} {
		command := exec.Command(binary, "--servers", file, "--dry-run", kind, "source", "--json")
		command.Env = append(os.Environ(), "PATH="+dir)
		out, err := command.Output()
		if err != nil {
			t.Fatal("offline omission", err)
		}
		var result struct {
			Version int    `json:"version"`
			Omitted bool   `json:"omitted"`
			Code    string `json:"code"`
		}
		if err = json.Unmarshal(out, &result); err != nil || result.Version != 1 || !result.Omitted || result.Code != "declaration_omitted" {
			t.Fatal("omission preview", string(out), err)
		}
	}
	command := exec.Command(binary, "--servers", file, "--non-interactive", "notification-worker", "manager")
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "token_file") || !strings.Contains(string(output), "chat_id_file") {
		t.Fatal("unattended command did not report missing fields", string(output), err)
	}
	command = exec.Command(binary, "--servers", file, "--non-interactive", "notification-worker", "qualify", "manager", "--json")
	command.Env = append(os.Environ(), "PATH="+dir)
	output, err = command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "operator observations") {
		t.Fatal("unattended qualification prompted or ran SSH", string(output), err)
	}
	v2 := filepath.Join(dir, "server-v2.yaml")
	if err = os.WriteFile(v2, []byte("version: 2\nservers:\n  manager: {host: example.test, user: deploy}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--servers", v2, "--dry-run", "backup-worker", "activate", "manager"},
		{"--servers", v2, "--dry-run", "backup-worker", "run", "manager"},
		{"--servers", file, "--dry-run", "backup-worker", "activate", "--config", "/private/runtime.json", "manager"},
	} {
		command = exec.Command(binary, args...)
		command.Env = append(os.Environ(), "PATH="+dir)
		output, err = command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "[DRY] backup-worker "+args[4]) {
			t.Fatal("legacy worker routing changed", string(output), err)
		}
	}
	probe, err := exec.Command(binary, "notification-worker", "__managed-bootstrap-probe").Output()
	if err != nil {
		t.Fatal("native bootstrap capability", err)
	}
	var capability struct {
		Bootstrap int `json:"bootstrap"`
	}
	if err = json.Unmarshal(probe, &capability); err != nil || capability.Bootstrap != 1 {
		t.Fatal("bootstrap capability missing", string(probe), err)
	}
	var request bytes.Buffer
	private := []byte(`{"version":1,"worker":"invalid","inputs":{"token":"Zml4dHVyZS1zZWNyZXQ="}}`)
	if err = binaryWriteFrame(&request, private); err != nil {
		t.Fatal(err)
	}
	control := exec.Command(binary, "__worker_control")
	control.Stdin = &request
	reply, err := control.Output()
	if err != nil {
		t.Fatal("native control response", err)
	}
	if len(reply) < 4 || int(binarypkg.BigEndian.Uint32(reply[:4])) != len(reply)-4 || bytes.Contains(reply, []byte("fixture-secret")) || bytes.Contains(reply, []byte("Zml4dHVyZS1zZWNyZXQ=")) {
		t.Fatal("invalid or unredacted native control response")
	}
	var failure struct {
		Version int    `json:"version"`
		Code    string `json:"code"`
	}
	if err = json.Unmarshal(reply[4:], &failure); err != nil || failure.Version != 1 || failure.Code != "operation_failed" {
		t.Fatal("control rejected request incorrectly", string(reply[4:]), err)
	}
	after, _ := os.ReadFile(file)
	if string(after) != body {
		t.Fatal("preview changed inventory")
	}
}

func binaryWriteFrame(b *bytes.Buffer, body []byte) error {
	if err := binarypkg.Write(b, binarypkg.BigEndian, uint32(len(body))); err != nil {
		return err
	}
	_, err := b.Write(body)
	return err
}
