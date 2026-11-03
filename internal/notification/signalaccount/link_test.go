package signalaccount

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func fixture(t *testing.T) (string, string, linker, *int) {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := safefs.NewID("n")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	l := linker{now: time.Now, run: func(context.Context, *Lease, string, io.Writer) error { calls++; return nil }}
	return path, id, l, &calls
}
func requireRuntime(t *testing.T, path string, allowed bool) {
	t.Helper()
	l, err := AcquireRuntime(path)
	if err == nil {
		l.Close()
	}
	if (err == nil) != allowed {
		t.Fatalf("runtime allowed=%v, want %v: %v", err == nil, allowed, err)
	}
}

func TestLinkLeaseAndExactRetry(t *testing.T) {
	path, id, l, calls := fixture(t)
	runtime, err := AcquireRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.link(context.Background(), path, "device", id, io.Discard); err == nil {
		t.Fatal("link bypassed relay lease")
	}
	if *calls != 0 {
		t.Fatal("busy link executed")
	}
	if _, err = os.Stat(filepath.Join(path, headFile)); !os.IsNotExist(err) {
		t.Fatal("busy link wrote intent", err)
	}
	runtime.Close()
	s, err := l.link(context.Background(), path, "device", id, io.Discard)
	if err != nil || s.Operation != id || s.State != "linked" || !s.RuntimeAllowed || *calls != 1 {
		t.Fatal(s, err, *calls)
	}
	if _, err = l.link(context.Background(), path, "device", id, io.Discard); err != nil || *calls != 1 {
		t.Fatal("retry relaunched", err, *calls)
	}
	if _, err = l.link(context.Background(), path, "other", id, io.Discard); err == nil {
		t.Fatal("ID reused with new name")
	}
	requireRuntime(t, path, true)
	next, _ := safefs.NewID("n")
	if _, err = l.link(context.Background(), path, "second", next, io.Discard); err != nil || *calls != 2 {
		t.Fatal(err, *calls)
	}
	if _, err = l.link(context.Background(), path, "device", id, io.Discard); err == nil {
		t.Fatal("historical ID resurrected")
	}
}

