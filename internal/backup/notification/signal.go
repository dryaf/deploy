package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func RunSignalRelay() error {
	account, err := ReadWorkerSecret("/secrets/signal-account")
	if err != nil {
		return err
	}
	recipient, err := ReadWorkerSecret("/secrets/signal-recipient")
	if err != nil {
		return err
	}
	dir, err := safefs.Open("/relay")
	if err != nil {
		return err
	}
	defer dir.Close()
	lease, err := dir.Lock("relay.lock", true, true)
	if err != nil {
		return err
	}
	defer lease.Close()
	// Share the canonical account lease with the notification-worker relay. A
	// socket-directory lock alone cannot exclude a second relay on another socket.
	accountLease, err := signalaccount.AcquireRuntime("/signal-state")
	if err != nil {
		return err
	}
	defer accountLease.Close()
	socket := filepath.Join("/relay", "signal.sock")
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("relay socket path contains a foreign file")
		}
		if err = os.Remove(socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	semaphore := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		select {
		case semaphore <- struct{}{}:
			defer func() { <-semaphore }()
		default:
			http.Error(w, "relay busy", 503)
			return
		}
		var request struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		if err := safefs.ReadJSON(http.MaxBytesReader(w, r.Body, 16<<10), &request, 16<<10); err != nil || request.ID == "" || len(request.ID) > 256 || len(request.Message) > 8192 {
			http.Error(w, "invalid message", 400)
			return
		}
		cmd := exec.Command("signal-cli", "--config", "/signal-state", "-a", account, "send", "-m", request.Message, "--", recipient)
		cmd.ExtraFiles = []*os.File{accountLease.File()}
		cmd.Env = []string{"PATH=/opt/signal-cli/bin:/opt/java/openjdk/bin:/usr/local/bin:/usr/bin:/bin", "HOME=/signal-state", "LANG=C.UTF-8", "JAVA_TOOL_OPTIONS=-Xmx256m"}
		var out, stderr process.Buffer
		out.Limit = 4096
		stderr.Limit = 4096
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		if err := process.RunContext(r.Context(), cmd, 9*time.Second, time.Second); err != nil {
			http.Error(w, "delivery outcome unknown", 502)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Accepted bool `json:"accepted"`
		}{true})
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 11 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 4096}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Receiving maintains Signal session/group state. Never persist or log incoming
	// message contents, download attachments, or interpret them as commands.
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case semaphore <- struct{}{}:
					cmd := exec.Command("signal-cli", "--config", "/signal-state", "-a", account, "receive", "--timeout", "1", "--max-messages", "32", "--ignore-attachments", "--ignore-stories", "--ignore-avatars", "--ignore-stickers")
					cmd.ExtraFiles = []*os.File{accountLease.File()}
					cmd.Env = []string{"PATH=/opt/signal-cli/bin:/opt/java/openjdk/bin:/usr/local/bin:/usr/bin:/bin", "HOME=/signal-state", "LANG=C.UTF-8", "JAVA_TOOL_OPTIONS=-Xmx256m"}
					cmd.Stdout = io.Discard
					cmd.Stderr = io.Discard
					_ = process.RunContext(ctx, cmd, 8*time.Second, time.Second)
					<-semaphore
				default:
				}
			}
		}
	}()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		case <-done:
		}
	}()
	if err = server.Serve(listener); err == http.ErrServerClosed {
		return nil
	}
	return err
}
