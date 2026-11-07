package ledhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dryaf/deploy/internal/notification/led"
	"github.com/dryaf/deploy/internal/notification/ledartifact"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

type RemovalPlan struct {
	Version      int      `json:"version"`
	Spec         Spec     `json:"spec"`
	Installation string   `json:"installation"`
	Hash         string   `json:"hash"`
	Files        []string `json:"files"`
	Actions      []string `json:"actions"`
}

type RemovalReceipt struct {
	Version      int    `json:"version"`
	Spec         Spec   `json:"spec"`
	Installation string `json:"installation"`
	Hash         string `json:"hash"`
	Complete     bool   `json:"complete"`
}

type removalJournal struct {
	Version      int    `json:"version"`
	Spec         Spec   `json:"spec"`
	Installation string `json:"installation"`
	Hash         string `json:"hash"`
	Stage        string `json:"stage"`
}

type removalServices interface {
	Stop(context.Context, Spec) error
	VerifyStopped(context.Context, Spec) error
}

func BuildRemoval(s Spec) (RemovalPlan, error) {
	p, err := Build(s)
	if err != nil {
		return RemovalPlan{}, err
	}
	r := RemovalPlan{Version: 1, Spec: s, Installation: p.Hash, Files: []string{"/etc/systemd/system/" + Unit(s.UID) + ".socket", "/etc/systemd/system/" + Unit(s.UID) + ".service"}, Actions: []string{
		"require an empty live LED registry and durably fence new states before stopping; retained obligations from any client block removal",
		"stop and disable the exact owned LED socket and daemon; API access ends for every client of this UID's daemon",
		"remove only unchanged owned system unit files and reload systemd",
		"retain the daemon archive, configuration, persistent registry, hardware lock inodes and installation/removal history",
		"gateway output must already be cleared; stopping a daemon does not acknowledge unresolved worker incidents or prove physical hardware restoration",
	}}
	r.Hash = protocol.Hash(struct {
		Version      int
		Spec         Spec
		Installation string
		Files        []string
		RemovalGuard int
	}{r.Version, r.Spec, r.Installation, r.Files, 1})
	return r, nil
}

// Remove is an explicit administrator operation. Ordinary worker control only
// stages its review. No arbitrary caller-supplied destination is ever removed.
func Remove(ctx context.Context, s Spec, approved string) (RemovalReceipt, error) {
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return RemovalReceipt{}, fmt.Errorf("run the reviewed LED host removal as administrator")
	}
	return (manager{owner: 0, services: systemServices{}, prepareRemoval: prepareDaemonRemoval}).remove(ctx, s, approved)
}

func prepareDaemonRemoval(ctx context.Context, s Spec, hash string) error {
	endpoint := led.Endpoint{Version: 1, Socket: Socket(s.UID), OwnerUID: s.UID, PeerUID: 0}
	return (led.Client{Endpoint: &endpoint, Socket: endpoint.Socket, Budget: 3 * time.Second}).PrepareRemoval(ctx, hash)
}

type daemonRemovalFence struct {
	Version  int    `json:"version"`
	PlanHash string `json:"plan_hash"`
}

func (m manager) verifyDaemonRemovalFence(s Spec, hash string) error {
	raw, err := m.read(Root(s.UID)+"/state/removal.json", 4096)
	if err != nil {
		return err
	}
	var fence daemonRemovalFence
	if safefs.DecodeJSON(raw, &fence, 4096) != nil || fence != (daemonRemovalFence{1, hash}) {
		return fmt.Errorf("managed daemon removal fence differs from approved plan")
	}
	return nil
}

