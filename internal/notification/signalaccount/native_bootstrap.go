package signalaccount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// NativeAccountReservation owns an initially empty account directory. It is not
// a Signal identity, a link receipt or authority to adopt an existing account.
type NativeAccountReservation struct {
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
}
type nativeAccountBootstrap struct {
	Reservation NativeAccountReservation `json:"reservation"`
	Attempts    []string                 `json:"attempts"`
	Ready       bool                     `json:"ready"`
	Complete    bool                     `json:"complete"`
}

func nativeAccountBootstrapName(path string) string {
	return ".signal-account-" + safefs.LockName(filepath.Base(path)) + ".json"
}
func (r NativeAccountReservation) valid() bool {
	return r.Version == 1 && safefs.ValidID(r.Operation, "n") && filepath.IsAbs(r.Path) && filepath.Clean(r.Path) == r.Path && r.Path != "/" && !strings.ContainsAny(r.Path, "\x00\r\n") && r.Inode != 0
}
func nativeAccountDirectoryPin(d *safefs.Directory) (uint64, uint64, error) {
	info, err := d.File().Stat()
	if err != nil {
		return 0, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
		return 0, 0, fmt.Errorf("native Signal account bootstrap requires private owned directories")
	}
	return uint64(st.Dev), st.Ino, nil
}
func checkNativeAccountReservation(d *safefs.Directory, r NativeAccountReservation) error {
	if !r.valid() {
		return fmt.Errorf("invalid native Signal account reservation")
	}
	dev, ino, err := nativeAccountDirectoryPin(d)
	if err != nil {
		return err
	}
	if dev != r.Device || ino != r.Inode {
		return fmt.Errorf("native Signal account directory replaced")
	}
	var marker NativeAccountReservation
	if err = d.ReadJSON("deploy-native-bootstrap.json", &marker); err != nil {
		return err
	}
	if marker != r {
		return fmt.Errorf("native Signal account reservation changed")
	}
	return nil
}