func TestLinkInterruptionAndReview(t *testing.T) {
	for _, point := range []string{"link_intent", "link_head", "link_started", "link_child_finished", "link_result"} {
		t.Run(point, func(t *testing.T) {
			path, id, l, calls := fixture(t)
			l.fault = func(p string) error {
				if p == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := l.link(context.Background(), path, "device", id, io.Discard); err == nil {
				t.Fatal("missing interruption")
			}
			status, err := Inspect(path)
			if err != nil {
				t.Fatal(err)
			}
			if status.RuntimeAllowed != (point == "link_result") {
				t.Fatal(status)
			}
			requireRuntime(t, path, point == "link_result")
			l.fault = nil
			if point == "link_intent" || point == "link_head" {
				other, _ := safefs.NewID("n")
				if _, err = l.link(context.Background(), path, "device", other, io.Discard); err == nil {
					t.Fatal("superseded prepared intent")
				}
				if _, err = l.link(context.Background(), path, "device", id, io.Discard); err != nil || *calls != 1 {
					t.Fatal(err, *calls)
				}
			} else if point == "link_result" {
				if _, err = l.link(context.Background(), path, "device", id, io.Discard); err != nil || *calls != 1 {
					t.Fatal(err, *calls)
				}
			} else {
				before := *calls
				if _, err = l.link(context.Background(), path, "device", id, io.Discard); err == nil || *calls != before {
					t.Fatal("uncertain link retried", err, *calls)
				}
				if _, err = Resolve(path, id, "Inspected account state after interrupted linking"); err != nil {
					t.Fatal(err)
				}
				if _, err = Resolve(path, id, "Inspected account state after interrupted linking"); err != nil {
					t.Fatal("review retry", err)
				}
				if _, err = Resolve(path, id, "different reason"); err == nil {
					t.Fatal("review reason changed")
				}
				if _, err = l.link(context.Background(), path, "device", id, io.Discard); err != nil || *calls != before {
					t.Fatal("reviewed request relaunched", err, *calls)
				}
			}
			requireRuntime(t, path, true)
		})
	}
}

func TestLinkFailedChildAndPrivateOutput(t *testing.T) {
	path, id, l, _ := fixture(t)
	const sensitive = "sgnl://linkdevice?uuid=private-linking-material"
	l.run = func(_ context.Context, _ *Lease, _ string, out io.Writer) error {
		_, _ = io.WriteString(out, sensitive)
		return errors.New(sensitive)
	}
	var out bytes.Buffer
	status, err := l.link(context.Background(), path, "device", id, &out)
	if err == nil || strings.Contains(err.Error(), sensitive) || status.State != "uncertain" || status.RuntimeAllowed || out.String() != sensitive {
		t.Fatal(status, err)
	}
	if err = filepath.WalkDir(path, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, e := os.ReadFile(name)
		if e != nil {
			return e
		}
		if bytes.Contains(raw, []byte(sensitive)) {
			t.Fatal("linking secret persisted", name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requireRuntime(t, path, false)
	if _, err = Resolve(path, id, ""); err == nil {
		t.Fatal("empty reason accepted")
	}
	other, _ := safefs.NewID("n")
	if _, err = Resolve(path, other, "reviewed"); err == nil {
		t.Fatal("wrong operation resolved")
	}
	if _, err = Resolve(path, id, "Reviewed configured account and linked devices"); err != nil {
		t.Fatal(err)
	}
	requireRuntime(t, path, true)
}

func TestLinkPreparedCanBeAbandonedWithoutExecution(t *testing.T) {
	path, id, l, calls := fixture(t)
	l.fault = func(string) error { return errors.New("stop") }
	if _, err := l.link(context.Background(), path, "device", id, io.Discard); err == nil {
		t.Fatal("missing fault")
	}
	s, err := Resolve(path, id, "Cancelled before launching the CLI")
	if err != nil || s.State != "reviewed" || !s.RuntimeAllowed || *calls != 0 {
		t.Fatal(s, err, *calls)
	}
	requireRuntime(t, path, true)
}

func TestLinkHistoryDamageFencesRuntime(t *testing.T) {
	for _, damage := range []string{"missing-intent", "missing-start", "missing-result", "changed-head", "unknown-file", "symlink-result", "too-many-temporaries"} {
		t.Run(damage, func(t *testing.T) {
			path, id, l, _ := fixture(t)
			if _, err := l.link(context.Background(), path, "device", id, io.Discard); err != nil {
				t.Fatal(err)
			}
			q := filepath.Join(path, historyDirectory)
			var err error
			switch damage {
			case "missing-intent":
				err = os.Remove(filepath.Join(q, recordFile(1, "intent")))
			case "missing-start":
				err = os.Remove(filepath.Join(q, recordFile(1, "start")))
			case "missing-result":
				err = os.Remove(filepath.Join(q, recordFile(1, "result")))
			case "changed-head":
				err = os.WriteFile(filepath.Join(path, headFile), []byte(`{"Version":1,"Sequence":2}`), 0600)
			case "unknown-file":
				err = os.WriteFile(filepath.Join(q, "foreign"), []byte("x"), 0600)
			case "symlink-result":
				name := filepath.Join(q, recordFile(1, "result"))
				if err = os.Rename(name, name+".old"); err == nil {
					err = os.Symlink(name+".old", name)
				}
			case "too-many-temporaries":
				for i := 0; i < 33; i++ {
					v, _ := safefs.NewID("n")
					if err = os.WriteFile(filepath.Join(q, "."+v+".json"), nil, 0600); err != nil {
						break
					}
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			requireRuntime(t, path, false)
		})
	}
}

func TestLinkBoundsClockAndPath(t *testing.T) {
	path, id, l, calls := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.link(ctx, path, "device", id, io.Discard); err == nil || *calls != 0 {
		t.Fatal(err, *calls)
	}
	if _, err := l.link(context.Background(), path, "bad\nname", id, io.Discard); err == nil {
		t.Fatal("invalid name")
	}
	for i := 0; i < maxLinks; i++ {
		id, _ = safefs.NewID("n")
		if _, err := l.link(context.Background(), path, "device", id, io.Discard); err != nil {
			t.Fatal(i, err)
		}
	}
	other, _ := safefs.NewID("n")
	if _, err := l.link(context.Background(), path, "device", other, io.Discard); err == nil {
		t.Fatal("unbounded link history")
	}
	if _, err := l.link(context.Background(), path, "device", id, io.Discard); err != nil {
		t.Fatal("exact retry at capacity", err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRuntime(alias); err == nil {
		t.Fatal("symlink account directory accepted")
	}
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireRuntime(path); err == nil {
		t.Fatal("non-private account directory accepted")
	}
	var out bytes.Buffer
	w := linkOutput{writer: &out, remaining: 2}
	if _, err := w.Write([]byte("abc")); err == nil || out.Len() != 0 {
		t.Fatal("output not bounded")
	}
}

func TestLinkClockRollbackLeavesFence(t *testing.T) {
	path, id, l, _ := fixture(t)
	now := time.Now()
	calls := 0
	l.now = func() time.Time {
		calls++
		if calls >= 3 {
			return now.Add(-time.Hour)
		}
		return now
	}
	if _, err := l.link(context.Background(), path, "device", id, io.Discard); err == nil {
		t.Fatal("clock rollback not detected")
	}
	requireRuntime(t, path, false)
	d, err := safefs.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	h, err := readHistory(d)
	if err != nil || h.Records[0].Result != nil || protocol.Hash(h.Records[0].Start) == "" {
		t.Fatal(h, err)
	}
}
