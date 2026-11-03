package worker

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

type receiptCompactionCrashInput struct {
	Config   Config
	Identity protocol.Identity
	Now      time.Time
	Boundary string
}

func TestReceiptCompactionCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_COMPACTION_CRASH_STATE")
	if path == "" {
		t.Skip("child helper")
	}
	d, err := safefs.Open(path)
	must(t, err)
	defer d.Close()
	var input receiptCompactionCrashInput
	must(t, d.ReadJSON("compaction-crash.json", &input))
	s, err := OpenManagedVerification(path, input.Config, input.Identity)
	must(t, err)
	defer s.Close()
	s.now = func() time.Time { return input.Now }
	c := receiptCompactor{s: s, fault: func(point string) error {
		if point == input.Boundary {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}}
	_, err = c.run(context.Background(), input.Config.Producers[0].Producer, 128, true)
	t.Fatal("crash boundary not reached", err)
}

func TestReceiptCompactionSIGKILL(t *testing.T) {
	for _, point := range []string{"receipt_compaction_deleted", "receipt_compaction_transaction", "receipt_compaction_commit"} {
		t.Run(point, func(t *testing.T) {
			s, now, path, _, cursor := receiptCompactionFixture(t)
			c, id := s.config, s.identity
			must(t, s.dir.WriteJSON("compaction-crash.json", receiptCompactionCrashInput{c, id, *now, point}, false))
			must(t, s.Close())
			executable, err := os.Executable()
			must(t, err)
			cmd := exec.Command(executable, "-test.run=^TestReceiptCompactionCrashHelper$", "-test.timeout=15s")
			cmd.Env = append(os.Environ(), "DEPLOY_COMPACTION_CRASH_STATE="+path)
			_, err = process.LimitedOutput(cmd, 20*time.Second, 8192)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("missing crash", err)
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal(cmd.ProcessState, err)
			}
			s, err = OpenManagedVerification(path, c, id)
			must(t, err)
			defer s.Close()
			s.now = func() time.Time { return *now }
			page, err := s.InspectReceipts(context.Background(), producer, 0, 128)
			must(t, err)
			if point == "receipt_compaction_commit" {
				if len(page.Receipts) != 1 || page.Compaction == nil || page.Compaction.Removed != 3 {
					t.Fatal(page)
				}
			} else if len(page.Receipts) != 4 || page.Compaction != nil {
				t.Fatal("partial compaction survived crash", page)
			}
			if page.AcknowledgedThrough != cursor.Position || page.ListedThrough != cursor.Position {
				t.Fatal("crash changed cursors", page)
			}
			_, err = s.CompactReceipts(context.Background(), producer, 128, true)
			must(t, err)
			page, err = s.InspectReceipts(context.Background(), producer, 0, 128)
			must(t, err)
			if len(page.Receipts) != 1 || page.Compaction.Removed != 3 || page.Compaction.Passes != 1 {
				t.Fatal("resumption duplicated deletion audit", page)
			}
		})
	}
}
