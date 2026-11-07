//go:build linux

package notification

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestWorkerSecretRejectsPipeWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential-pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadWorkerSecret(path); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("credential FIFO accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("credential read waited for a pipe writer")
	}
}
