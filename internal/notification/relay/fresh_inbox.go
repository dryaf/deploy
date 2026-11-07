package relay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// FreshInboxBootstrap reserves a canonical empty inbox without recovery history.
// Account is the public runtime account fingerprint, never its private selector.
type FreshInboxBootstrap struct {
	Version   int               `json:"version"`
	Operation string            `json:"operation"`
	State     string            `json:"state"`
	Identity  protocol.Identity `json:"identity"`
	Account   string            `json:"account"`
}

type freshInboxJournal struct {
	Reservation    FreshInboxBootstrap `json:"reservation"`
	Attempts       []string            `json:"attempts,omitempty"`
	Ready          bool                `json:"ready"`
	Complete       bool                `json:"complete"`
	Device         uint64              `json:"device,omitempty"`
	Inode          uint64              `json:"inode,omitempty"`
	DatabaseDevice uint64              `json:"database_device,omitempty"`
	DatabaseInode  uint64              `json:"database_inode,omitempty"`
}

func (b FreshInboxBootstrap) validate() error {
	if b.Version != 1 || !safefs.ValidID(b.Operation, "n") || !b.Identity.Valid() || !filepath.IsAbs(b.State) || filepath.Clean(b.State) != b.State || b.State == "/" {
		return fmt.Errorf("invalid fresh Signal inbox reservation")
	}
	if !protocol.ValidToken(b.Account) {
		return fmt.Errorf("invalid Signal inbox account")
	}
	return nil
}

// BootstrapFreshInbox atomically publishes an initialized fresh inbox state. Retries
// retain the reserved identity; an existing directory is never initialized or
// adopted without this operation's canonical reservation. The parent must exist
// and be owned and private. No provider or service calls are made here.
func BootstrapFreshInbox(ctx context.Context, state, operation string, account string) (FreshInboxBootstrap, error) {
	return bootstrapFreshInbox(ctx, state, operation, account, nil)
}

