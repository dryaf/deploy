package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/notification/provider"
	"github.com/dryaf/deploy/internal/notification/relay"
	"github.com/dryaf/deploy/internal/notification/signalaccount"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// SignalVerificationHealth is deliberately distinct from relay runtime health.
// No successful response from this endpoint grants provider or intake authority.
type SignalVerificationHealth struct {
	ServiceHash       string                 `json:"service_hash,omitempty"`
	Version           int                    `json:"version"`
	VerificationOnly  bool                   `json:"verification_only"`
	IntakeEnabled     bool                   `json:"intake_enabled"`
	ManifestHash      string                 `json:"manifest_hash"`
	InstallationHash  string                 `json:"installation_hash"`
	RecoveryOperation string                 `json:"recovery_operation"`
	CommitHash        string                 `json:"commit_hash"`
	Signal            provider.SignalAccount `json:"signal"`
}

type signalEndpointVerifier struct {
	root     *signalVerificationRoot
	account  *signalaccount.RecoveryVerification
	inbox    *relay.SignalRuntimeVerification
	manifest SignalVerificationManifest
	service  signalServiceBinding
}

func openSignalEndpointVerifier(ctx context.Context, root, pin string) (*signalEndpointVerifier, error) {
	return openSignalEndpointVerifierService(ctx, root, pin, signalServiceBinding{})
}
func openSignalEndpointVerifierService(ctx context.Context, root, pin string, service signalServiceBinding) (_ *signalEndpointVerifier, err error) {
	if !safefs.DigestPattern.MatchString(pin) {
		return nil, fmt.Errorf("verification manifest digest required")
	}
	v := &signalEndpointVerifier{service: service}
	defer func() {
		if err != nil {
			v.close()
		}
	}()
	if v.root, err = openSignalVerificationRoot(root, false); err != nil {
		return nil, err
	}
	if v.manifest, err = readSignalVerificationManifest(v.root.dir, signalVerificationComplete); err != nil {
		return nil, err
	}
	m := v.manifest
	if m.Spec.Root != root || m.ManifestHash != pin {
		return nil, fmt.Errorf("verification endpoint pin changed")
	}
	if err = v.root.audit(m, true); err != nil {
		return nil, err
	}
	if v.account, err = signalaccount.OpenRecoveryVerification(m.Spec.AccountState, m.Spec.Recovery); err != nil {
		return nil, err
	}
	if v.inbox, err = relay.OpenSignalRuntimeVerification(ctx, m.Spec.InboxDestination, m.Installation.InstallationHash); err != nil {
		return nil, err
	}
	if protocol.Hash(v.inbox.Installation()) != protocol.Hash(m.Installation) {
		return nil, fmt.Errorf("verification installation changed")
	}
	if err = v.check(ctx); err != nil {
		return nil, err
	}
	return v, nil
}
func (v *signalEndpointVerifier) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkSignalServiceBinding(v.root.dir, v.service); err != nil {
		return err
	}
	if err := v.root.audit(v.manifest, true); err != nil {
		return err
	}
	if err := v.account.Check(); err != nil {
		return err
	}
	if err := v.inbox.Check(ctx); err != nil {
		return err
	}
	receipt, ok, err := v.inbox.VerificationEndpoint()
	if err != nil {
		return err
	}
	if !ok || receipt != v.manifest.receipt() {
		return fmt.Errorf("verification endpoint receipt changed")
	}
	return v.root.audit(v.manifest, true)
}
func (v *signalEndpointVerifier) close() {
	if v.inbox != nil {
		v.inbox.Close()
		v.inbox = nil
	}
	if v.account != nil {
		v.account.Close()
		v.account = nil
	}
	if v.root != nil {
		v.root.close()
		v.root = nil
	}
}