// BootstrapNativeAccount publishes one fresh account directory. No CLI is run
// and existing directories are never adopted. The private parent must exist.
func BootstrapNativeAccount(ctx context.Context, path, operation string) (NativeAccountReservation, error) {
	return bootstrapNativeAccount(ctx, path, operation, nil)
}
func bootstrapNativeAccount(ctx context.Context, path, operation string, fault func(string) error) (NativeAccountReservation, error) {
	var zero NativeAccountReservation
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || strings.ContainsAny(path, "\x00\r\n") || !safefs.ValidID(operation, "n") {
		return zero, fmt.Errorf("absolute account path and stable operation required")
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	parent, err := safefs.Open(filepath.Dir(path))
	if err != nil {
		return zero, err
	}
	defer parent.Close()
	if _, _, err = nativeAccountDirectoryPin(parent); err != nil {
		return zero, err
	}
	name := nativeAccountBootstrapName(path)
	claimed := false
	if f, e := parent.Open(name+".lock", os.O_RDONLY, 0); e == nil {
		f.Close()
		claimed = true
	} else if !errors.Is(e, os.ErrNotExist) {
		return zero, e
	}
	if !claimed {
		if f, e := parent.Open(name, os.O_RDONLY, 0); e == nil {
			f.Close()
			return zero, fmt.Errorf("native account reservation lock missing")
		} else if !errors.Is(e, os.ErrNotExist) {
			return zero, e
		}
	}
	lock, err := parent.Lock(name+".lock", true, true)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	boundary := func(at string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fault != nil {
			return fault(at)
		}
		return nil
	}
	var j nativeAccountBootstrap
	err = parent.ReadJSON(name, &j)
	if errors.Is(err, os.ErrNotExist) {
		if claimed {
			return zero, fmt.Errorf("native account reservation journal missing")
		}
		if d, e := parent.Sub(filepath.Base(path)); e == nil {
			d.Close()
			return zero, fmt.Errorf("existing Signal account requires explicit adoption")
		} else if !errors.Is(e, os.ErrNotExist) {
			return zero, e
		}
		j.Reservation = NativeAccountReservation{Version: 1, Operation: operation, Path: path}
		if err = parent.WriteJSON(name, j, false); err != nil {
			return zero, err
		}
	} else if err != nil {
		return zero, err
	}
	r := j.Reservation
	if r.Version != 1 || r.Operation != operation || r.Path != path || len(j.Attempts) > 16 || j.Complete && !j.Ready || j.Ready && !r.valid() {
		return zero, fmt.Errorf("native Signal account reservation differs")
	}
	for _, attempt := range j.Attempts {
		if !safefs.ValidID(attempt, "n") {
			return zero, fmt.Errorf("invalid account staging attempt")
		}
	}
	if err = boundary("reserved"); err != nil {
		return r, err
	}
	if d, e := parent.Sub(filepath.Base(path)); e == nil {
		defer d.Close()
		if !j.Ready {
			return r, fmt.Errorf("unowned Signal account appeared during bootstrap")
		}
		if err = checkNativeAccountReservation(d, r); err != nil {
			return r, err
		}
		if !j.Complete {
			j.Complete = true
			if err = parent.WriteJSON(name, j, true); err != nil {
				return r, err
			}
		}
		return r, boundary("complete")
	} else if !errors.Is(e, os.ErrNotExist) {
		return r, e
	}
	if j.Complete {
		return r, fmt.Errorf("completed native account missing; recovery required")
	}
	var stage string
	if j.Ready {
		if len(j.Attempts) == 0 {
			return r, fmt.Errorf("account staging authority missing")
		}
		stage = ".signal-account-stage-" + j.Attempts[len(j.Attempts)-1]
		d, e := parent.Sub(stage)
		if e != nil {
			return r, e
		}
		e = checkNativeAccountReservation(d, r)
		d.Close()
		if e != nil {
			return r, e
		}
	} else {
		if len(j.Attempts) == 16 {
			return r, fmt.Errorf("account staging retry bound reached")
		}
		id, e := safefs.NewID("n")
		if e != nil {
			return r, e
		}
		j.Attempts = append(j.Attempts, id)
		if err = parent.WriteJSON(name, j, true); err != nil {
			return r, err
		}
		stage = ".signal-account-stage-" + id
		if err = unix.Mkdirat(int(parent.File().Fd()), stage, 0700); err != nil {
			return r, err
		}
		d, e := parent.Sub(stage)
		if e != nil {
			return r, e
		}
		r.Device, r.Inode, e = nativeAccountDirectoryPin(d)
		if e == nil {
			e = d.WriteJSON("deploy-native-bootstrap.json", r, false)
		}
		if e == nil {
			e = d.File().Sync()
		}
		d.Close()
		if e != nil {
			return r, e
		}
		if err = boundary("staging"); err != nil {
			return r, err
		}
		j.Reservation = r
		j.Ready = true
		if err = parent.WriteJSON(name, j, true); err != nil {
			return r, err
		}
	}
	if err = boundary("ready"); err != nil {
		return r, err
	}
	if err = parent.Rename(stage, parent, filepath.Base(path)); err != nil {
		return r, err
	}
	if err = boundary("published"); err != nil {
		return r, err
	}
	j.Complete = true
	if err = parent.WriteJSON(name, j, true); err != nil {
		return r, err
	}
	return r, boundary("complete")
}

func InspectNativeAccountReservation(r NativeAccountReservation) error {
	if !r.valid() {
		return fmt.Errorf("invalid native account reservation")
	}
	parent, err := safefs.Open(filepath.Dir(r.Path))
	if err != nil {
		return err
	}
	defer parent.Close()
	if _, _, err = nativeAccountDirectoryPin(parent); err != nil {
		return err
	}
	name := nativeAccountBootstrapName(r.Path)
	lock, err := parent.Lock(name+".lock", false, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	var j nativeAccountBootstrap
	if err = parent.ReadJSON(name, &j); err != nil {
		return err
	}
	if !j.Ready || !j.Complete || j.Reservation != r || len(j.Attempts) == 0 || len(j.Attempts) > 16 {
		return fmt.Errorf("native account publication incomplete or changed")
	}
	d, err := parent.Sub(filepath.Base(r.Path))
	if err != nil {
		return err
	}
	defer d.Close()
	return checkNativeAccountReservation(d, r)
}
