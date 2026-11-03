package process

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

func Output(cmd *exec.Cmd, duration time.Duration) ([]byte, error) {
	var out, errs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errs
	err := Run(cmd, duration, 10*time.Second)
	if err != nil {
		return out.Bytes(), fmt.Errorf("%w: %s", err, errs.String())
	}
	return out.Bytes(), nil
}

func GitOutput(args ...string) ([]byte, error) {
	return Output(exec.Command("git", args...), 2*time.Minute)
}

func GitRun(args ...string) error { _, err := GitOutput(args...); return err }

// Terminate the whole local subprocess group, including compiler/rsync children.
// Remote phases own their own watchdog and retain locks across a lost SSH channel.
func Run(cmd *exec.Cmd, duration, grace time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return RunContext(ctx, cmd, duration, grace)
}

func RunContext(ctx context.Context, cmd *exec.Cmd, duration, grace time.Duration) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.WaitDelay = grace
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	var cause error
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		cause = ctx.Err()
	case <-timer.C:
		cause = context.DeadlineExceeded
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	shutdown := time.NewTimer(grace)
	defer shutdown.Stop()
	select {
	case <-done:
		// A parent can exit before a child which ignored TERM.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	case <-shutdown.C:
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	return fmt.Errorf("%s: %w", cmd.Path, cause)
}

// A bounded writer fails the subprocess pipe rather than retaining unbounded data.
type Buffer struct {
	buffer bytes.Buffer
	Limit  int
}

func (b *Buffer) Bytes() []byte { return b.buffer.Bytes() }

func (b *Buffer) String() string { return b.buffer.String() }

func (b *Buffer) Len() int { return b.buffer.Len() }

func (b *Buffer) Write(p []byte) (int, error) {
	if len(p) > b.Limit-b.Len() {
		return 0, fmt.Errorf("output exceeds limit")
	}
	return b.buffer.Write(p)
}
