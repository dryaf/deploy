package worker

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
)

func quarantineFixture(t *testing.T) (string, string, Config, SnapshotManifest) {
	t.Helper()
	s, now, path := fixture(t)
	m := report(*now, "retained", 1, 1, 1)
	_, err := s.Enqueue(context.Background(), m)
	must(t, err)
	from := filepath.Join(t.TempDir(), "snapshot")
	manifest, err := s.Snapshot(context.Background(), from)
	must(t, err)
	c := s.config
	must(t, s.Close())
	return path, from, c, manifest
}

func TestQuarantineRecoveryExactRetry(t *testing.T) {
	path, from, c, manifest := quarantineFixture(t)
	ctx := context.Background()
	first, err := QuarantineRecovery(ctx, path, from, c, manifest.ID, manifest.DatabaseHash)
	must(t, err)
	if first.Snapshot != manifest || first.Before != manifest.Identity || first.After == first.Before || first.Phase != "quarantined" || first.DeliveryEnabled || !safefs.DigestPattern.MatchString(first.OriginalSourceHash) {
		t.Fatal(first)
	}
	again, err := QuarantineRecovery(ctx, path, from, c, manifest.ID, manifest.DatabaseHash)
	must(t, err)
	if again != first {
		t.Fatal("quarantine retry changed identity or evidence")
	}
	verified, err := VerifySnapshot(ctx, from, c)
	must(t, err)
	if verified != manifest {
		t.Fatal("original snapshot changed")
	}
	if store, err := Open(path, c, false); err == nil {
		store.Close()
		t.Fatal("recovery preparation activated worker")
	}
	dir, err := safefs.Open(path)
	must(t, err)
	defer dir.Close()
	db, err := snapshotDB(dir, "state.db", true)
	must(t, err)
	defer db.Close()
	var raw string
	must(t, db.QueryRow("SELECT value FROM metadata WHERE key='identity'").Scan(&raw))
	var original protocol.Identity
	must(t, json.Unmarshal([]byte(raw), &original))
	if original != manifest.Identity {
		t.Fatal("pre-recovery database identity changed")
	}
}

func TestQuarantineRecoveryRejectsInvalidAuthority(t *testing.T) {
	for _, kind := range []string{"wrong-hash", "wrong-id", "missing-state", "cancelled", "worker-lease", "changed-snapshot", "damaged-journal", "wrong-config", "missing-snapshot", "different-snapshot"} {
		t.Run(kind, func(t *testing.T) {
			path, from, c, m := quarantineFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			id, hash := m.ID, m.DatabaseHash
			if kind == "damaged-journal" || kind == "different-snapshot" {
				_, err := QuarantineRecovery(ctx, path, from, c, id, hash)
				must(t, err)
			}
			switch kind {
			case "wrong-hash":
				hash = protocol.Hash("another database")
			case "wrong-id":
				id, _ = safefs.NewID("n")
			case "missing-state":
				path = filepath.Join(t.TempDir(), "absent")
			case "cancelled":
				cancel()
			case "worker-lease":
				s, err := Open(path, c, false)
				must(t, err)
				defer s.Close()
			case "changed-snapshot":
				must(t, os.WriteFile(filepath.Join(from, "state.db"), []byte("changed"), 0600))
			case "damaged-journal":
				must(t, os.WriteFile(filepath.Join(path, "recovery.json"), []byte("{}"), 0600))
			case "wrong-config":
				c.MaxMessages++
			case "missing-snapshot":
				from = filepath.Join(t.TempDir(), "absent")
			case "different-snapshot":
				m.ID, _ = safefs.NewID("n")
				d, err := safefs.Open(from)
				must(t, err)
				must(t, d.WriteJSON("snapshot.json", m, true))
				d.Close()
				id = m.ID
				_, err = VerifySnapshot(ctx, from, c)
				must(t, err)
			}
			if _, err := QuarantineRecovery(ctx, path, from, c, id, hash); err == nil {
				t.Fatal("unsafe preparation accepted")
			}
			if kind == "missing-state" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("initialized missing worker", err)
				}
			} else if kind != "damaged-journal" && kind != "different-snapshot" {
				if _, err := os.Stat(filepath.Join(path, "recovery.json")); !os.IsNotExist(err) {
					t.Fatal("invalid input created recovery authority", err)
				}
			}
		})
	}
}

func TestQuarantineRecoveryRejectsSnapshotChangeBeforePreparation(t *testing.T) {
	path, from, c, m := quarantineFixture(t)
	m.ID, _ = safefs.NewID("n")
	if _, err := preparePinnedSnapshotRecovery(context.Background(), path, from, c, &m, nil); err == nil {
		t.Fatal("snapshot changed between verification and preparation")
	}
	if _, err := os.Stat(filepath.Join(path, "recovery.json")); !os.IsNotExist(err) {
		t.Fatal("changed snapshot created recovery authority", err)
	}
}

type quarantineCrashInput struct {
	State, Snapshot, ID, Hash, Point string
	Config                           Config
}

func TestQuarantineRecoveryCrashHelper(t *testing.T) {
	path := os.Getenv("DEPLOY_QUARANTINE_CLI_CRASH")
	if path == "" {
		t.Skip("subprocess only")
	}
	raw, err := os.ReadFile(path)
	must(t, err)
	var input quarantineCrashInput
	must(t, json.Unmarshal(raw, &input))
	_, err = quarantineRecovery(context.Background(), input.State, input.Snapshot, input.Config, input.ID, input.Hash, func(at string) error {
		if at == input.Point {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatal("crash boundary missed", err)
}

func TestQuarantineRecoverySIGKILL(t *testing.T) {
	for _, point := range []string{"prepared", "identity", "epoch_published", "binding", "classified", "committed", "quarantined", "original_intent", "original_copy", "original_archive", "original_acknowledged", "original_ready", "quarantine_cli_result"} {
		t.Run(point, func(t *testing.T) {
			path, from, c, manifest := quarantineFixture(t)
			raw, err := json.Marshal(quarantineCrashInput{path, from, manifest.ID, manifest.DatabaseHash, point, c})
			must(t, err)
			input := filepath.Join(t.TempDir(), "input.json")
			must(t, os.WriteFile(input, raw, 0600))
			cmd := exec.Command(os.Args[0], "-test.run=^TestQuarantineRecoveryCrashHelper$", "-test.timeout=30s")
			cmd.Env = append(os.Environ(), "DEPLOY_QUARANTINE_CLI_CRASH="+input)
			out, err := process.LimitedOutput(cmd, 35*time.Second, 16384)
			if err == nil || cmd.ProcessState == nil {
				t.Fatal("child did not crash", err, string(out))
			}
			status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatal("unexpected exit", cmd.ProcessState, string(out))
			}
			d, err := safefs.Open(path)
			must(t, err)
			var original snapshotRecovery
			must(t, d.ReadJSON("recovery.json", &original))
			d.Close()
			v, err := QuarantineRecovery(context.Background(), path, from, c, manifest.ID, manifest.DatabaseHash)
			must(t, err)
			if v.Operation != original.Operation || v.After != original.After || v.Phase != "quarantined" || v.DeliveryEnabled {
				t.Fatal("resume replaced operation or escaped quarantine", v)
			}
			replay, err := QuarantineRecovery(context.Background(), path, from, c, manifest.ID, manifest.DatabaseHash)
			must(t, err)
			if replay != v {
				t.Fatal("completion replay changed")
			}
		})
	}
}
