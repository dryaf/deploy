package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
)

type WorkerSource struct {
	Alias      string `json:"alias"`
	Host       string `json:"host"`
	User       string `json:"user"`
	Port       int    `json:"port"`
	KeyFile    string `json:"key_file"`
	KnownHosts string `json:"known_hosts"`
	SourceID   string `json:"source_id"`
}

func restrictedSourceCommand(s WorkerSource, connect time.Duration) *exec.Cmd {
	seconds := int(connect.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	args := []string{"-F", "/dev/null", "-T", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "StrictHostKeyChecking=yes", "-o", "GlobalKnownHostsFile=/dev/null", "-o", "UserKnownHostsFile=" + s.KnownHosts, "-o", "ClearAllForwardings=yes", "-o", "ForwardAgent=no", "-o", "PermitLocalCommand=no", "-o", "ProxyCommand=none", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ConnectionAttempts=1", "-o", "ConnectTimeout=" + strconv.Itoa(seconds), "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=2", "-i", s.KeyFile, "-p", strconv.Itoa(s.Port), "-l", s.User, s.Host}
	cmd := exec.Command("ssh", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=/nonexistent"}
	return cmd
}

func sourceRequestInput(request SourceRequest) io.Reader {
	body, _ := json.Marshal(request)
	var input bytes.Buffer
	_ = protocol.WriteBackupFrame(&input, body)
	return &input
}

func QueryRestrictedSource(ctx context.Context, s WorkerSource, policy config.WorkerTimeouts, operation string, dest any) error {
	cmd := restrictedSourceCommand(s, config.Budget(policy.Connect, "10s"))
	cmd.Stdin = sourceRequestInput(SourceRequest{Protocol: 1, Operation: operation})
	var stdout, stderr process.Buffer
	stdout.Limit = protocol.MaxBackupHeader + 4
	stderr.Limit = 4096
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := process.RunContext(ctx, cmd, config.Budget(policy.Inventory, "30s"), config.Budget(policy.ShutdownGrace, "10s")); err != nil {
		return fmt.Errorf("restricted source %s request failed", operation)
	}
	reader := bytes.NewReader(stdout.Bytes())
	body, err := protocol.ReadBackupFrame(reader)
	if err != nil {
		return err
	}
	if reader.Len() != 0 {
		return fmt.Errorf("trailing source protocol bytes")
	}
	return protocol.StrictBackupJSON(body, dest)
}

func FetchRestrictedSnapshot(ctx context.Context, source WorkerSource, policy config.BackupWorkerConfig, attempt protocol.SnapshotAttempt, file *os.File) (protocol.SnapshotHeader, error) {
	var header protocol.SnapshotHeader
	cmd := restrictedSourceCommand(source, config.Budget(policy.Timeouts.Connect, "10s"))
	cmd.Stdin = sourceRequestInput(SourceRequest{Protocol: 1, Operation: "snapshot", Attempt: &attempt})
	var stderr process.Buffer
	stderr.Limit = 4096
	cmd.Stderr = &stderr
	pipe, pipeWriter, err := os.Pipe()
	if err != nil {
		return header, err
	}
	cmd.Stdout = pipeWriter
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	var processErr error
	go func() {
		defer close(done)
		defer pipeWriter.Close()
		processErr = process.RunContext(ctx, cmd, config.Budget(policy.Timeouts.Snapshot, "5m")+config.Budget(policy.Timeouts.Transfer, "10m"), config.Budget(policy.Timeouts.ShutdownGrace, "10s"))
	}()
	defer func() { cancel(); pipe.Close(); <-done }()
	if err = pipe.SetReadDeadline(time.Now().Add(config.Budget(policy.Timeouts.Connect, "10s") + config.Budget(policy.Timeouts.Snapshot, "5m"))); err != nil {
		return header, err
	}
	header, err = protocol.ReadSnapshotTransferPhased(pipe, file, attempt, policy.MaxSnapshotBytes, func() error {
		return pipe.SetReadDeadline(time.Now().Add(config.Budget(policy.Timeouts.Transfer, "10m")))
	})
	if err != nil {
		return header, err
	}
	<-done
	if processErr != nil {
		return header, fmt.Errorf("source transfer process did not exit successfully")
	}
	return header, nil
}
