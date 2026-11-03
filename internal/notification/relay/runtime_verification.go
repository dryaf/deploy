package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// SignalRuntimeVerification retains both offline leases. It exposes no receive,
// send, list or acknowledgement method and never opens a writable inbox runtime.
type SignalRuntimeVerification struct {
	mu       sync.Mutex
	src, dst *safefs.Directory
	sl, dl   *safefs.Lock
	stage    *safefs.Directory
	p        SignalRuntimeInstallation
	files    []runtimeVerificationFile
}
type runtimeVerificationFile struct {
	dir  *safefs.Directory
	name string
	info os.FileInfo
}

func OpenSignalRuntimeVerification(ctx context.Context, path, pin string) (_ *SignalRuntimeVerification, err error) {
	if !safefs.DigestPattern.MatchString(pin) {
		return nil, fmt.Errorf("exact Signal installation digest required")
	}
	v := &SignalRuntimeVerification{}
	defer func() {
		if err != nil {
			v.Close()
		}
	}()
	if v.dst, err = privateInboxDirectory(path); err != nil {
		return nil, err
	}
	if err = v.dst.ReadJSON(runtimeInstallComplete, &v.p); err != nil {
		return nil, err
	}
	if err = v.p.validate(); err != nil {
		return nil, err
	}
	if v.p.Destination != path || v.p.InstallationHash != pin {
		return nil, fmt.Errorf("Signal installation reference changed")
	}
	if v.src, err = privateInboxDirectory(v.p.Source); err != nil {
		return nil, err
	}
	if v.sl, err = v.src.Lock("inbox.lock", true, false); err != nil {
		return nil, err
	}
	if v.dl, err = v.dst.Lock("inbox.lock", true, false); err != nil {
		return nil, err
	}
	items := []struct {
		d    *safefs.Directory
		name string
	}{{v.src, "inbox.db"}, {v.dst, "inbox.db"}, {v.dst, runtimeInstallImage}}
	if v.p.Cursor.Handoff.Summary.Manifest.Restore.Operation != "" {
		stage, _, e := runtimeSourceDirectory(v.src, v.p.Cursor)
		if e != nil {
			return nil, e
		}
		v.stage = stage
		for _, name := range []string{"source.db", "candidate.db", signalCursorDatabase} {
			items = append(items, struct {
				d    *safefs.Directory
				name string
			}{stage, name})
		}
	} else {
		items = append(items, struct {
			d    *safefs.Directory
			name string
		}{v.src, signalCursorDatabase})
	}
	for _, item := range items {
		f, e := inboxArtifactFile(item.d, item.name, 34<<20)
		if e != nil {
			return nil, e
		}
		i, e := f.Stat()
		f.Close()
		if e != nil {
			return nil, e
		}
		v.files = append(v.files, runtimeVerificationFile{item.d, item.name, i})
	}

	if err = v.Check(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

// Installation returns a detached proof so callers cannot mutate future checks.
func (v *SignalRuntimeVerification) Installation() SignalRuntimeInstallation {
	v.mu.Lock()
	defer v.mu.Unlock()
	var p SignalRuntimeInstallation
	// A validated record is bounded and JSON encodable.
	raw, _ := json.Marshal(v.p)
	_ = protocol.Decode(raw, &p, safefs.MaxRecordBytes)
	return p
}

func (v *SignalRuntimeVerification) Check(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.check(ctx)
}
func (v *SignalRuntimeVerification) check(ctx context.Context) error {
	if v.dst == nil {
		return fmt.Errorf("Signal runtime verifier closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	checkPaths := func() error {
		if v.stage != nil {
			current, closeDir, err := runtimeSourceDirectory(v.src, v.p.Cursor)
			if err != nil {
				return err
			}
			old, err := v.stage.File().Stat()
			if err != nil {
				closeDir()
				return err
			}
			now, err := current.File().Stat()
			closeDir()
			if err != nil {
				return err
			}
			if !os.SameFile(old, now) {
				return fmt.Errorf("Signal verification source stage replaced")
			}
		}
		for _, item := range []struct {
			path string
			d    *safefs.Directory
			l    *safefs.Lock
		}{{v.p.Source, v.src, v.sl}, {v.p.Destination, v.dst, v.dl}} {
			if err := checkRuntimeDirectory(item.path, item.d); err != nil {
				return err
			}
			if err := checkRuntimeLease(item.d, item.l); err != nil {
				return err
			}
		}
		for _, item := range v.files {
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if err := absentRuntimeFile(item.dir, item.name+suffix); err != nil {
					return err
				}
			}
			f, err := inboxArtifactFile(item.dir, item.name, 34<<20)
			if err != nil {
				return err
			}
			i, err := f.Stat()
			f.Close()
			if err != nil {
				return err
			}
			if !os.SameFile(i, item.info) {
				return fmt.Errorf("Signal verification database replaced")
			}
		}
		return nil
	}
	if err := checkPaths(); err != nil {
		return err
	}
	if err := verifyRuntimeSource(ctx, v.src, v.p.Cursor); err != nil {
		return err
	}
	if err := verifyRuntimeInstallation(ctx, v.src, v.dst, v.p); err != nil {
		return err
	}
	return checkPaths()
}

// SignalVerificationEndpoint independently binds this installation to one
// endpoint publication. Losing the publication cannot authorize a second root.
type SignalVerificationEndpoint struct {
	Version      int    `json:"version"`
	ManifestHash string `json:"manifest_hash"`
	Root         string `json:"root"`
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
}

const verificationEndpointFile = "signal-verification-endpoint.json"

func (v *SignalRuntimeVerification) VerificationEndpoint() (SignalVerificationEndpoint, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var p SignalVerificationEndpoint
	if v.dst == nil {
		return p, false, fmt.Errorf("Signal runtime verifier closed")
	}
	err := readRuntimeVerificationEndpoint(v.dst, &p)
	if os.IsNotExist(err) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (v *SignalRuntimeVerification) BindVerificationEndpoint(ctx context.Context, p SignalVerificationEndpoint) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if p.Version != 1 || !safefs.DigestPattern.MatchString(p.ManifestHash) || p.Inode == 0 || !runtimeInstallationPaths(p.Root, v.p.Destination) || !runtimeInstallationPaths(p.Root, v.p.Source) {
		return fmt.Errorf("invalid Signal verification endpoint binding")
	}
	if err := v.check(ctx); err != nil {
		return err
	}
	var old SignalVerificationEndpoint
	err := readRuntimeVerificationEndpoint(v.dst, &old)
	if err == nil {
		if old != p {
			return fmt.Errorf("Signal installation already bound to another verification endpoint")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return v.dst.WriteJSON(verificationEndpointFile, p, false)
}

func (v *SignalRuntimeVerification) Close() {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.dl != nil {
		v.dl.Close()
		v.dl = nil
	}
	if v.sl != nil {
		v.sl.Close()
		v.sl = nil
	}
	if v.stage != nil {
		v.stage.Close()
		v.stage = nil
	}
	if v.dst != nil {
		v.dst.Close()
		v.dst = nil
	}
	if v.src != nil {
		v.src.Close()
		v.src = nil
	}
}

func readRuntimeVerificationEndpoint(d *safefs.Directory, p *SignalVerificationEndpoint) error {
	f, err := d.Open(verificationEndpointFile, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	i, err := f.Stat()
	if err != nil {
		return err
	}
	st := i.Sys().(*syscall.Stat_t)
	if !i.Mode().IsRegular() || i.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) || st.Nlink != 1 || i.Size() > 4096 {
		return fmt.Errorf("unsafe Signal verification endpoint receipt")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return err
	}
	return safefs.DecodeJSON(raw, p, 4096)
}
