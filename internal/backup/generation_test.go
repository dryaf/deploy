//go:build linux

package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/backup/source"
	deployinventory "github.com/dryaf/deploy/internal/inventory"
	"github.com/dryaf/deploy/internal/quadlet"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupReviewInitialGenerationPublicationCanResume(t *testing.T) {
	_, _, attempt, header, _ := archiveFixture(t)
	home := t.TempDir()
	root := filepath.Join(home, ".local/state/deploy/backup-source")
	if err := os.MkdirAll(filepath.Join(root, "generations"), 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := safefs.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	identity := source.SourceIdentity{Schema: 1, ID: attempt.SourceID, Workers: map[string]source.SourceAuthorization{}, Generations: map[string]string{}}
	if err = dir.WriteJSON("identity.json", identity, false); err != nil {
		t.Fatal(err)
	}
	// Exact on-disk boundary after the generation file is durable but before the identity ledger is updated.
	generation := source.SourceGeneration{Schema: 1, Generation: testutil.ID(t, "g"), BindingHash: attempt.BindingHash, Metadata: header.Metadata}
	if err = dir.WriteJSON("generations/"+safefs.LockName(attempt.Service)+".json", generation, false); err != nil {
		t.Fatal(err)
	}
	marker, err := quadlet.EncodeMetadata(header.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	unit := marker + "[Container]\nImage=unused\n"
	path := filepath.Join(home, ".config/containers/systemd", attempt.Service+".container")
	owner := deployinventory.InventoryOwner{Schema: 1, Service: attempt.Service, Target: header.Metadata.TargetDir, Mode: header.Metadata.Mode, Current: "release-1"}
	raw := deployinventory.InventoryProbeResult{Schema: 1, UID: os.Getuid(), Complete: true, Files: []deployinventory.InventoryFile{{Path: path, Name: attempt.Service + ".container", Content: unit, Evidence: &deployinventory.InventoryEvidence{Owner: owner, TargetOwner: owner, Current: owner, CandidateSHA256: fmtDigest([]byte(unit))}}}, Units: map[string]map[string]string{attempt.Service + ".service": {"SourcePath": path, "LoadState": "loaded", "ActiveState": "inactive"}}}
	body, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(home, "inventory.json")
	if err = os.WriteFile(payload, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "python3"), []byte("#!/bin/sh\ncat \"$REVIEW_INVENTORY\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home+":"+os.Getenv("PATH"))
	t.Setenv("REVIEW_INVENTORY", payload)
	inventory, err := source.CollectInventory(home, dir, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory.Instances) != 1 {
		t.Fatalf("invalid fixture: %+v", inventory)
	}
	if !inventory.Instances[0].Eligible {
		t.Fatalf("interrupted first enrollment cannot reach generation repair on retry: %+v", inventory.Instances[0])
	}
	worker := testutil.ID(t, "w")
	identity.Workers[worker] = source.SourceAuthorization{Enabled: true}
	testutil.Must(t, dir.WriteJSON("identity.json", identity, true))
	lock, err := dir.Lock("policy.lock", true, true)
	testutil.Must(t, err)
	lock.Close()
	actual, err := source.SourceGenerationForExport(dir, identity, worker, header.Metadata, attempt)
	testutil.Must(t, err)
	testutil.Must(t, dir.ReadJSON("identity.json", &identity))
	if actual != generation.Generation || identity.Generations[attempt.Service] != actual {
		t.Fatal("retry replaced the enrolled generation")
	}
	identity.Generations[attempt.Service] = testutil.ID(t, "g")
	testutil.Must(t, dir.WriteJSON("identity.json", identity, true))
	if _, err = source.SourceGenerationForExport(dir, identity, worker, header.Metadata, attempt); err == nil {
		t.Fatal("conflicting ledger was overwritten")
	}
}
