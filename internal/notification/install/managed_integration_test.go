//go:build notificationintegration

package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/client"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/worker"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type executableManagedServices struct {
	*fakeManagedServices
	root string
	cmd  *exec.Cmd
	done chan error
	log  *os.File
}

func (f *executableManagedServices) Stop(ctx context.Context) error {
	if f.cmd != nil {
		if err := f.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return err
		}
		select {
		case err := <-f.done:
			// The wait result is consumed even on failure. Clear the process
			// before returning so cleanup cannot wait on the same channel twice.
			f.cmd = nil
			f.log.Close()
			f.log = nil
			if err != nil {
				return fmt.Errorf("verification process exit: %w", err)
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			f.cmd.Process.Kill()
			<-f.done
			f.cmd = nil
			f.log.Close()
			f.log = nil
			return fmt.Errorf("verification process did not stop")
		}
	}
	return f.fakeManagedServices.Stop(ctx)
}
func (f *executableManagedServices) EnableStart(ctx context.Context) error {
	d, err := safefs.Open(f.root)
	if err != nil {
		return err
	}
	defer d.Close()
	var m ManagedManifest
	if err = d.ReadJSON("installation.json", &m); err != nil {
		return err
	}
	f.log, err = os.OpenFile(filepath.Join(filepath.Dir(f.root), "runtime-output"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	f.cmd = exec.Command(m.Paths.Executable, "notification-worker", "__managed-serve", "--local", "--root", m.Root, "--revision", m.Revision)
	f.cmd.Stdout = f.log
	f.cmd.Stderr = f.log
	if err = f.cmd.Start(); err != nil {
		f.log.Close()
		f.cmd = nil
		return err
	}
	f.done = make(chan error, 1)
	cmd := f.cmd
	done := f.done
	go func() { done <- cmd.Wait() }()
	return f.fakeManagedServices.EnableStart(ctx)
}
func (f *executableManagedServices) Verify(ctx context.Context, m ManagedManifest) error {
	return (userManagedServices{}).Verify(ctx, m)
}

func TestManagedNativeExecutableLifecycle(t *testing.T) {
	spec, i, f, id := managedFixture(t)
	qualificationFixtureInputs(t, spec.RuntimeFile)
	// Keep the actual Unix socket pathname below sockaddr_un's limit.
	short, err := os.MkdirTemp("", "managed-native-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	oldState := spec.State
	spec.State = filepath.Join(short, "state")
	if err = os.Rename(oldState, spec.State); err != nil {
		t.Fatal(err)
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", spec.BinaryFile, "./cmd/deploy")
	build.Dir = repo
	if out, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal("build", err, string(out))
	}
	if err = os.Chmod(spec.BinaryFile, 0700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(b)
	native := &executableManagedServices{fakeManagedServices: f, root: spec.Root}
	i.services = native
	i.probe = probeManagedBinary
	t.Cleanup(func() {
		if native.cmd != nil {
			native.cmd.Process.Kill()
			select {
			case <-native.done:
			case <-time.After(5 * time.Second):
			}
			if native.log != nil {
				native.log.Close()
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	m, err := i.prepare(ctx, "setup", spec)
	if err != nil {
		out, _ := os.ReadFile(filepath.Join(filepath.Dir(spec.Root), "runtime-output"))
		t.Fatal(err, string(out))
	}
	if m.Identity != id {
		t.Fatal("identity changed")
	}
	// Exercise the real evidence CLI while the verification process runs. These
	// pending fixture checks are explicitly not live qualification assertions.
	template, err := process.LimitedOutput(exec.CommandContext(ctx, m.Paths.Executable, "notification-worker", "qualification-template", "--local", "--root", m.Root), 15*time.Second, 1<<20)
	if err != nil {
		t.Fatal("qualification template", err, string(template))
	}
	var report QualificationReport
	if err = json.Unmarshal(template, &report); err != nil {
		t.Fatal(err)
	}
	reportPath := writeQualificationReport(t, report)
	for _, args := range [][]string{
		{"notification-worker", "qualify", "--local", "--root", m.Root, "--report", reportPath},
		{"notification-worker", "qualification-status", "--local", "--root", m.Root},
	} {
		output, e := process.LimitedOutput(exec.CommandContext(ctx, m.Paths.Executable, args...), 15*time.Second, 1<<20)
		if e != nil {
			t.Fatal("qualification CLI", e, string(output))
		}
		var status QualificationStatus
		if e = json.Unmarshal(output, &status); e != nil || status.Record == nil || status.Record.Report.ID != report.ID || !status.BindingMatches || status.EvidenceComplete || status.Qualified || status.ActivationAllowed {
			t.Fatal(status, e)
		}
	}
	// Validate native syntax without installing anything into the real manager.
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := process.LimitedOutput(exec.CommandContext(ctx, analyze, "--user", "verify", "--man=no", filepath.Join(spec.UnitDirectory, managedUnit)), 15*time.Second, 16384); err != nil {
		t.Fatal("unit validation", err, string(out))
	}
	api := client.Client{Socket: m.Paths.Socket, Identity: id}
	c, err := ReadRuntime(m.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Worker.Producers[0].Producer
	now := time.Now().UTC()
	snapshot := protocol.Snapshot{Producer: p, Sequence: 1, BasisRevision: 1, ConfigRevision: "one", EvaluatedAt: now, Subjects: []protocol.Subject{{Key: "subject", Condition: "ok", Reason: "managed verification", ValidUntil: now.Add(time.Minute)}}}
	if _, err = api.Call(ctx, protocol.Request{Operation: "status.replace", Producer: p, Snapshot: &snapshot}, nil); err != nil {
		t.Fatal(err)
	}
	msg := protocol.Message{Producer: p, Sequence: 1, ID: "managed-report", Generation: "one", Subject: "subject", Kind: "report", Severity: "info", ObservedAt: now, ExpiresAt: now.Add(time.Minute), Text: "Synthetic managed report", Route: "ops", ReportKey: "day", ReportType: "daily", ReportRevision: 1, BasisRevision: 1}
	if _, err = api.Call(ctx, protocol.Request{Operation: "message.enqueue", Producer: p, Message: &msg}, nil); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"queue-inspect", "receipts-inspect"} {
		var output bytes.Buffer
		if err = runInspectionCLI([]string{command, "--local", "--state-dir", spec.State, "--socket", m.Paths.Socket, "--producer", p.ID, "--producer-epoch", p.Epoch}, &output); err != nil {
			t.Fatal(err)
		}
		if command == "queue-inspect" && !bytes.Contains(output.Bytes(), []byte(msg.ID)) {
			t.Fatal("missing inspected report", output.String())
		}
		if command == "receipts-inspect" && (!bytes.Contains(output.Bytes(), []byte(`"listed_through":0`)) || !bytes.Contains(output.Bytes(), []byte(`"acknowledged_through":0`))) {
			t.Fatal("inspection issued cursor", output.String())
		}
	}
	var h worker.Health
	if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &h); err != nil || !h.VerificationOnly || h.Condition == "ok" || h.InstallationRevision != m.Revision {
		t.Fatal(h, err)
	}
	// The real runtime must retain the installation lease for its whole lifetime.
	d, err := safefs.Open(spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if lease, e := d.Lock("installation.lock", true, false); e == nil {
		lease.Close()
		t.Fatal("runtime dropped installation lease")
	}
	d.Close()
	// Keep both real executable revisions distinct while preserving ELF behavior.
	candidate, err := os.ReadFile(spec.BinaryFile)
	if err != nil {
		t.Fatal(err)
	}
	candidate = append(candidate, []byte("native rollback integration revision")...)
	if err = os.WriteFile(spec.BinaryFile, candidate, 0700); err != nil {
		t.Fatal(err)
	}
	spec.BinarySHA256 = managedDigest(candidate)
	native.lostStart = true
	if _, err = i.prepare(ctx, "update", spec); err == nil {
		t.Fatal("lost start ack treated as completion")
	}
	updated, err := i.resume(ctx, spec.Root)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Identity != id || updated.Revision == m.Revision {
		t.Fatal("bad updated revision")
	}
	rolled, err := i.rollback(ctx, spec.Root, m.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.BinaryHash != m.BinaryHash || rolled.Revision == m.Revision || rolled.Revision == updated.Revision {
		t.Fatal("native rollback did not create selected release")
	}
	if _, err = api.Call(ctx, protocol.Request{Operation: "system.status"}, &h); err != nil || h.InstallationRevision != rolled.Revision || !h.VerificationOnly {
		t.Fatal("rollback runtime revision mismatch", h, err)
	}
	var job worker.Job
	if _, err = api.Call(ctx, protocol.Request{Operation: "message.get", Producer: p, EventID: msg.ID}, &job); err != nil || job.Tries != 0 || job.Attempt != "" {
		t.Fatal("durable report lost or dispatched", job, err)
	}
	// An operator can also choose to uninstall instead of resuming a start whose
	// acknowledgement was lost. The live child must stop and state must survive.
	native.lostStart = true
	if _, err = i.prepare(ctx, "update", spec); err == nil {
		t.Fatal("missing interrupted update")
	}
	if _, err = i.uninstall(ctx, spec.Root); err != nil {
		t.Fatal(err)
	}
	s, err := worker.OpenManagedVerification(spec.State, c.Worker, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Identity() != id {
		t.Fatal("uninstall changed identity")
	}
	job, err = s.Get(ctx, p, msg.ID)
	if err != nil || job.Tries != 0 {
		t.Fatal("uninstall changed pending obligation", job, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	testNativeMessageAdministration(t, spec.BinaryFile, m.Paths.Config, spec.State, p, msg.ID)
}