func (m manager) remove(ctx context.Context, s Spec, approved string) (RemovalReceipt, error) {
	var zero RemovalReceipt
	p, err := BuildRemoval(s)
	if err != nil {
		return zero, err
	}
	if approved != p.Hash {
		return zero, fmt.Errorf("LED removal approval does not match the reviewed plan")
	}
	service, ok := m.services.(removalServices)
	if !ok {
		return zero, fmt.Errorf("LED host removal adapter unavailable")
	}
	if err = m.checkDir(Root(s.UID), 0755); err != nil {
		return zero, err
	}
	d, err := safefs.Open(m.path(Root(s.UID)))
	if err != nil {
		return zero, err
	}
	defer d.Close()
	if _, err = m.read(Root(s.UID)+"/operation.lock", 0); err != nil {
		return zero, err
	}
	lock, err := d.Lock("operation.lock", true, false)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	var installed journal
	raw, err := m.read(Root(s.UID)+"/journal.json", 64<<10)
	if err != nil || safefs.DecodeJSON(raw, &installed, 64<<10) != nil || installed.Version != 1 || installed.Spec != s || installed.Hash != p.Installation || installed.Stage != "complete" {
		return zero, fmt.Errorf("LED removal requires the completed installation journal")
	}
	var receipt Receipt
	raw, err = m.read(Root(s.UID)+"/receipt.json", 64<<10)
	if err != nil || safefs.DecodeJSON(raw, &receipt, 64<<10) != nil || receipt != (Receipt{Version: 1, Spec: s, Hash: p.Installation, Complete: true}) {
		return zero, fmt.Errorf("LED installation receipt missing or changed")
	}
	artifact, err := m.read(Root(s.UID)+"/daemon.pyz", ledartifact.MaxBytes)
	if err != nil || bytesHash(artifact) != s.ArtifactSHA256 {
		return zero, fmt.Errorf("retained LED artifact missing or changed")
	}
	if _, err = ledartifact.Inspect(artifact); err != nil {
		return zero, err
	}
	installPlan, _ := Build(s)
	config, err := m.read(Root(s.UID)+"/config.toml", 64<<10)
	if err != nil || !bytes.Equal(config, []byte(installPlan.Files[Root(s.UID)+"/config.toml"])) {
		return zero, fmt.Errorf("retained LED configuration changed")
	}
	if err = m.checkDir(Root(s.UID)+"/state", 0700); err != nil {
		return zero, err
	}
	var j removalJournal
	raw, err = m.read(Root(s.UID)+"/removal.json", 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		if _, e := os.Lstat(m.path(Root(s.UID) + "/removal-intent.json")); !errors.Is(e, os.ErrNotExist) {
			return zero, fmt.Errorf("LED removal journal missing from claimed operation")
		}
		if _, e := os.Lstat(m.path(Root(s.UID) + "/removal-receipt.json")); !errors.Is(e, os.ErrNotExist) {
			return zero, fmt.Errorf("LED removal history exists without its journal")
		}
		if err = m.verifyFiles(installPlan, artifact); err != nil {
			return zero, err
		}
		if err = m.services.Inspect(ctx, s, true); err != nil {
			return zero, err
		}
		j = removalJournal{Version: 1, Spec: s, Installation: p.Installation, Hash: p.Hash, Stage: "prepared"}
		if err = d.WriteJSON("removal.json", j, false); err != nil {
			return zero, err
		}
	} else if err != nil || safefs.DecodeJSON(raw, &j, 64<<10) != nil || j.Version != 1 || j.Spec != s || j.Installation != p.Installation || j.Hash != p.Hash || (j.Stage != "prepared" && j.Stage != "stopped" && j.Stage != "units" && j.Stage != "complete") {
		return zero, fmt.Errorf("LED removal journal changed")
	}
	public := RemovalReceipt{Version: 1, Spec: s, Installation: p.Installation, Hash: p.Hash}
	if j.Stage == "prepared" {
		if err = m.publishRemovalRecord("removal-intent.json", public); err != nil {
			return zero, err
		}
	} else if err = m.verifyRemovalRecord("removal-intent.json", public); err != nil {
		return zero, err
	}
	if err = m.checkpoint("remove-prepared"); err != nil {
		return zero, err
	}
	save := func(stage string) error { j.Stage = stage; return d.WriteJSON("removal.json", j, true) }
	if j.Stage == "prepared" {
		// Recheck bytes on retries before touching a system service.
		if err = m.verifyFiles(installPlan, artifact); err != nil {
			return zero, err
		}
		if err = m.services.Inspect(ctx, s, true); err != nil {
			return zero, err
		}
		if err = m.verifyDaemonRemovalFence(s, p.Hash); errors.Is(err, os.ErrNotExist) {
			if m.prepareRemoval == nil {
				return zero, fmt.Errorf("managed daemon removal fence adapter unavailable")
			}
			if err = m.prepareRemoval(ctx, s, p.Hash); err != nil {
				return zero, err
			}
		} else if err != nil {
			return zero, err
		}
		if err = m.verifyDaemonRemovalFence(s, p.Hash); err != nil {
			return zero, err
		}
		// A lost daemon response may follow publication but precede directory
		// sync. Confirm durability locally before stopping its process.
		state, e := safefs.Open(m.path(Root(s.UID) + "/state"))
		if e != nil {
			return zero, e
		}
		e = state.File().Sync()
		state.Close()
		if e != nil {
			return zero, e
		}
		if err = m.checkpoint("remove-fenced"); err != nil {
			return zero, err
		}
		if err = service.Stop(ctx, s); err != nil {
			return zero, err
		}
		if err = m.checkpoint("remove-stop-returned"); err != nil {
			return zero, err
		}
		if err = service.VerifyStopped(ctx, s); err != nil {
			return zero, err
		}
		if err = save("stopped"); err != nil {
			return zero, err
		}
	}
	if err = m.verifyDaemonRemovalFence(s, p.Hash); err != nil {
		return zero, err
	}
	if err = service.VerifyStopped(ctx, s); err != nil {
		return zero, err
	}
	if _, e := os.Lstat(m.path(Socket(s.UID))); !errors.Is(e, os.ErrNotExist) {
		return zero, fmt.Errorf("LED socket remains after stop; inspect its owner before removal")
	}
	if j.Stage == "stopped" {
		for _, name := range p.Files {
			body, e := m.read(name, 64<<10)
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil || !bytes.Equal(body, []byte(installPlan.Files[name])) {
				return zero, fmt.Errorf("LED unit changed during removal: %s", name)
			}
			parent, e := safefs.Open(filepath.Dir(m.path(name)))
			if e != nil {
				return zero, e
			}
			e = unix.Unlinkat(int(parent.File().Fd()), filepath.Base(name), 0)
			if e == nil {
				e = parent.File().Sync()
			}
			parent.Close()
			if e != nil {
				return zero, e
			}
			if err = m.checkpoint("removed:" + filepath.Base(name)); err != nil {
				return zero, err
			}
		}
		if err = m.services.Reload(ctx); err != nil {
			return zero, err
		}
		if err = save("units"); err != nil {
			return zero, err
		}
	}
	for _, name := range p.Files {
		if _, e := os.Lstat(m.path(name)); !errors.Is(e, os.ErrNotExist) {
			return zero, fmt.Errorf("removed LED service file reappeared")
		}
	}
	if err = m.services.Inspect(ctx, s, false); err != nil {
		return zero, err
	}
	public.Complete = true
	if j.Stage == "units" {
		if err = m.publishRemovalRecord("removal-receipt.json", public); err != nil {
			return zero, err
		}
		if err = m.checkpoint("remove-receipt"); err != nil {
			return zero, err
		}
		if err = save("complete"); err != nil {
			return zero, err
		}
	}
	if err = m.verifyRemovalRecord("removal-receipt.json", public); err != nil {
		return zero, err
	}
	return public, m.checkpoint("remove-complete")
}

