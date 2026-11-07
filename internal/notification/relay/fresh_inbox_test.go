package relay

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestFreshInboxBootstrapResumesEveryPublicationBoundary(t *testing.T) {
	for _, boundary := range []string{"reserved", "staging", "ready", "published", "complete"} {
		t.Run(boundary, func(t *testing.T) {
			root := freshInboxTestParent(t)
			path := filepath.Join(root, "inbox")
			op := testutil.ID(t, "n")
			first, err := bootstrapFreshInbox(context.Background(), path, op, fixtureAccount, func(point string) error {
				if point == boundary {
					return errors.New("interruption")
				}
				return nil
			})
			if err == nil || !first.Identity.Valid() {
				t.Fatal("missing retained interruption", first, err)
			}
			done, err := BootstrapFreshInbox(context.Background(), path, op, fixtureAccount)
			testutil.Must(t, err)
			if done != first {
				t.Fatal("retry changed inbox identity")
			}
			again, err := BootstrapFreshInbox(context.Background(), path, op, fixtureAccount)
			testutil.Must(t, err)
			if again != done {
				t.Fatal("completed retry changed identity")
			}
			s, err := OpenFreshSignalInbox(context.Background(), done, func(context.Context) error { return nil })
			testutil.Must(t, err)
			defer s.Close()
			if s.Cursor().Position != 0 || s.baseline != nil || s.installation != nil {
				t.Fatal("fresh inbox fabricated recovery history")
			}
			var count int
			testutil.Must(t, s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE '%recovery%'").Scan(&count))
			if count != 0 {
				t.Fatal("fresh inbox has recovery tables")
			}
			if _, err = OpenFreshSignalInbox(context.Background(), done, func(context.Context) error { return nil }); err == nil {
				t.Fatal("two receivers acquired inbox")
			}
		})
	}
}

func TestFreshInboxIntakeRechecksAdmissionAndCanonicalFiles(t *testing.T) {
	path := filepath.Join(freshInboxTestParent(t), "inbox")
	b, err := BootstrapFreshInbox(context.Background(), path, testutil.ID(t, "n"), fixtureAccount)
	testutil.Must(t, err)
	allowed := true
	s, err := OpenFreshSignalInbox(context.Background(), b, func(context.Context) error {
		if !allowed {
			return errors.New("service admission removed")
		}
		return nil
	})
	testutil.Must(t, err)
	defer s.Close()
	event := interaction.SpoolEvent{Key: "fresh-event", Hash: protocol.Hash("synthetic fixture")}
	testutil.Must(t, s.Enqueue(context.Background(), event))
	allowed = false
	if err = s.Enqueue(context.Background(), interaction.SpoolEvent{Key: "unadmitted", Hash: protocol.Hash("two")}); err == nil {
		t.Fatal("lost admission still received")
	}
	allowed = true
	page, err := s.List(context.Background(), s.Cursor())
	testutil.Must(t, err)
	if len(page.Events) != 1 {
		t.Fatal("unadmitted event persisted")
	}
	testutil.Must(t, os.Rename(path, path+".old"))
	testutil.Must(t, os.Mkdir(path, 0700))
	if err = s.CheckActive(context.Background()); err == nil {
		t.Fatal("replaced canonical directory still active")
	}
}

func TestFreshInboxNeverReinitializesLostAuthority(t *testing.T) {
	for _, part := range []string{"journal", "lock", "state", "database", "marker"} {
		t.Run(part, func(t *testing.T) {
			parent := freshInboxTestParent(t)
			path := filepath.Join(parent, "inbox")
			b, err := BootstrapFreshInbox(context.Background(), path, testutil.ID(t, "n"), fixtureAccount)
			testutil.Must(t, err)
			journal := filepath.Join(parent, ".signal-inbox-"+safefs.LockName("inbox")+".json")
			lost := map[string]string{"journal": journal, "lock": journal + ".lock", "state": path, "database": filepath.Join(path, "inbox.db"), "marker": filepath.Join(path, "fresh-bootstrap.json")}[part]
			testutil.Must(t, os.Rename(lost, lost+".saved"))
			if _, err = BootstrapFreshInbox(context.Background(), path, b.Operation, b.Account); err == nil {
				t.Fatal("lost authority reinitialized")
			}
			if _, err = OpenFreshSignalInbox(context.Background(), b, func(context.Context) error { return nil }); err == nil {
				t.Fatal("lost authority admitted")
			}
			if _, err = os.Lstat(lost); !os.IsNotExist(err) {
				t.Fatal("missing authority recreated", err)
			}
		})
	}
}

func TestFreshInboxForeignStateAndRetargeting(t *testing.T) {
	parent := freshInboxTestParent(t)
	path := filepath.Join(parent, "inbox")
	testutil.Must(t, os.Mkdir(path, 0700))
	if _, err := BootstrapFreshInbox(context.Background(), path, testutil.ID(t, "n"), fixtureAccount); err == nil {
		t.Fatal("adopted foreign directory")
	}
	path = filepath.Join(freshInboxTestParent(t), "inbox")
	b, err := BootstrapFreshInbox(context.Background(), path, testutil.ID(t, "n"), fixtureAccount)
	testutil.Must(t, err)
	if _, err = BootstrapFreshInbox(context.Background(), path, b.Operation, fixtureActor); err == nil {
		t.Fatal("retargeted account")
	}
	if _, err = BootstrapFreshInbox(context.Background(), path, testutil.ID(t, "n"), fixtureAccount); err == nil {
		t.Fatal("retargeted operation")
	}
	// Replacing even an identical database file requires explicit recovery.
	raw, err := os.ReadFile(filepath.Join(path, "inbox.db"))
	testutil.Must(t, err)
	testutil.Must(t, os.Rename(filepath.Join(path, "inbox.db"), filepath.Join(path, "inbox.old")))
	testutil.Must(t, os.WriteFile(filepath.Join(path, "inbox.db"), raw, 0600))
	if _, err = BootstrapFreshInbox(context.Background(), path, b.Operation, b.Account); err == nil {
		t.Fatal("replaced database admitted")
	}
}

func TestFreshInboxCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_FRESH_INBOX_CRASH_PATH")
	if path == "" {
		return
	}
	_, _ = bootstrapFreshInbox(context.Background(), path, os.Getenv("DEPLOY_FRESH_INBOX_CRASH_OP"), fixtureAccount, func(point string) error {
		if point == os.Getenv("DEPLOY_FRESH_INBOX_CRASH_POINT") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("crash point was not reached")
}

func TestFreshInboxBootstrapSIGKILL(t *testing.T) {
	for _, point := range []string{"reserved", "staging", "ready", "published", "complete"} {
		t.Run(point, func(t *testing.T) {
			parent := freshInboxTestParent(t)
			path := filepath.Join(parent, "inbox")
			op := testutil.ID(t, "n")
			cmd := exec.Command(os.Args[0], "-test.run=^TestFreshInboxCrashHelper$")
			cmd.Env = append(os.Environ(), "DEPLOY_FRESH_INBOX_CRASH_PATH="+path, "DEPLOY_FRESH_INBOX_CRASH_OP="+op, "DEPLOY_FRESH_INBOX_CRASH_POINT="+point)
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("helper did not die at boundary", err)
			}
			d, err := safefs.Open(parent)
			testutil.Must(t, err)
			var journal freshInboxJournal
			testutil.Must(t, d.ReadJSON(".signal-inbox-"+safefs.LockName("inbox")+".json", &journal))
			d.Close()
			b, err := BootstrapFreshInbox(context.Background(), path, op, fixtureAccount)
			testutil.Must(t, err)
			if b != journal.Reservation {
				t.Fatal("process death changed identity")
			}
		})
	}
}

func freshInboxTestParent(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	testutil.Must(t, os.Chmod(path, 0700))
	return path
}
