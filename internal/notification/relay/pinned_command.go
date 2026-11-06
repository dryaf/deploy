package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/dryaf/deploy/internal/safefs"
)

// RunPinnedCommand executes a private native deploy supervisor and Signal CLI
// through retained file descriptors. Its caller keeps service/account admission
// and both executable identities checked. Cancellation is sent to the supervisor
// alone; it owns termination and joining of its children before this returns.
func RunPinnedCommand(ctx context.Context, helper, cli, lease *os.File, request CommandRequest, output io.Writer) error {
	if helper == nil || cli == nil || lease == nil {
		return fmt.Errorf("pinned Signal command descriptors required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateCommandExecutable(cli, request.CLIHash); err != nil {
		return err
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(raw) > safefs.MaxRecordBytes {
		return fmt.Errorf("Signal command exceeds control limit")
	}
	cmd := exec.Command("/proc/self/fd/5", "notification-worker", "__signal-command-supervisor")
	cmd.ExtraFiles = []*os.File{lease, cli, helper}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(raw), output, io.Discard
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("cannot start Signal command supervisor")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		if err != nil {
			return fmt.Errorf("managed Signal command failed")
		}
		return nil
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-done
		return ctx.Err()
	}
}
