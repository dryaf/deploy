package main

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

func boundedOutput(cmd *exec.Cmd, duration time.Duration) ([]byte, error) {
	var out, errs bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errs
	err := runBounded(cmd, duration, 10*time.Second)
	if err != nil {
		return out.Bytes(), fmt.Errorf("%w: %s", err, errs.String())
	}
	return out.Bytes(), nil
}

func gitOutput(args ...string) ([]byte, error) {
	return boundedOutput(exec.Command("git", args...), 2*time.Minute)
}
func gitRun(args ...string) error { _, err := gitOutput(args...); return err }

// Terminate the whole local subprocess group, including compiler/rsync children.
// Remote phases own their own watchdog and retain locks across a lost SSH channel.
func runBounded(cmd *exec.Cmd, duration, grace time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runBoundedContext(ctx, cmd, duration, grace)
}

func runBoundedContext(ctx context.Context, cmd *exec.Cmd, duration, grace time.Duration) error {
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

func sshBudget(e Environment) time.Duration {
	t := e.Timeouts
	return budget(t.Build, "15m") + budget(t.Pull, "15m") + 6*budget(t.Database, "5m") + 10*budget(t.Service, "60s") + budget(t.Recovery, "5m") + budget(e.Quadlet.HealthTimeout, "60s") + budget(t.ShutdownGrace, "10s")
}
