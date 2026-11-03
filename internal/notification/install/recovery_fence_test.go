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

func TestRecoveryCLIFenceProducer(t *testing.T) {
	config, path, review := recoveryCLIFixture(t)
	args := []string{"recovery-fence-producer", "--local", "--config", config, "--state-dir", path, "--producer", review.Message.Producer.ID}
	var first bytes.Buffer
	if err := runRecoveryCLI(args, &first); err != nil {
		t.Fatal(err)
	}
	var proof worker.RecoveryOwnershipFence
	if err := json.Unmarshal(first.Bytes(), &proof); err != nil {
		t.Fatal(err)
	}
	if !proof.ReconciliationPending || proof.DeliveryEnabled || proof.Producer != review.Message.Producer {
		t.Fatal(proof)
	}
	var second bytes.Buffer
	if err := runRecoveryCLI(args, &second); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("retry changed fence")
	}
	for _, flags := range [][]string{{"--message", "unrelated"}, {"--reason", "unrelated"}, {"--action", "retry"}, {"--accept-duplicate-risk"}, {"--producer", "not-enrolled"}} {
		if err := runRecoveryCLI(append(append([]string{}, args...), flags...), &second); err == nil {
			t.Fatal("invalid flags accepted", flags)
		}
	}
	c, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	session, err := worker.OpenRecoveryOwnershipSession(context.Background(), path, c.Worker)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	completed, err := session.PrepareRecoveryOwnership(context.Background(), proof.Producer)
	if err != nil {
		t.Fatal(err)
	}
	if completed.PreviousHistory != proof.History {
		t.Fatal("CLI fence lost provenance")
	}
}

func TestRecoveryCLIFenceDryRunDoesNotCreateState(t *testing.T) {
	_, config := runtimeFixture(t)
	c, err := ReadRuntime(config)
	if err != nil {
		t.Fatal(err)
	}
	before := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = before }()
	var out bytes.Buffer
	path := filepath.Join(t.TempDir(), "nonexistent")
	if err := runRecoveryCLI([]string{"recovery-fence-producer", "--local", "--config", config, "--state-dir", path, "--producer", c.Worker.Producers[0].Producer.ID}, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dry-run accessed or created state", err)
	}
}
