package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"golang.org/x/sys/unix"
)

// GatewayBootstrap reserves a real gateway identity before any producer exists.
// It grants neither delivery authority nor command/LED admission. Config contains
// resolved route identities and budgets, never credentials or temporary producers.
type GatewayBootstrap struct {
	Version   int               `json:"version"`
	Operation string            `json:"operation"`
	State     string            `json:"state"`
	Identity  protocol.Identity `json:"identity"`
	Config    Config            `json:"config"`
}

type gatewayBootstrapJournal struct {
	Reservation GatewayBootstrap `json:"reservation"`
	Attempts    []string         `json:"attempts,omitempty"`
	Ready       bool             `json:"ready"`
	Complete    bool             `json:"complete"`
}

func (b GatewayBootstrap) validate() error {
	if b.Version != 1 || !safefs.ValidID(b.Operation, "n") || !b.Identity.Valid() || !filepath.IsAbs(b.State) || filepath.Clean(b.State) != b.State || b.State == "/" {
		return fmt.Errorf("invalid unbound gateway reservation")
	}
	return b.Config.ValidateBootstrap()
}

// BootstrapGateway atomically publishes an initialized unbound state. Retries
// retain the reserved identity; an existing directory is never initialized or
// adopted without this operation's canonical reservation. The parent must exist
// and be owned and private. No provider or service calls are made here.
func BootstrapGateway(ctx context.Context, state, operation string, c Config) (GatewayBootstrap, error) {
	return bootstrapGateway(ctx, state, operation, c, nil)
}

func bootstrapGateway(ctx context.Context, state, operation string, c Config, fault func(string) error) (GatewayBootstrap, error) {
	var zero GatewayBootstrap
	if !filepath.IsAbs(state) || filepath.Clean(state) != state || state == "/" || !safefs.ValidID(operation, "n") {
		return zero, fmt.Errorf("absolute state and stable bootstrap operation required")
	}
	if err := c.ValidateBootstrap(); err != nil {
		return zero, err
	}
	parent, err := safefs.Open(filepath.Dir(state))
	if err != nil {
		return zero, err
	}
	defer parent.Close()
	if err = privateBootstrapDirectory(parent); err != nil {
		return zero, err
	}
	name := filepath.Base(state)
	journalName := ".gateway-" + safefs.LockName(name) + ".json"
	claimed := false
	if f, e := parent.Open(journalName+".lock", os.O_RDONLY, 0); e == nil {
		claimed = true
		f.Close()
	} else if !os.IsNotExist(e) {
		return zero, e
	}
	if !claimed {
		if f, e := parent.Open(journalName, os.O_RDONLY, 0); e == nil {
			f.Close()
			return zero, fmt.Errorf("bootstrap lock missing; recovery required")
		} else if !os.IsNotExist(e) {
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
	var j gatewayBootstrapJournal
	err = parent.ReadJSON(journalName, &j)
	if os.IsNotExist(err) {
		if claimed {
			return zero, fmt.Errorf("bootstrap reservation missing; recovery required")
		}
		if existing, e := parent.Sub(name); e == nil {
			existing.Close()
			return zero, fmt.Errorf("state exists without bootstrap authority; recovery required")
		} else if !os.IsNotExist(e) {
			return zero, e
		}
		id, e := safefs.NewID("n")
		if e != nil {
			return zero, e
		}
		recovery, e := safefs.NewID("n")
		if e != nil {
			return zero, e
		}
		j.Reservation = GatewayBootstrap{1, operation, state, protocol.Identity{Worker: id, Recovery: recovery}, c}
		if err = parent.WriteJSON(journalName, j, false); err != nil {
			return zero, err
		}
	} else if err != nil {
		return zero, err
	}
	b := j.Reservation
	if b.validate() != nil || b.Operation != operation || b.State != state || protocol.Hash(b.Config) != protocol.Hash(c) || len(j.Attempts) > 16 || (j.Complete && !j.Ready) {
		return zero, fmt.Errorf("bootstrap policy or operation differs from reserved gateway")
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
		existing.Close()
		if !j.Ready {
			return b, fmt.Errorf("unowned state appeared during bootstrap")
		}
		s, e := OpenBootstrapVerification(state, c, b.Identity, "")
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
	} else if !os.IsNotExist(e) {
		return b, e
	}
	if j.Complete {
		return b, fmt.Errorf("completed gateway state missing; recovery required")
	}
	var stage string
	if j.Ready {
		if len(j.Attempts) == 0 {
			return b, fmt.Errorf("bootstrap staging reference missing")
		}
		stage = ".gateway-stage-" + j.Attempts[len(j.Attempts)-1]
		s, e := OpenBootstrapVerification(filepath.Join(filepath.Dir(state), stage), c, b.Identity, "")
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
		stage = ".gateway-stage-" + attempt
		if err = unix.Mkdirat(int(parent.File().Fd()), stage, 0700); err != nil {
			return b, err
		}
		d, e := parent.Sub(stage)
		if e != nil {
			return b, e
		}
		e = d.WriteJSON("bootstrap.json", b, false)
		d.Close()
		if e != nil {
			return b, e
		}
		if err = boundary("staging"); err != nil {
			return b, err
		}
		s, e := openStoreWithBootstrap(filepath.Join(filepath.Dir(state), stage), c, true, nil, false, b.Identity, "", &b)
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
		e = published.File().Sync()
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

func privateBootstrapDirectory(d *safefs.Directory) error {
	info, err := d.File().Stat()
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0700 {
		return fmt.Errorf("gateway bootstrap parent must be owned and private")
	}
	return nil
}

// OpenBootstrapVerification is a distinct API: ordinary Open and all delivery
// entry points still reject empty enrollment. revision is only for a caller
// already holding and checking the managed installation lease.
func OpenBootstrapVerification(state string, c Config, expected protocol.Identity, revision string) (*Store, error) {
	if !expected.Valid() || (revision != "" && !safefs.ValidID(revision, "n")) {
		return nil, fmt.Errorf("bootstrap requires pinned identity and optional managed revision")
	}
	d, err := safefs.Open(state)
	if err != nil {
		return nil, err
	}
	var b GatewayBootstrap
	err = d.ReadJSON("bootstrap.json", &b)
	d.Close()
	if err != nil {
		return nil, err
	}
	if b.Identity != expected {
		return nil, fmt.Errorf("gateway bootstrap identity changed")
	}
	s, err := openStoreWithBootstrap(state, c, false, nil, true, expected, "", &b)
	if err == nil {
		s.installationRevision = revision
	}
	return s, err
}
