package ledhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/ledartifact"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type Receipt struct {
	Version  int    `json:"version"`
	Spec     Spec   `json:"spec"`
	Hash     string `json:"hash"`
	Complete bool   `json:"complete"`
}
type journal struct {
	Version int    `json:"version"`
	Spec    Spec   `json:"spec"`
	Hash    string `json:"hash"`
	Stage   string `json:"stage"`
}
type services interface {
	Inspect(context.Context, Spec, bool) error
	Reload(context.Context) error
	Start(context.Context, Spec) error
	Verify(context.Context, Spec) error
}

// The alternate filesystem and adapters are package-private test seams. The
// administrator entry point always uses fixed host paths and root ownership.
type manager struct {
	prefix         string
	owner          int
	services       services
	validateHost   func(context.Context, Spec) error
	verifyDaemon   func(context.Context, Spec) error
	prepareRemoval func(context.Context, Spec, string) error
	boundary       func(string) error
}

func (m manager) path(name string) string {
	if m.prefix == "" {
		return name
	}
	return filepath.Join(m.prefix, name)
}
func (m manager) checkpoint(name string) error {
	if m.boundary != nil {
		return m.boundary(name)
	}
	return nil
}

func (m manager) install(ctx context.Context, s Spec, approved string, artifact []byte) (Receipt, error) {
	var zero Receipt
	p, err := Build(s)
	if err != nil {
		return zero, err
	}
	if approved != p.Hash {
		return zero, fmt.Errorf("LED prerequisite approval does not match the reviewed plan")
	}
	if bytesHash(artifact) != s.ArtifactSHA256 {
		return zero, fmt.Errorf("LED artifact differs from approved plan")
	}
	if _, err = ledartifact.Inspect(artifact); err != nil {
		return zero, err
	}
	if err = m.validateHost(ctx, s); err != nil {
		return zero, err
	}
	// Nothing privileged is changed before all artifact and host checks pass.
	if err = m.ensureDir("/var/lib/deploy-led-host", 0755); err != nil {
		return zero, err
	}
	if err = m.ensureDir(Root(s.UID), 0755); err != nil {
		return zero, err
	}
	d, err := safefs.Open(m.path(Root(s.UID)))
	if err != nil {
		return zero, err
	}
	defer d.Close()
	lock, err := d.Lock("operation.lock", true, true)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	for _, name := range []string{"removal.json", "removal-intent.json", "removal-receipt.json"} {
		if _, e := os.Lstat(m.path(Root(s.UID) + "/" + name)); !errors.Is(e, os.ErrNotExist) {
			return zero, fmt.Errorf("LED host removal is retained; explicit replacement is required")
		}
	}
	if _, err = m.read(Root(s.UID)+"/operation.lock", 0); err != nil {
		return zero, err
	}
	var j journal
	raw, err := m.read(Root(s.UID)+"/journal.json", 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		for _, name := range append(sortedFiles(p), Root(s.UID)+"/daemon.pyz", Root(s.UID)+"/receipt.json", Root(s.UID)+"/state", Socket(s.UID)) {
			if _, e := os.Lstat(m.path(name)); !errors.Is(e, os.ErrNotExist) {
				return zero, fmt.Errorf("LED prerequisite destination already exists: %s", name)
			}
		}
		if err = m.services.Inspect(ctx, s, false); err != nil {
			return zero, err
		}
		j = journal{Version: Version, Spec: s, Hash: p.Hash, Stage: "prepared"}
		if err = d.WriteJSON("journal.json", j, false); err != nil {
			return zero, err
		}
	} else if err != nil {
		return zero, err
	} else if safefs.DecodeJSON(raw, &j, 64<<10) != nil || j.Version != Version || j.Spec != s || j.Hash != p.Hash || (j.Stage != "prepared" && j.Stage != "files" && j.Stage != "running" && j.Stage != "complete") {
		return zero, fmt.Errorf("LED prerequisite journal differs; explicit host migration is required")
	}
	if err = m.checkpoint("prepared"); err != nil {
		return zero, err
	}
	if j.Stage == "prepared" {
		if err = m.ensureDir(Root(s.UID)+"/state", 0700); err != nil {
			return zero, err
		}
		for _, name := range sortedFiles(p) {
			if err = m.publish(name, []byte(p.Files[name]), 0644); err != nil {
				return zero, err
			}
			if err = m.checkpoint("published:" + filepath.Base(name)); err != nil {
				return zero, err
			}
		}
		if err = m.publish(Root(s.UID)+"/daemon.pyz", artifact, 0644); err != nil {
			return zero, err
		}
		j.Stage = "files"
		if err = d.WriteJSON("journal.json", j, true); err != nil {
			return zero, err
		}
	}
	if err = m.verifyFiles(p, artifact); err != nil {
		return zero, err
	}
	if err = m.checkpoint("files"); err != nil {
		return zero, err
	}
	if j.Stage == "files" {
		if err = m.services.Inspect(ctx, s, true); err != nil {
			return zero, err
		}
		if err = m.services.Reload(ctx); err != nil {
			return zero, err
		}
		if err = m.services.Inspect(ctx, s, true); err != nil {
			return zero, err
		}
		if err = m.services.Start(ctx, s); err != nil {
			return zero, err
		}
		if err = m.checkpoint("started"); err != nil {
			return zero, err
		}
		j.Stage = "running"
		if err = d.WriteJSON("journal.json", j, true); err != nil {
			return zero, err
		}
	}
	if err = m.services.Inspect(ctx, s, true); err != nil {
		return zero, err
	}
	if err = m.services.Verify(ctx, s); err != nil {
		return zero, err
	}
	if err = m.verifyDaemon(ctx, s); err != nil {
		return zero, err
	}
	receipt := Receipt{Version: Version, Spec: s, Hash: p.Hash, Complete: true}
	if j.Stage == "running" {
		raw, err = json.Marshal(receipt)
		if err != nil {
			return zero, err
		}
		if err = m.publish(Root(s.UID)+"/receipt.json", raw, 0644); err != nil {
			return zero, err
		}
		if err = m.checkpoint("receipt"); err != nil {
			return zero, err
		}
		j.Stage = "complete"
		if err = d.WriteJSON("journal.json", j, true); err != nil {
			return zero, err
		}
	}
	var retained Receipt
	raw, err = m.read(Root(s.UID)+"/receipt.json", 64<<10)
	if err != nil || safefs.DecodeJSON(raw, &retained, 64<<10) != nil || retained != receipt {
		return zero, fmt.Errorf("LED prerequisite completion receipt missing or changed")
	}
	return receipt, m.checkpoint("complete")
}