func (m manager) publishRemovalRecord(name string, r RemovalReceipt) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return m.publish(Root(r.Spec.UID)+"/"+name, raw, 0644)
}
func (m manager) verifyRemovalRecord(name string, r RemovalReceipt) error {
	raw, err := m.read(Root(r.Spec.UID)+"/"+name, 64<<10)
	var got RemovalReceipt
	if err != nil || safefs.DecodeJSON(raw, &got, 64<<10) != nil || got != r {
		return fmt.Errorf("LED removal evidence missing or changed")
	}
	return nil
}

// ReadRemovalReceipt is rootless and read-only. It verifies the retained public
// result and that owned service files/socket remain absent; it never restarts.
func ReadRemovalReceipt(uid int) (RemovalReceipt, error) {
	return (manager{owner: 0}).readRemovalReceipt(uid)
}
func (m manager) readRemovalReceipt(uid int) (RemovalReceipt, error) {
	var r RemovalReceipt
	if uid <= 0 || uint64(uid) > 1<<31-1 {
		return r, fmt.Errorf("invalid LED client UID")
	}
	if err := m.checkDir(Root(uid), 0755); err != nil {
		return r, err
	}
	raw, err := m.read(Root(uid)+"/removal-receipt.json", 64<<10)
	if err != nil {
		return r, err
	}
	if safefs.DecodeJSON(raw, &r, 64<<10) != nil || r.Version != 1 || !r.Complete || r.Spec.UID != uid {
		return r, fmt.Errorf("invalid LED removal receipt")
	}
	p, err := BuildRemoval(r.Spec)
	if err != nil || r.Hash != p.Hash || r.Installation != p.Installation {
		return r, fmt.Errorf("LED removal policy changed")
	}
	intent := r
	intent.Complete = false
	if err = m.verifyRemovalRecord("removal-intent.json", intent); err != nil {
		return r, err
	}
	var installed Receipt
	raw, err = m.read(Root(uid)+"/receipt.json", 64<<10)
	if err != nil || safefs.DecodeJSON(raw, &installed, 64<<10) != nil || installed != (Receipt{Version: 1, Spec: r.Spec, Hash: r.Installation, Complete: true}) {
		return r, fmt.Errorf("removed LED installation history changed")
	}
	for _, name := range append(p.Files, Socket(uid)) {
		if _, e := os.Lstat(m.path(name)); !errors.Is(e, os.ErrNotExist) {
			return r, fmt.Errorf("removed LED service files or socket reappeared")
		}
	}
	return r, nil
}
