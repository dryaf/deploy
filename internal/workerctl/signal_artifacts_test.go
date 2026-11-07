package workerctl

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSignalArtifactRequiresTargetNativePin(t *testing.T) {
	raw, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "native-cli")
	if err = os.WriteFile(path, raw, 0700); err != nil {
		t.Fatal(err)
	}
	a := Artifact{File: path, SHA256: inputDigest(raw)}
	if err = VerifySignalArtifact(a, runtime.GOARCH); err != nil {
		t.Fatal(err)
	}
	other := "arm64"
	if runtime.GOARCH == other {
		other = "amd64"
	}
	if err = VerifySignalArtifact(a, other); err == nil {
		t.Fatal("wrong architecture accepted")
	}
	if err = os.WriteFile(path, append(raw, 1), 0700); err != nil {
		t.Fatal(err)
	}
	if err = VerifySignalArtifact(a, runtime.GOARCH); err == nil {
		t.Fatal("mutated executable accepted")
	}
	text := []byte("#!/bin/sh\nexit 0\n")
	if err = os.WriteFile(path, text, 0700); err != nil {
		t.Fatal(err)
	}
	a.SHA256 = inputDigest(text)
	if err = VerifySignalArtifact(a, runtime.GOARCH); err == nil {
		t.Fatal("script accepted as native CLI")
	}
}
