//go:build notificationintegration

package install

import (
	"context"
	"encoding/json"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestSignalLinkNativeExecutable(t *testing.T) {
	base := t.TempDir()
	binary := filepath.Join(base, "deploy")
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/deploy")
	build.Dir = repo
	if output, err := process.LimitedOutput(build, time.Minute, 16384); err != nil {
		t.Fatal("build", err, string(output))
	}
	state := filepath.Join(base, "account")
	if err = os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf . >> \"$2/fixture-calls\"\nprintf 'synthetic-private-link-uri\\n'\nexit 1\n"
	if err = os.WriteFile(filepath.Join(base, "signal-cli"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	id, _ := safefs.NewID("n")
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(binary, append([]string{"notification-worker"}, args...)...)
		cmd.Env = append(os.Environ(), "PATH="+base+":"+os.Getenv("PATH"))
		var out, stderr process.Buffer
		out.Limit, stderr.Limit = 16384, 4096
		cmd.Stdout, cmd.Stderr = &out, &stderr
		err := process.Run(cmd, 15*time.Second, time.Second)
		if strings.Contains(stderr.String(), "synthetic-private-link-uri") {
			t.Fatal("linking output leaked into errors")
		}
		return out.Bytes(), err
	}
	probe, e := run("__signal-account-probe")
	var capability struct {
		Version int `json:"version"`
	}
	if e != nil || json.Unmarshal(probe, &capability) != nil || capability.Version != signalaccount.MaintenanceVersion {
		t.Fatal("maintenance capability", string(probe), e)
	}
	link := []string{"__signal-link", "--local", "--state-dir", state, "--operation", id, "--name", "fixture"}
	if output, err := run(link...); err == nil || !strings.Contains(string(output), "synthetic-private-link-uri") {
		t.Fatal("expected synthetic failed link", err, string(output))
	}
	if _, err = run(link...); err == nil {
		t.Fatal("uncertain link replayed")
	}
	output, err := run("signal-link-status", "--local", "--state-dir", state)
	if err != nil {
		t.Fatal(err, string(output))
	}
	var status signalaccount.Status
	if err = json.Unmarshal(output, &status); err != nil || status.Operation != id || status.State != "uncertain" || status.RuntimeAllowed {
		t.Fatal(status, err)
	}
	output, err = run("signal-link-resolve", "--local", "--state-dir", state, "--operation", id, "--accept-account-state", "--reason", "Reviewed synthetic account after fixture failure")
	if err != nil {
		t.Fatal(err, string(output))
	}
	if err = json.Unmarshal(output, &status); err != nil || status.State != "reviewed" || !status.RuntimeAllowed {
		t.Fatal(status, err)
	}
	if output, err = run(link...); err != nil || strings.Contains(string(output), "synthetic-private-link-uri") {
		t.Fatal("reviewed attempt executed again", err, string(output))
	}
	count, err := os.ReadFile(filepath.Join(state, "fixture-calls"))
	if err != nil || string(count) != "." {
		t.Fatal("wrong CLI attempt count", string(count), err)
	}
	// The real executable shares the gate with its coordinator. A failed native
	// child is reviewed locally, then exact resume restores the prior activity.
	coordinated := filepath.Join(base, "coordinated-account")
	if err = os.Mkdir(coordinated, 0700); err != nil {
		t.Fatal(err)
	}
	op, _ := safefs.NewID("n")
	svc := &nativeSignalService{path: coordinated, binding: protocol.Hash("fixture-service")}
	if err = svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if svc.lease != nil {
			svc.lease.Close()
		}
	}()
	request := signalaccount.ServiceRequest{Operation: op, Name: "fixture", Binding: svc.binding}
	launch := func(context.Context) error {
		_, e := run("__signal-link", "--local", "--state-dir", coordinated, "--operation", op, "--name", "fixture")
		return e
	}
	if _, err = signalaccount.MaintainLink(context.Background(), coordinated, request, svc, launch); err == nil || svc.active {
		t.Fatal("uncertain child restored service", err)
	}
	output, err = run("signal-service-status", "--local", "--state-dir", coordinated)
	var maintenance signalaccount.MaintenanceStatus
	if err != nil || json.Unmarshal(output, &maintenance) != nil || maintenance.Phase != "stopped" || maintenance.RuntimeAllowed {
		t.Fatal(string(output), err)
	}
	output, err = run("signal-link-resolve", "--local", "--state-dir", coordinated, "--operation", op, "--accept-account-state", "--reason", "Reviewed synthetic coordinated attempt")
	if err != nil || json.Unmarshal(output, &status) != nil || status.RuntimeAllowed {
		t.Fatal("review bypassed coordinator", string(output), err)
	}
	if _, err = signalaccount.MaintainLink(context.Background(), coordinated, request, svc, launch); err != nil || !svc.active {
		t.Fatal("resume", err)
	}
	count, err = os.ReadFile(filepath.Join(coordinated, "fixture-calls"))
	if err != nil || string(count) != "." {
		t.Fatal("coordinated native child repeated", string(count), err)
	}

}

type nativeSignalService struct {
	path, binding string
	active        bool
	lease         *signalaccount.Lease
}

func (s *nativeSignalService) Inspect(context.Context) (signalaccount.ServiceState, error) {
	return signalaccount.ServiceState{Binding: s.binding, Active: s.active}, nil
}
func (s *nativeSignalService) Stop(context.Context) error {
	if s.lease != nil {
		s.lease.Close()
		s.lease = nil
	}
	s.active = false
	return nil
}
func (s *nativeSignalService) Start(context.Context) error {
	l, e := signalaccount.AcquireRuntime(s.path)
	if e != nil {
		return e
	}
	s.lease = l
	s.active = true
	return nil
}
