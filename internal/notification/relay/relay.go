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
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
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

// ManagedExecution is supplied by the managed service after durable admission.
// Command must execute its pinned CLI and inherit the supplied account descriptor.
// It must return only after its complete process group has exited.
type AccountRuntime interface {
	FileFor(string) (*os.File, error)
}

type ManagedExecution struct {
	fresh   bool
	Account AccountRuntime
	Inbox   *Inbox
	Check   func(context.Context) error
	Command func(context.Context, *os.File, io.Writer, ...string) error
	Health  func(context.Context) (any, error)
}

func RunManaged(ctx context.Context, c Config, m ManagedExecution) error {
	if m.Account == nil || m.Inbox == nil || m.Check == nil || m.Command == nil || c.Interactive == nil || m.Inbox.installation == nil || m.Inbox.runtimeAdmission == nil || c.Interactive.InboxDir != m.Inbox.dir.File().Name() || c.Interactive.Account != m.Inbox.account {
		return fmt.Errorf("managed Signal runtime requires admitted account, inbox and pinned command")
	}
	if _, err := m.Account.FileFor(c.StateDir); err != nil {
		return err
	}
	if err := m.Check(ctx); err != nil {
		return err
	}
	return runRelay(ctx, c, &m)
}

// FreshExecution owns a genuinely linked native account and a fresh canonical
// inbox. Direct alert delivery is permitted only on this distinct first-install
// path; recovered services retain their conversation-only contract.
type FreshExecution struct {
	Account *signalaccount.NativeRuntime
	Inbox   *Inbox
	Check   func(context.Context) error
	Command func(context.Context, *os.File, io.Writer, ...string) error
	Health  func(context.Context) (any, error)
}

func RunFreshManaged(ctx context.Context, c Config, f FreshExecution) error {
	if f.Account == nil || f.Inbox == nil || f.Inbox.freshBootstrap == nil || f.Inbox.installation != nil || f.Inbox.runtimeAdmission == nil || f.Check == nil || f.Command == nil || c.RecipientFile == "" {
		return fmt.Errorf("fresh Signal runtime requires its native account, canonical inbox and pinned direct destination")
	}
	if c.Interactive != nil && (c.Interactive.InboxDir != f.Inbox.dir.File().Name() || c.Interactive.Account != f.Inbox.account) {
		return fmt.Errorf("fresh Signal intake differs from canonical inbox")
	}
	if _, err := f.Account.FileFor(c.StateDir); err != nil {
		return err
	}
	if account, err := f.Account.AccountIdentity(); err != nil || account != f.Inbox.account {
		return fmt.Errorf("fresh Signal account and inbox enrollment differ")
	}
	if err := f.Check(ctx); err != nil {
		return err
	}
	m := ManagedExecution{fresh: true, Account: f.Account, Inbox: f.Inbox, Check: f.Check, Command: f.Command, Health: f.Health}
	return runRelay(ctx, c, &m)
}

func Run(ctx context.Context, c Config) error {
	return runRelay(ctx, c, nil)
}

func runRelay(ctx context.Context, c Config, managed *ManagedExecution) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	account, err := provider.ReadSecret(c.AccountFile)
	if err != nil {
		return err
	}
	var recipient string
	if managed == nil || managed.fresh {
		recipient, err = provider.ReadSecret(c.RecipientFile)
		if err != nil {
			return err
		}
	}
	var lease *signalaccount.Lease
	var inbox *Inbox
	if managed == nil {
		lease, err = signalaccount.AcquireRuntime(c.StateDir)
		if err != nil {
			return err
		}
		defer lease.Close()
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
	} else {
		if c.Interactive != nil {
			if err = c.Interactive.Validate(); err != nil {
				return err
			}
		} else if !managed.fresh {
			return fmt.Errorf("recovered managed Signal requires conversation enrollment")
		}
		inbox = managed.Inbox
	}
	check := func(ctx context.Context) error {
		if managed == nil {
			return ctx.Err()
		}
		if err := managed.Check(ctx); err != nil {
			return err
		}
		if _, err := managed.Account.FileFor(c.StateDir); err != nil {
			return err
		}
		return inbox.CheckActive(ctx)
	}
	listener, socketCheck, cleanup, err := openRelaySocket(c.Socket)
	if err != nil {
		return err
	}
	defer cleanup()
	authorityCheck := check
	check = func(ctx context.Context) error {
		if err := authorityCheck(ctx); err != nil {
			return err
		}
		return socketCheck()
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
		if err := check(ctx); err != nil {
			return err
		}
		if managed != nil {
			file, err := managed.Account.FileFor(c.StateDir)
			if err != nil {
				return err
			}
			return managed.Command(ctx, file, output, append([]string{"--config", c.StateDir}, args...)...)
		}
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
		expectedVersion := "0.14.9"
		if c.Interactive != nil {
			expectedVersion = c.Interactive.Version
		}
		var version process.Buffer
		version.Limit = 4096
		if err = runCLI(ctx, &version, "--version"); err != nil || strings.TrimSpace(version.String()) != "signal-cli "+expectedVersion {
			return fmt.Errorf("interactive Signal CLI version mismatch")
		}
		var accounts process.Buffer
		accounts.Limit = 32 << 10
		if err = runCLI(ctx, &accounts, "--output=json", "listAccounts"); err != nil {
			return fmt.Errorf("Signal account identity check failed")
		}
		if err = verifySignalAccount([]byte(accounts.String()), account, inbox.account); err != nil {
			return err
		}
	}
	mux := http.NewServeMux()
	if managed != nil && managed.Health != nil {
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method rejected", http.StatusMethodNotAllowed)
				return
			}
			value, err := managed.Health(r.Context())
			if err != nil {
				http.Error(w, "Signal health unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(value)
		})
	}
	if inbox != nil && c.Interactive != nil {
		installInteractiveHandlers(mux, inbox, *c.Interactive, slots, runOutput)
	}
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if managed != nil && !managed.fresh {
			http.Error(w, "managed Signal requires an enrolled conversation", http.StatusForbidden)
			return
		}
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
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := check(r.Context()); err != nil {
			// A client may disconnect during a valid audit (for example while
			// the gateway restarts). That cancels only its request, not account
			// ownership or the independent receive maintenance service.
			if r.Context().Err() == nil {
				cancel()
			}
			http.Error(w, "relay admission unavailable", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	})
	server := http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 2 * time.Second, MaxHeaderBytes: 4096}
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
				if err := check(ctx); err != nil {
					intakeFailure <- fmt.Errorf("Signal runtime admission withdrawn")
					cancel()
					return
				}
				if inbox != nil && inbox.CanReceive(ctx) != nil {
					continue
				}
				select {
				case slots <- struct{}{}:
					if inbox == nil || c.Interactive == nil {
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
	err = server.Serve(listener)
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
