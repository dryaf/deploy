package workerctl

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type secretPrompter struct{ calls int }

func (p *secretPrompter) Ask(ctx context.Context, label string, secret bool) (string, error) {
	p.calls++
	if p.calls%2 == 1 {
		if secret {
			return "", fmt.Errorf("wrong prompt")
		}
		return "new", nil
	}
	if !secret {
		return "", fmt.Errorf("secret would be echoed")
	}
	return "sensitive-test-value", nil
}
func TestGuidedSecretsStayOutOfInventoryAndOutput(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	configRoot := t.TempDir()
	if err := os.Chmod(configRoot, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	file := inventoryFixture(t)
	prompt := &secretPrompter{}
	var out bytes.Buffer
	c := CLI{Prompt: prompt, Out: &out, Err: &out}
	if err := c.Run(context.Background(), "notification-worker", []string{"configure", "manager"}, Options{Inventory: file}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	if bytes.Contains(raw, []byte("sensitive-test-value")) || strings.Contains(out.String(), "sensitive-test-value") {
		t.Fatal("secret disclosed")
	}
	files, err := filepath.Glob(filepath.Join(configRoot, "deploy/worker-inputs/*/manager/*"))
	if err != nil || len(files) != 2 {
		t.Fatal(files, err)
	}
	for _, f := range files {
		value, err := ReadCredentialInput(f)
		if err != nil || string(value) != "sensitive-test-value" {
			t.Fatal("private input unavailable", err)
		}
	}
}
func TestCredentialInputRejectsPublicSymlinkAndOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("value"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCredentialInput(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCredentialInput(file); err == nil {
		t.Fatal("public secret accepted")
	}
	if err := os.Chmod(file, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	os.Symlink(file, link)
	if _, err := ReadCredentialInput(link); err == nil {
		t.Fatal("symlink accepted")
	}
	os.WriteFile(file, bytes.Repeat([]byte("x"), 4097), 0600)
	if _, err := ReadCredentialInput(file); err == nil {
		t.Fatal("oversized input accepted")
	}
}
