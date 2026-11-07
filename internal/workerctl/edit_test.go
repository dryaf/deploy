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
)

func TestConfigureJSONUnchangedAndReadonlyCandidate(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	file := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(file, []byte(backupOnlyInventory), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	remote := &countingInspector{}
	c := CLI{Out: &out, Inspector: remote}
	if err := c.Run(context.Background(), "backup-worker", []string{"configure", "manager", "--json"}, Options{Inventory: file}); err != nil {
		t.Fatal(err)
	}
	var unchanged Plan
	if err := json.Unmarshal(out.Bytes(), &unchanged); err != nil || unchanged.Version != 1 || unchanged.State != "unchanged" {
		t.Fatal(out.String(), err)
	}
	before := []byte(strings.Replace(backupOnlyInventory, "sources: [source]", "sources: []", 1))
	if err := os.WriteFile(file, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0400); err != nil {
		t.Fatal(err)
	}
	c.Prompt = StreamPrompt{Input: bufio.NewReader(strings.NewReader("source\n"))}
	out.Reset()
	if err := c.Run(context.Background(), "backup-worker", []string{"configure", "manager", "--json"}, Options{Inventory: file}); err != nil {
		t.Fatal(err)
	}
	var candidate struct {
		Version                int
		State, Code, Candidate string
	}
	if err := json.Unmarshal(out.Bytes(), &candidate); err != nil || candidate.State != "candidate" || !strings.Contains(candidate.Candidate, "- source") {
		t.Fatal(out.String(), err)
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(before, after) || remote.Calls != 0 {
		t.Fatal("configuration candidate wrote or contacted target", err)
	}
}

func TestInventoryEditPreservesCommentsAndConflicts(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	file := inventoryFixture(t)
	e, err := OpenInventoryEdit(file)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Set("manager", []string{"notification_worker", "providers", "telegram", "token_input"}, "./telegram-token"); err != nil {
		t.Fatal(err)
	}
	if err = e.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	for _, want := range []string{"# keep this comment", "# keep provider comment", "token_input: ./telegram-token", "host: example.com"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Fatalf("lost %s: %s", want, raw)
		}
	}
	if err = e.Set("manager", []string{"notification_worker", "providers", "telegram", "chat_id_input"}, "./chat"); err != nil {
		t.Fatal(err)
	}
	external := append(raw, []byte("# concurrent edit\n")...)
	if err = os.WriteFile(file, external, 0600); err != nil {
		t.Fatal(err)
	}
	if err = e.Save(); err == nil {
		t.Fatal("overwrote concurrent edit")
	}
	actual, _ := os.ReadFile(file)
	if !bytes.Equal(actual, external) {
		t.Fatal("concurrent content changed")
	}
}

func TestConfigurePersistsReferencesAndNeverContactsTarget(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	file := inventoryFixture(t)
	remote := &countingInspector{}
	var out bytes.Buffer
	c := CLI{Inspector: remote, Out: &out, Prompt: StreamPrompt{Input: bufio.NewReader(strings.NewReader("local:./chat\nlocal:./token\n"))}}
	if err := c.Run(context.Background(), "notification-worker", []string{"configure", "manager"}, Options{Inventory: file}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	if !bytes.Contains(raw, []byte("token_input: ./token")) || !bytes.Contains(raw, []byte("chat_id_input: ./chat")) {
		t.Fatal(string(raw))
	}
	if remote.Calls != 0 {
		t.Fatal("configure deployed")
	}
	c.Prompt = StreamPrompt{Input: bufio.NewReader(strings.NewReader(""))}
	if err := c.Run(context.Background(), "notification-worker", []string{"configure", "manager"}, Options{Inventory: file}); err != nil {
		t.Fatal("configured reference prompted again", err)
	}
}

func TestConfigureCancellationAndReadonlyCandidate(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	file := inventoryFixture(t)
	before, _ := os.ReadFile(file)
	var out bytes.Buffer
	c := CLI{Out: &out, Prompt: StreamPrompt{Input: bufio.NewReader(strings.NewReader("local:./chat\n"))}}
	if err := c.Run(context.Background(), "notification-worker", []string{"configure", "manager"}, Options{Inventory: file}); err == nil {
		t.Fatal("EOF accepted")
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("partial configuration saved")
	}
	if err := os.Chmod(file, 0400); err != nil {
		t.Fatal(err)
	}
	c.Prompt = StreamPrompt{Input: bufio.NewReader(strings.NewReader("local:./chat\nlocal:./token\n"))}
	if err := c.Run(context.Background(), "notification-worker", []string{"configure", "manager"}, Options{Inventory: file}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "token_input") {
		t.Fatal("no candidate")
	}
	after, _ = os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("readonly inventory overwritten")
	}
	link := filepath.Join(filepath.Dir(file), "linked.yaml")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenInventoryEdit(link); err == nil {
		t.Fatal("accepted symlink edit")
	}
}
