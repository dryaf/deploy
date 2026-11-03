// Package relay runs the Signal CLI behind a private Unix HTTP socket. The
// account lease is independent of socket names and inherited by CLI children.
package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type Config struct {
	StateDir      string             `json:"state_dir"`
	Socket        string             `json:"socket"`
	AccountFile   string             `json:"account_file"`
	RecipientFile string             `json:"recipient_file"`
	Interactive   *InteractiveConfig `json:"interactive,omitempty"`
}

type InteractiveConfig struct {
	Version      string                    `json:"cli_version"`
	Account      string                    `json:"account"`
	InboxDir     string                    `json:"inbox_dir"`
	Destinations []interaction.Destination `json:"destinations"`
	Actors       []string                  `json:"actors"`
}

func Destination(account, recipient string) string {
	h := sha256.Sum256([]byte("signal\x00" + account + "\x00" + recipient))
	return hex.EncodeToString(h[:])
}

func Run(ctx context.Context, c Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	account, err := provider.ReadSecret(c.AccountFile)
	if err != nil {
		return err
	}
	recipient, err := provider.ReadSecret(c.RecipientFile)
	if err != nil {
		return err
	}
	lease, err := signalaccount.AcquireRuntime(c.StateDir)
	if err != nil {
		return err
	}
	defer lease.Close()
	var inbox *Inbox
	if c.Interactive != nil {
		if err = c.Interactive.Validate(); err != nil {
			return err
		}
		inbox, err = OpenInbox(c.Interactive.InboxDir, c.Interactive.Account, false)
		if err != nil {
			return err
		}
		defer inbox.Close()
	}
	socketDir, err := safefs.Open(filepath.Dir(c.Socket))
	if err != nil {
		return err
	}
	defer socketDir.Close()
	info, err := socketDir.File().Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
		return fmt.Errorf("relay socket directory must be private")
	}
	if info, err = os.Lstat(c.Socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("foreign relay socket path")
		}
		conn, e := net.DialTimeout("unix", c.Socket, 200*time.Millisecond)
		if e == nil {
			conn.Close()
			return fmt.Errorf("relay socket is live")
		}
		if err = os.Remove(c.Socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: c.Socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer l.Close()
	if err = os.Chmod(c.Socket, 0600); err != nil {
		return err
	}
	slots := make(chan struct{}, 1)
	var children sync.WaitGroup
	var childMu sync.Mutex
	closing := false
	runCLI := func(ctx context.Context, output io.Writer, args ...string) error {
		childMu.Lock()
		if closing {
			childMu.Unlock()
			return context.Canceled
		}
		children.Add(1)
		childMu.Unlock()
		defer children.Done()
		cmd := exec.Command("signal-cli", append([]string{"--config", c.StateDir}, args...)...)
		cmd.ExtraFiles = []*os.File{lease.File()}
		cmd.Env = []string{"PATH=/opt/signal-cli/bin:/opt/java/openjdk/bin:/usr/local/bin:/usr/bin:/bin", "HOME=" + c.StateDir, "LANG=C.UTF-8", "JAVA_TOOL_OPTIONS=-Xmx256m"}
		cmd.Stdout = output
		cmd.Stderr = io.Discard
		return process.RunContext(ctx, cmd, 8*time.Second, time.Second)
	}
	runOutput := func(ctx context.Context, output io.Writer, args ...string) error {
		return runCLI(ctx, output, append([]string{"-a", account}, args...)...)
	}
	run := func(ctx context.Context, args ...string) error { return runOutput(ctx, io.Discard, args...) }
	if inbox != nil {
		var version process.Buffer
		version.Limit = 4096
		if err = runCLI(ctx, &version, "--version"); err != nil || strings.TrimSpace(version.String()) != "signal-cli "+c.Interactive.Version {
			return fmt.Errorf("interactive Signal CLI version mismatch")
		}
		var accounts process.Buffer
		accounts.Limit = 32 << 10
		if err = runCLI(ctx, &accounts, "--output=json", "listAccounts"); err != nil {
			return fmt.Errorf("Signal account identity check failed")
		}
		if err = verifySignalAccount([]byte(accounts.String()), account, c.Interactive.Account); err != nil {
			return err
		}
	}
	mux := http.NewServeMux()
	if inbox != nil {
		installInteractiveHandlers(mux, inbox, *c.Interactive, slots, runOutput)
	}
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method rejected", 405)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "busy", 503)
			return
		}
		var req struct {
			ID          string `json:"id"`
			Message     string `json:"message"`
			Destination string `json:"destination"`
		}
		if err := safefs.ReadJSON(http.MaxBytesReader(w, r.Body, 16<<10), &req, 16<<10); err != nil || req.ID == "" || len(req.ID) > 256 || len(req.Message) == 0 || len(req.Message) > 15000 {
			http.Error(w, "invalid request", 400)
			return
		}
		if req.Destination != Destination(account, recipient) {
			http.Error(w, "destination changed", 409)
			return
		}
		if inbox != nil {
			if err := inbox.CheckActive(r.Context()); err != nil {
				http.Error(w, "relay recovery fenced", 409)
				return
			}
		}
		if err := run(r.Context(), "send", "-m", req.Message, "--", recipient); err != nil {
			http.Error(w, "outcome uncertain", 502)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Accepted bool `json:"accepted"`
		}{true})
	})
	server := http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 4096}
	maintenanceDone := make(chan struct{})
	intakeFailure := make(chan error, 1)
	go func() {
		defer close(maintenanceDone)
		interval := time.Minute
		if inbox != nil {
			interval = 5 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if inbox != nil && inbox.CanReceive(ctx) != nil {
					continue
				}
				select {
				case slots <- struct{}{}:
					if inbox == nil {
						_ = run(ctx, "receive", "--timeout", "1", "--max-messages", "32", "--ignore-attachments", "--ignore-stories", "--ignore-avatars", "--ignore-stickers")
					} else {
						output := &receiveWriter{ctx: ctx, inbox: inbox, selector: account, config: *c.Interactive}
						receiveErr := runOutput(ctx, output, "--output=json", "receive", "--timeout", "1", "--max-messages", "16", "--ignore-attachments", "--ignore-stories", "--ignore-avatars", "--ignore-stickers")
						if receiveErr != nil || output.err != nil || len(output.buffer) != 0 || output.discard != nil {
							intakeFailure <- fmt.Errorf("Signal intake stopped with uncertain receive boundary")
							<-slots
							cancel()
							return
						}
					}
					<-slots
				default:
				}
			}
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	err = server.Serve(l)
	// The caller must cancel the service context on shutdown. All active children
	// finish before the account lease's last local reference is closed.
	childMu.Lock()
	closing = true
	childMu.Unlock()
	cancel()
	_ = server.Close()
	<-maintenanceDone
	children.Wait()
	select {
	case failure := <-intakeFailure:
		return failure
	default:
	}
	if err == http.ErrServerClosed {
		return ctx.Err()
	}
	return err
}