func bootstrapFreshInbox(ctx context.Context, state, operation string, account string, fault func(string) error) (FreshInboxBootstrap, error) {
	var zero FreshInboxBootstrap
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if !filepath.IsAbs(state) || filepath.Clean(state) != state || state == "/" || !safefs.ValidID(operation, "n") {
		return zero, fmt.Errorf("absolute state and stable bootstrap operation required")
	}
	if !protocol.ValidToken(account) {
		return zero, fmt.Errorf("Signal inbox account required")
	}
	parent, err := safefs.Open(filepath.Dir(state))
	if err != nil {
		return zero, err
	}
	defer parent.Close()
	if err = privateFreshInboxDirectory(parent); err != nil {
		return zero, err
	}
	name := filepath.Base(state)
	journalName := ".signal-inbox-" + safefs.LockName(name) + ".json"
	claimed := false
	if f, e := parent.Open(journalName+".lock", os.O_RDONLY, 0); e == nil {
		claimed = true
		f.Close()
	} else if !errors.Is(e, os.ErrNotExist) {
		return zero, e
	}
	if !claimed {
		if f, e := parent.Open(journalName, os.O_RDONLY, 0); e == nil {
			f.Close()
			return zero, fmt.Errorf("bootstrap lock missing; recovery required")
		} else if !errors.Is(e, os.ErrNotExist) {
			return zero, e
		}
	}
	lock, err := parent.Lock(journalName+".lock", true, true)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	boundary := func(stage string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if fault != nil {
			return fault(stage)
		}
		return nil
	}
	var j freshInboxJournal
	err = parent.ReadJSON(journalName, &j)
	if errors.Is(err, os.ErrNotExist) {
		if claimed {
			return zero, fmt.Errorf("bootstrap reservation missing; recovery required")
		}
		if existing, e := parent.Sub(name); e == nil {
			existing.Close()
			return zero, fmt.Errorf("state exists without bootstrap authority; recovery required")
		} else if !errors.Is(e, os.ErrNotExist) {
			return zero, e
		}
		id, e := safefs.NewID("inbox")
		if e != nil {
			return zero, e
		}
		recovery, e := safefs.NewID("epoch")
		if e != nil {
			return zero, e
		}
		j.Reservation = FreshInboxBootstrap{1, operation, state, protocol.Identity{Worker: id, Recovery: recovery}, account}
		if err = parent.WriteJSON(journalName, j, false); err != nil {
			return zero, err
		}
	} else if err != nil {
		return zero, err
	}
	b := j.Reservation
	if b.validate() != nil || b.Operation != operation || b.State != state || b.Account != account || len(j.Attempts) > 16 || (j.Complete && !j.Ready) {
		return zero, fmt.Errorf("bootstrap policy or operation differs from reserved inbox")
	}
	for _, attempt := range j.Attempts {
		if !safefs.ValidID(attempt, "n") {
			return zero, fmt.Errorf("invalid bootstrap attempt")
		}
	}
	if err = boundary("reserved"); err != nil {
		return b, err
	}
	if existing, e := parent.Sub(name); e == nil {
		if !j.Ready {
			existing.Close()
			return b, fmt.Errorf("unowned state appeared during bootstrap")
		}
		e = verifyFreshInboxFiles(existing, j)
		existing.Close()
		if e != nil {
			return b, e
		}
		s, e := openFreshInboxImage(state, b)
		if e != nil {
			return b, e
		}
		if e = s.Close(); e != nil {
			return b, e
		}
		j.Complete = true
		if err = parent.WriteJSON(journalName, j, true); err != nil {
			return b, err
		}
		return b, boundary("complete")
	} else if !errors.Is(e, os.ErrNotExist) {
		return b, e
	}
	if j.Complete {
		return b, fmt.Errorf("completed inbox state missing; recovery required")
	}
	var stage string
	if j.Ready {
		if len(j.Attempts) == 0 {
			return b, fmt.Errorf("bootstrap staging reference missing")
		}
		stage = ".signal-inbox-stage-" + j.Attempts[len(j.Attempts)-1]
		d, e := parent.Sub(stage)
		if e != nil {
			return b, e
		}
		e = verifyFreshInboxFiles(d, j)
		d.Close()
		if e != nil {
			return b, e
		}
		s, e := openFreshInboxImage(filepath.Join(filepath.Dir(state), stage), b)
		if e != nil {
			return b, e
		}
		if e = s.Close(); e != nil {
			return b, e
		}
	} else {
		// An unfinished private staging directory is never authority. Reserve a new
		// bounded attempt with the SAME identity; retain old staging for diagnosis.
		if len(j.Attempts) == 16 {
			return b, fmt.Errorf("bootstrap staging retry bound reached; inspect retained attempts")
		}
		attempt, e := safefs.NewID("n")
		if e != nil {
			return b, e
		}
		j.Attempts = append(j.Attempts, attempt)
		if err = parent.WriteJSON(journalName, j, true); err != nil {
			return b, err
		}
		stage = ".signal-inbox-stage-" + attempt
		if err = unix.Mkdirat(int(parent.File().Fd()), stage, 0700); err != nil {
			return b, err
		}
		d, e := parent.Sub(stage)
		if e != nil {
			return b, e
		}
		e = d.WriteJSON("fresh-bootstrap.json", b, false)
		d.Close()
		if e != nil {
			return b, e
		}
		if err = boundary("staging"); err != nil {
			return b, err
		}
		s, e := openInboxLeasedSeed(filepath.Join(filepath.Dir(state), stage), account, true, nil, nil, nil, &b.Identity)
		if e != nil {
			return b, e
		}
		// Closing the sole connection checkpoints WAL before the directory move.
		if e = s.Close(); e != nil {
			return b, e
		}
		published, e := parent.Sub(stage)
		if e != nil {
			return b, e
		}
		j.Device, j.Inode, e = freshInboxFileIdentity(published.File())
		if e == nil {
			f, openErr := published.Open("inbox.db", os.O_RDONLY, 0)
			e = openErr
			if e == nil {
				j.DatabaseDevice, j.DatabaseInode, e = freshInboxFileIdentity(f)
				f.Close()
			}
		}
		if e == nil {
			e = published.File().Sync()
		}
		published.Close()
		if e != nil {
			return b, e
		}
		j.Ready = true
		if err = parent.WriteJSON(journalName, j, true); err != nil {
			return b, err
		}
	}
	if err = boundary("ready"); err != nil {
		return b, err
	}
	if err = parent.Rename(stage, parent, name); err != nil {
		return b, err
	}
	if err = boundary("published"); err != nil {
		return b, err
	}
	j.Complete = true
	if err = parent.WriteJSON(journalName, j, true); err != nil {
		return b, err
	}
	return b, boundary("complete")
}

func privateFreshInboxDirectory(d *safefs.Directory) error {
	info, err := d.File().Stat()
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
		return fmt.Errorf("Signal inbox bootstrap parent must be owned and private")
	}
	return nil
}

func openFreshInboxImage(path string, b FreshInboxBootstrap) (*Inbox, error) {
	d, err := safefs.Open(path)
	if err != nil {
		return nil, err
	}
	var retained FreshInboxBootstrap
	err = d.ReadJSON("fresh-bootstrap.json", &retained)
	d.Close()
	if err != nil || retained != b || b.validate() != nil {
		return nil, fmt.Errorf("fresh inbox bootstrap identity changed")
	}
	s, err := OpenInbox(path, b.Account, false)
	if err != nil {
		return nil, err
	}
	if s.identity != b.Identity || s.baseline != nil || s.installation != nil {
		s.Close()
		return nil, fmt.Errorf("fresh inbox identity or history changed")
	}
	return s, nil
}

