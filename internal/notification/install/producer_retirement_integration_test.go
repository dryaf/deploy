//go:build notificationintegration

package install

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
)

func TestProducerRetirementNativeExecutable(t *testing.T) {
	spec, _, _, id := managedFixture(t)
	c, err := ReadRuntime(spec.RuntimeFile)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Worker.Producers[0].Producer
	s, err := worker.OpenManagedVerification(spec.State, c.Worker, id)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Revoke(context.Background(), p, "one")
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "deploy")
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = repo
	if raw, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal(err, string(raw))
	}
	args := []string{"notification-worker", "producer-retire", "--local", "--config", spec.RuntimeFile, "--state-dir", spec.State, "--producer", p.ID, "--reason", "Native fixture decommissioned"}
	run := func(extra ...string) worker.ProducerRetirementReview {
		t.Helper()
		raw, err := process.LimitedOutput(exec.Command(binary, append(append([]string{}, args...), extra...)...), 30*time.Second, 16384)
		if err != nil {
			t.Fatal(err, string(raw))
		}
		var v worker.ProducerRetirementReview
		if err = json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err, string(raw))
		}
		return v
	}
	preview := run()
	if !preview.Eligible || preview.Applied || preview.Retirement != nil {
		t.Fatal(preview)
	}
	first := run("--apply")
	if !first.Applied || first.Retirement == nil || first.EvidenceHash != preview.EvidenceHash {
		t.Fatal(first)
	}
	second := run("--apply")
	if second.Retirement == nil || *first.Retirement != *second.Retirement {
		t.Fatal("retry changed durable decision", second)
	}
	if raw, err := process.LimitedOutput(exec.Command(binary, append(args, "--apply", "--reason", "Conflicting decision")...), 30*time.Second, 16384); err == nil {
		t.Fatal("conflicting decision accepted", string(raw))
	}
	s, err = worker.OpenManagedVerification(spec.State, c.Worker, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Stage(p, "replacement"); err == nil {
		t.Fatal("native retirement did not fence ownership")
	}
	h, err := s.Health(context.Background())
	if err != nil || len(h.RetiredProducers) != 1 || h.RetiredProducers[0] != p.ID {
		t.Fatal(h, err)
	}
}
