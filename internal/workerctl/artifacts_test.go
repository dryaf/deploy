package workerctl

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A minimal ELF with an executable load segment exercises artifact selection;
// native execution/build tests separately verify real release helpers.
func elfFixture(t *testing.T, arch string) Artifact {
	t.Helper()
	body := make([]byte, 121)
	copy(body, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(body[16:], 2)
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(body[18:], machine)
	binary.LittleEndian.PutUint32(body[20:], 1)
	binary.LittleEndian.PutUint16(body[52:], 64)
	binary.LittleEndian.PutUint64(body[24:], 0x400078)
	binary.LittleEndian.PutUint64(body[32:], 64)
	binary.LittleEndian.PutUint16(body[54:], 56)
	binary.LittleEndian.PutUint16(body[56:], 1)
	binary.LittleEndian.PutUint32(body[64:], 1)
	binary.LittleEndian.PutUint32(body[68:], 5)
	binary.LittleEndian.PutUint64(body[80:], 0x400000)
	binary.LittleEndian.PutUint64(body[96:], 121)
	binary.LittleEndian.PutUint64(body[104:], 121)
	binary.LittleEndian.PutUint64(body[112:], 4096)

	file := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(file, body, 0700); err != nil {
		t.Fatal(err)
	}
	return Artifact{File: file, SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}
}
func TestArtifactPinsAndArchitecture(t *testing.T) {
	a := elfFixture(t, "arm64")
	if err := VerifyArtifact(a, "arm64", true); err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(a, "amd64", true); err == nil {
		t.Fatal("wrong architecture accepted")
	}
	raw, _ := os.ReadFile(a.File)
	raw[63] = 1
	os.WriteFile(a.File, raw, 0700)
	if err := VerifyArtifact(a, "arm64", true); err == nil {
		t.Fatal("changed artifact accepted")
	}
}
func TestCatalogRejectsEscapesAndFloatingImages(t *testing.T) {
	for _, entry := range []ArchitectureArtifacts{
		{Helper: Artifact{File: "../helper", SHA256: strings.Repeat("a", 64)}},
		{Helper: Artifact{File: "/helper", SHA256: strings.Repeat("a", 64)}},
		{Helper: Artifact{File: "helper", SHA256: strings.Repeat("a", 64)}, BackupImage: "example.test/worker:latest"},
	} {
		c := Catalog{Version: 1, Release: "test", Linux: map[string]ArchitectureArtifacts{"arm64": entry}}
		body, _ := json.Marshal(c)
		file := filepath.Join(t.TempDir(), "catalog.json")
		os.WriteFile(file, body, 0600)
		if _, err := ReadCatalog(file); err == nil {
			t.Fatal("invalid catalog accepted")
		}
	}
}

func TestCatalogRejectsDuplicateJSONKeys(t *testing.T) {
	file := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(file, []byte(`{"version":1,"version":1,"release":"test","linux":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadCatalog(file); err == nil {
		t.Fatal("duplicate catalog keys accepted")
	}
}
