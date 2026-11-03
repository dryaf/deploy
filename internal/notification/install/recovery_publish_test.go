package install

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/ui"
)

func publishCLIArguments(t *testing.T) (string, []string) {
	t.Helper()
	state, flags, raw := publicationVerifyArguments(t)
	var old worker.RecoveryPublicationImage
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	c, err := ReadRuntime(flags[2])
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, e := range c.Worker.Producers {
		if _, err = worker.FenceRecoveryOwnership(ctx, state, c.Worker, e.Producer); err != nil {
			t.Fatal(err)
		}
	}
	v, err := worker.ReviewRecovery(ctx, state, c.Worker, old.Operation)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := worker.CheckpointRecovery(ctx, state, c.Worker, old.Operation, v.ReviewHash)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := worker.VerifyRecoveryCheckpoint(ctx, state, c.Worker, old.Operation, v.ReviewHash, cp.CheckpointHash)
	if err != nil {
		t.Fatal(err)
	}
	image, err := worker.PrepareRecoveryPublication(ctx, state, c.Worker, old.Operation, v.ReviewHash, cp.CheckpointHash, proof.ProofHash)
	if err != nil {
		t.Fatal(err)
	}
	return state, []string{"--local", "--config", flags[2], "--state-dir", state, "--operation", old.Operation, "--review-hash", v.ReviewHash, "--checkpoint-hash", cp.CheckpointHash, "--proof-hash", proof.ProofHash, "--image-hash", image.ImageHash}
}

func TestRecoveryPublishCLI(t *testing.T) {
	state, args := publishCLIArguments(t)
	before, err := os.ReadFile(filepath.Join(state, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	var one, two bytes.Buffer
	if err = runRecoveryPublishCLI(args, &one); err != nil {
		t.Fatal(err)
	}
	if err = runRecoveryPublishCLI(args, &two); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(one.Bytes(), two.Bytes()) {
		t.Fatal("publication replay changed")
	}
	var result worker.RecoveryPublicationResult
	if err = json.Unmarshal(one.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.CanonicalInstalled || result.DeliveryEnabled || result.ExecutionEnabled {
		t.Fatal(result)
	}
	old, err := os.ReadFile(filepath.Join(state, "recovery-publication-exchange.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old, before) {
		t.Fatal("CLI publication lost original")
	}
	if err = os.WriteFile(filepath.Join(state, "recovery-publication-complete.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	one.Reset()
	if err = runRecoveryPublishCLI(args, &one); err == nil || one.Len() != 0 {
		t.Fatal("damaged completion emitted success", err, one.String())
	}
}

func TestRecoveryPublishCLIFlagsAndDryRun(t *testing.T) {
	state, args := publishCLIArguments(t)
	for _, a := range [][]string{args[1:], args[:len(args)-2], append(append([]string(nil), args...), "--activate")} {
		var out bytes.Buffer
		if err := runRecoveryPublishCLI(a, &out); err == nil || out.Len() != 0 {
			t.Fatal("invalid publication flags accepted", err)
		}
	}
	saved := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = saved }()
	var out bytes.Buffer
	if err := runRecoveryPublishCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("[DRY]")) {
		t.Fatal(out.String())
	}
	if _, err := os.Stat(filepath.Join(state, "recovery-publication.json")); !os.IsNotExist(err) {
		t.Fatal("dry run froze recovery", err)
	}
}