// OpenFreshSignalInbox requires its genuine first-install receipt and an active
// service admission callback. It never synthesizes a recovery cursor or account
// handoff. The ordinary exclusive inbox lease still permits only one receiver.
func OpenFreshSignalInbox(ctx context.Context, b FreshInboxBootstrap, admission func(context.Context) error) (*Inbox, error) {
	if b.validate() != nil || admission == nil {
		return nil, fmt.Errorf("fresh inbox requires reservation and service admission")
	}
	var s *Inbox
	check := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		parent, err := safefs.Open(filepath.Dir(b.State))
		if err != nil {
			return err
		}
		defer parent.Close()
		if err = privateFreshInboxDirectory(parent); err != nil {
			return err
		}
		name := ".signal-inbox-" + safefs.LockName(filepath.Base(b.State)) + ".json"
		f, err := parent.Open(name+".lock", os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("fresh inbox reservation lock missing")
		}
		f.Close()
		var j freshInboxJournal
		if err = parent.ReadJSON(name, &j); err != nil {
			return err
		}
		if j.Reservation != b || !j.Complete || !j.Ready || len(j.Attempts) == 0 || len(j.Attempts) > 16 {
			return fmt.Errorf("fresh inbox publication is incomplete or changed")
		}
		current, err := parent.Sub(filepath.Base(b.State))
		if err != nil {
			return err
		}
		defer current.Close()
		if err = verifyFreshInboxFiles(current, j); err != nil {
			return err
		}
		var retained FreshInboxBootstrap
		if err = current.ReadJSON("fresh-bootstrap.json", &retained); err != nil || retained != b {
			return fmt.Errorf("fresh inbox reservation changed")
		}
		if s != nil {
			held, e := s.dir.File().Stat()
			if e != nil {
				return e
			}
			actual, e := current.File().Stat()
			if e != nil {
				return e
			}
			if !os.SameFile(held, actual) {
				return fmt.Errorf("fresh inbox canonical directory replaced")
			}
		}
		return admission(ctx)
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	var err error
	s, err = openFreshInboxImage(b.State, b)
	if err != nil {
		return nil, err
	}
	s.runtimeAdmission = check
	s.freshBootstrap = &b
	if err = s.CheckActive(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func freshInboxFileIdentity(f *os.File) (uint64, uint64, error) {
	info, err := f.Stat()
	if err != nil {
		return 0, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != uint32(os.Getuid()) {
		return 0, 0, fmt.Errorf("fresh inbox file owner changed")
	}
	return uint64(st.Dev), st.Ino, nil
}

// InspectFreshSignalInbox verifies retained publication while its runtime owns
// the inbox lease. It does not open SQLite, initialize state or repair records.
func InspectFreshSignalInbox(b FreshInboxBootstrap) error {
	if err := b.validate(); err != nil {
		return err
	}
	parent, err := safefs.Open(filepath.Dir(b.State))
	if err != nil {
		return err
	}
	defer parent.Close()
	if err = privateFreshInboxDirectory(parent); err != nil {
		return err
	}
	name := ".signal-inbox-" + safefs.LockName(filepath.Base(b.State)) + ".json"
	lock, err := parent.Lock(name+".lock", false, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	var j freshInboxJournal
	if err = parent.ReadJSON(name, &j); err != nil {
		return err
	}
	if j.Reservation != b || !j.Complete || !j.Ready || len(j.Attempts) == 0 || len(j.Attempts) > 16 {
		return fmt.Errorf("fresh inbox publication is incomplete or changed")
	}
	d, err := parent.Sub(filepath.Base(b.State))
	if err != nil {
		return err
	}
	defer d.Close()
	if err = verifyFreshInboxFiles(d, j); err != nil {
		return err
	}
	var retained FreshInboxBootstrap
	if err = d.ReadJSON("fresh-bootstrap.json", &retained); err != nil {
		return err
	}
	if retained != b {
		return fmt.Errorf("fresh inbox reservation changed")
	}
	return nil
}

func verifyFreshInboxFiles(d *safefs.Directory, j freshInboxJournal) error {
	if !j.Ready || j.Inode == 0 || j.DatabaseInode == 0 {
		return fmt.Errorf("fresh inbox publication lost its file identity pins")
	}
	if err := privateFreshInboxDirectory(d); err != nil {
		return err
	}
	device, inode, err := freshInboxFileIdentity(d.File())
	if err != nil {
		return err
	}
	if device != j.Device || inode != j.Inode {
		return fmt.Errorf("fresh inbox directory was replaced; recovery required")
	}
	f, err := d.Open("inbox.db", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0600 {
		return fmt.Errorf("fresh inbox database must remain private")
	}
	device, inode, err = freshInboxFileIdentity(f)
	if err != nil {
		return err
	}
	if device != j.DatabaseDevice || inode != j.DatabaseInode {
		return fmt.Errorf("fresh inbox database was replaced; recovery required")
	}
	return nil
}
