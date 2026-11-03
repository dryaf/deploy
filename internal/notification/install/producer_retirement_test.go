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

func TestProducerRetirementCLI(t *testing.T) {
	spec, _, _, id := managedFixture(t)
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := worker.OpenManagedVerification(spec.State, c.Worker, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = prepared.Revoke(context.Background(), c.Worker.Producers[0].Producer, "one"); err != nil {
		t.Fatal(err)
	}
	prepared.Close()
	args := []string{"producer-retire", "--local", "--config", spec.RuntimeFile, "--state-dir", spec.State, "--producer", c.Worker.Producers[0].Producer.ID, "--reason", "Disposable source decommissioned"}
	var out bytes.Buffer
	if err = runProducerRetirementCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	var preview worker.ProducerRetirementReview
	if err = json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.Eligible || preview.Applied || preview.Retirement != nil {
		t.Fatal(preview)
	}
	s, err := worker.OpenManagedVerification(spec.State, c.Worker, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = runProducerRetirementCLI(append(args, "--apply"), &out); err == nil {
		t.Fatal("CLI bypassed runtime lease")
	}
	s.Close()
	out.Reset()
	if err = runProducerRetirementCLI(append(args, "--apply"), &out); err != nil {
		t.Fatal(err)
	}
	var applied worker.ProducerRetirementReview
	if err = json.Unmarshal(out.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || applied.Retirement == nil || applied.EvidenceHash != preview.EvidenceHash {
		t.Fatal(applied)
	}
	out.Reset()
	if err = runProducerRetirementCLI(append(args, "--apply"), &out); err != nil {
		t.Fatal(err)
	}
	var again worker.ProducerRetirementReview
	if err = json.Unmarshal(out.Bytes(), &again); err != nil {
		t.Fatal(err)
	}
	if *again.Retirement != *applied.Retirement {
		t.Fatal("CLI retry changed decision")
	}
	s, err = worker.OpenManagedVerification(spec.State, c.Worker, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, err := s.Health(context.Background())
	if err != nil || len(h.RetiredProducers) != 1 {
		t.Fatal(h, err)
	}
}

func TestProducerRetirementCLIDryRunAndValidation(t *testing.T) {
	spec, _, _, _ := managedFixture(t)
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "absent-state")
	args := []string{"producer-retire", "--local", "--config", spec.RuntimeFile, "--state-dir", missing, "--producer", c.Worker.Producers[0].Producer.ID, "--reason", "Decommissioned", "--apply"}
	old := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = old }()
	var out bytes.Buffer
	if err = runProducerRetirementCLI(args, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("no producer retired")) {
		t.Fatal(out.String())
	}
	if _, err = os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("dry-run created state", err)
	}
	for _, extra := range [][]string{{"--reason", ""}, {"--producer", "not-enrolled"}, {"--actor", "somebody"}, {"--local=false"}, {"--reason", "two\nlines"}} {
		if err = runProducerRetirementCLI(append(append([]string{}, args...), extra...), &out); err == nil {
			t.Fatal("invalid retirement accepted", extra)
		}
	}
}