func sortedFiles(p Plan) []string {
	names := make([]string, 0, len(p.Files))
	for name := range p.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func (m manager) verifyFiles(p Plan, artifact []byte) error {
	for _, name := range append(sortedFiles(p), Root(p.Spec.UID)+"/daemon.pyz") {
		want := []byte(p.Files[name])
		if filepath.Base(name) == "daemon.pyz" {
			want = artifact
		}
		got, err := m.read(name, ledartifact.MaxBytes)
		if err != nil || !bytes.Equal(got, want) {
			return fmt.Errorf("owned LED prerequisite file missing or changed: %s", name)
		}
	}
	return m.checkDir(Root(p.Spec.UID)+"/state", 0700)
}
func (m manager) checkDir(name string, mode os.FileMode) error {
	info, err := os.Lstat(m.path(name))
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(stat.Uid) != m.owner || info.Mode().Perm() != mode {
		return fmt.Errorf("unsafe LED prerequisite directory: %s", name)
	}
	d, err := safefs.Open(m.path(name))
	if err != nil {
		return err
	}
	return d.Close()
}
func (m manager) ensureDir(name string, mode os.FileMode) error {
	if err := m.trustedParent(name); err != nil {
		return err
	}
	parent, err := safefs.Open(filepath.Dir(m.path(name)))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err = unix.Mkdirat(int(parent.File().Fd()), filepath.Base(name), uint32(mode)); err != nil && err != unix.EEXIST {
		return err
	}
	if err == nil {
		created, e := parent.Sub(filepath.Base(name))
		if e != nil {
			return e
		}
		e = created.File().Chmod(mode)
		if e == nil {
			e = created.File().Sync()
		}
		created.Close()
		if e != nil {
			return e
		}
	}
	if err = m.checkDir(name, mode); err != nil {
		return err
	}
	return parent.File().Sync()
}
func (m manager) read(name string, limit int64) ([]byte, error) {
	if err := m.trustedParent(name); err != nil {
		return nil, err
	}
	d, err := safefs.Open(filepath.Dir(m.path(name)))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	f, err := d.Open(filepath.Base(name), unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	mode := os.FileMode(0644)
	if filepath.Base(name) == "journal.json" || filepath.Base(name) == "removal.json" || filepath.Base(name) == "operation.lock" {
		mode = 0600
	}
	if !ok || int(stat.Uid) != m.owner || info.Size() > limit || info.Mode().Perm() != mode {
		return nil, fmt.Errorf("unsafe LED prerequisite file")
	}
	body, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, fmt.Errorf("LED prerequisite file exceeds bounds")
	}
	return body, nil
}
func (m manager) publish(name string, body []byte, mode uint32) error {
	current, err := m.read(name, int64(len(body))+1)
	if err == nil {
		if !bytes.Equal(current, body) {
			return fmt.Errorf("LED prerequisite destination changed: %s", name)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	d, err := safefs.Open(filepath.Dir(m.path(name)))
	if err != nil {
		return err
	}
	defer d.Close()
	id, err := safefs.NewID("n")
	if err != nil {
		return err
	}
	tmp := "." + id
	f, err := d.Open(tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(int(d.File().Fd()), tmp, 0)
	err = f.Chmod(os.FileMode(mode))
	if err == nil {
		_, err = f.Write(body)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return d.Rename(tmp, d, filepath.Base(name))
}

// ReadReceipt verifies the public, root-owned completion authority. Runtime
// discovery must still check the live peer and the installed artifact/config.
func ReadReceipt(uid int) (Receipt, error) {
	var r Receipt
	m := manager{owner: 0}
	if uid <= 0 || uint64(uid) > 1<<31-1 {
		return r, fmt.Errorf("invalid LED target UID")
	}
	if err := m.checkDir(Root(uid), 0755); err != nil {
		return r, err
	}
	for _, name := range []string{"removal-intent.json", "removal-receipt.json"} {
		if _, e := os.Lstat(m.path(Root(uid) + "/" + name)); !errors.Is(e, os.ErrNotExist) {
			return r, fmt.Errorf("LED daemon removal is pending or complete; inspect the retained removal receipt")
		}
	}
	raw, err := m.read(Root(uid)+"/receipt.json", 64<<10)
	if err != nil {
		return r, err
	}
	if safefs.DecodeJSON(raw, &r, 64<<10) != nil || r.Version != Version || !r.Complete || r.Spec.Validate() != nil || r.Spec.UID != uid {
		return r, fmt.Errorf("invalid LED host receipt for UID %s", strconv.Itoa(uid))
	}
	p, _ := Build(r.Spec)
	if r.Hash != p.Hash {
		return r, fmt.Errorf("LED host receipt policy changed")
	}
	artifact, err := m.read(Root(uid)+"/daemon.pyz", ledartifact.MaxBytes)
	if err != nil || bytesHash(artifact) != r.Spec.ArtifactSHA256 {
		return r, fmt.Errorf("LED host artifact changed")
	}
	if _, err = ledartifact.Inspect(artifact); err != nil {
		return r, err
	}
	// A root-only state directory cannot be opened by the worker; verify its
	// metadata separately and inspect the daemon through the authenticated API.
	for _, name := range sortedFiles(p) {
		body, e := m.read(name, 64<<10)
		if e != nil || !bytes.Equal(body, []byte(p.Files[name])) {
			return r, fmt.Errorf("LED host configuration changed")
		}
	}
	return r, nil
}

func bytesHash(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }

func (m manager) trustedParent(name string) error {
	for p := filepath.Dir(m.path(name)); ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || int(st.Uid) != m.owner || info.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("unsafe LED prerequisite ancestor: %s (mode %v)", p, info.Mode())
		}
		if p == "/" || m.prefix != "" && p == m.prefix {
			break
		}
	}
	return nil
}