// ServeSignalVerification holds the enrolled account and both offline inboxes.
// Its only API is GET /health. It has no provider credentials, CLI or transport.
func ServeSignalVerification(ctx context.Context, root, pin string) error {
	return serveSignalVerification(ctx, root, pin, time.Second)
}
func serveSignalVerification(ctx context.Context, root, pin string, interval time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	v, err := openSignalEndpointVerifier(ctx, root, pin)
	if err != nil {
		return err
	}
	defer v.close()
	return serveSignalVerifier(ctx, v, interval, nil)
}
func serveSignalVerifier(ctx context.Context, v *signalEndpointVerifier, interval time.Duration, serviceCheck func() error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	l, socketCheck, cleanup, err := signalVerificationSocket(v.root)
	if err != nil {
		return err
	}
	defer cleanup()
	// One audit at a time; requests beyond this bound receive 503 immediately.
	gate := make(chan struct{}, 1)
	audit := func(ctx context.Context) error {
		if serviceCheck != nil {
			if err := serviceCheck(); err != nil {
				return err
			}
		}
		if err := socketCheck(); err != nil {
			return err
		}
		if err := v.check(ctx); err != nil {
			return err
		}
		return socketCheck()
	}
	fatal := make(chan error, 1)
	fail := func(err error) {
		select {
		case fatal <- err:
		default:
		}
		cancel()
	}
	next, err := v.manifest.gateway()
	if err != nil {
		return err
	}
	c, err := decodeRuntime(next)
	if err != nil {
		return err
	}
	health := SignalVerificationHealth{Version: 1, VerificationOnly: true, ServiceHash: v.service.ManifestHash, ManifestHash: v.manifest.ManifestHash, InstallationHash: v.manifest.Installation.InstallationHash, RecoveryOperation: v.manifest.Spec.Recovery.Operation, CommitHash: v.manifest.Spec.CommitHash, Signal: *c.Signal}
	var handlers sync.WaitGroup
	var handlerMu sync.Mutex
	closing := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerMu.Lock()
		if closing {
			handlerMu.Unlock()
			http.Error(w, "verification stopped", http.StatusServiceUnavailable)
			return
		}
		handlers.Add(1)
		handlerMu.Unlock()
		defer handlers.Done()
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet || r.URL.Path != "/health" || r.URL.RawQuery != "" || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			http.Error(w, "verification endpoint only", http.StatusForbidden)
			return
		}
		select {
		case gate <- struct{}{}:
			defer func() { <-gate }()
		default:
			http.Error(w, "verification busy", http.StatusServiceUnavailable)
			return
		}
		checkCtx, done := context.WithTimeout(ctx, 10*time.Second)
		defer done()
		if err := audit(checkCtx); err != nil {
			http.Error(w, "verification unavailable", http.StatusServiceUnavailable)
			fail(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(health)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 12 * time.Second, IdleTimeout: time.Second, MaxHeaderBytes: 4096, ErrorLog: log.New(io.Discard, "", 0), BaseContext: func(net.Listener) context.Context { return ctx }}
	server.SetKeepAlivesEnabled(false)
	served := make(chan error, 1)
	go func() {
		served <- server.Serve(&signalVerificationListener{UnixListener: l, slots: make(chan struct{}, 16)})
	}()
	defer func() {
		cancel()
		handlerMu.Lock()
		closing = true
		handlerMu.Unlock()
		shutdownCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		_ = server.Shutdown(shutdownCtx)
		_ = server.Close()
		handlers.Wait()
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case err := <-served:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-ctx.Done():
			select {
			case err := <-fatal:
				return err
			default:
				return ctx.Err()
			}
		case <-ticker.C:
			select {
			case gate <- struct{}{}:
				checkCtx, done := context.WithTimeout(ctx, 10*time.Second)
				err := audit(checkCtx)
				done()
				<-gate
				if err != nil {
					return err
				}
			default:
			}
		}
	}
}

// Binding through the retained directory descriptor prevents a path replacement
// from redirecting socket creation/cleanup. Cleanup only removes our own inode.
func signalVerificationSocket(r *signalVerificationRoot) (*net.UnixListener, func() error, func(), error) {
	path := fmt.Sprintf("/proc/self/fd/%d/api.sock", r.dir.File().Fd())
	info, err := os.Lstat(path)
	if err == nil {
		st := info.Sys().(*syscall.Stat_t)
		if info.Mode()&os.ModeSocket == 0 || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0600 {
			return nil, nil, nil, fmt.Errorf("foreign verification socket")
		}
		c, e := net.DialTimeout("unix", path, 200*time.Millisecond)
		if e == nil {
			c.Close()
			return nil, nil, nil, fmt.Errorf("verification socket is live")
		}
		if !errors.Is(e, syscall.ECONNREFUSED) {
			return nil, nil, nil, fmt.Errorf("verification socket liveness uncertain")
		}
		current, e := os.Lstat(path)
		if e != nil || !os.SameFile(info, current) {
			return nil, nil, nil, fmt.Errorf("verification socket replaced")
		}
		if err = unix.Unlinkat(int(r.dir.File().Fd()), "api.sock", 0); err != nil {
			return nil, nil, nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, nil, nil, err
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, nil, err
	}
	l.SetUnlinkOnClose(false)
	info, err = os.Lstat(path)
	if err != nil {
		l.Close()
		return nil, nil, nil, err
	}
	cleanup := func() {
		l.Close()
		current, e := os.Lstat(path)
		if e == nil && os.SameFile(info, current) {
			_ = unix.Unlinkat(int(r.dir.File().Fd()), "api.sock", 0)
		}
	}
	if err = unix.Fchmodat(int(r.dir.File().Fd()), "api.sock", 0600, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	check := func() error {
		current, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !os.SameFile(info, current) || current.Mode().Perm() != 0600 {
			return fmt.Errorf("verification socket replaced or unsafe")
		}
		return nil
	}
	return l, check, cleanup, nil
}

type signalVerificationListener struct {
	*net.UnixListener
	slots chan struct{}
}

func (l *signalVerificationListener) Accept() (net.Conn, error) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		raw, err := c.SyscallConn()
		var cred *unix.Ucred
		var peerErr error
		if err == nil {
			err = raw.Control(func(fd uintptr) { cred, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
		}
		if err != nil || peerErr != nil || cred == nil || cred.Uid != uint32(os.Getuid()) {
			c.Close()
			continue
		}
		select {
		case l.slots <- struct{}{}:
			return &signalVerificationConn{Conn: c, release: func() { <-l.slots }}, nil
		default:
			c.Close()
		}
	}
}

type signalVerificationConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *signalVerificationConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
