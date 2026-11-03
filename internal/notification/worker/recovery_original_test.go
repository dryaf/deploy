package worker

import (
	"context"
	"database/sql"
	"errors"
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

func originalFixture(t *testing.T) *recoveryReconciler {
	t.Helper()
	path, from, c, _ := quarantineFixture(t)
	_, err := prepareSnapshotRecovery(context.Background(), path, from, c, nil)
	must(t, err)
	r, err := openRecoveryReconciler(context.Background(), path, c, nil)
	must(t, err)
	t.Cleanup(func() { r.Close() })
	return r
}

func TestRecoveryOriginalWALWriter(t *testing.T) {
	path := os.Getenv("DEPLOY_ORIGINAL_WAL_SOURCE")
	if path == "" {
		t.Skip("subprocess only")
	}
	dir, err := safefs.Open(path)
	must(t, err)
	db, err := snapshotDB(dir, "state.db", false)
	must(t, err)
	_, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO metadata VALUES('post-snapshot-wal','committed')")
	must(t, err)
	must(t, syscall.Kill(os.Getpid(), syscall.SIGKILL))
	t.Fatal("SIGKILL returned")
}

func leaveOriginalWAL(t *testing.T, r *recoveryReconciler) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryOriginalWALWriter$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "DEPLOY_ORIGINAL_WAL_SOURCE="+r.dir.File().Name())
	out, err := process.LimitedOutput(cmd, 25*time.Second, 8192)
	if err == nil || cmd.ProcessState == nil {
		t.Fatal("writer did not crash", err, string(out))
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatal("unexpected writer failure", string(out))
	}
}

func TestRecoveryOriginalIncludesCommittedWAL(t *testing.T) {
	r := originalFixture(t)
	ctx := context.Background()
	leaveOriginalWAL(t, r)
	db, err := immutableOriginalDB(r.dir, "state.db")
	must(t, err)
	var value string
	if err = db.QueryRow("SELECT value FROM metadata WHERE key='post-snapshot-wal'").Scan(&value); err != sql.ErrNoRows {
		t.Fatal("fixture commit was already in main file", value, err)
	}
	must(t, db.Close())
	hash, err := r.preserveOriginal(ctx)
	must(t, err)
	if !safefs.DigestPattern.MatchString(hash) {
		t.Fatal(hash)
	}
	db, err = r.originalDB(ctx, r.db)
	must(t, err)
	must(t, db.QueryRow("SELECT value FROM metadata WHERE key='post-snapshot-wal'").Scan(&value))
	if value != "committed" {
		t.Fatal("WAL evidence lost", value)
	}
	if _, err = db.Exec("DELETE FROM metadata"); err == nil {
		t.Fatal("archive is writable")
	}
	must(t, db.Close())
	replay, err := r.preserveOriginal(ctx)
	must(t, err)
	if replay != hash {
		t.Fatal("archive changed on retry")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err = os.Stat(filepath.Join(r.stage.File().Name(), originalArchiveFile+suffix)); !os.IsNotExist(err) {
			t.Fatal("immutable reader created sidecar", suffix, err)
		}
	}
	var state string
	must(t, r.db.QueryRow("SELECT state FROM messages WHERE id='retained'").Scan(&state))
	if state != "ambiguous" {
		t.Fatal("preservation activated restored message", state)
	}
}

func TestRecoveryOriginalRetainsLEDAndDomainProofs(t *testing.T) {
	f := newLEDEvidenceFixture(t, true, false)
	ctx := context.Background()
	var beforeSources recoveryLEDSources
	var beforeDomains recoveryLEDDomains
	must(t, f.r.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		beforeSources, err = f.r.collectLEDSources(ctx, tx)
		if err != nil {
			return err
		}
		beforeDomains, err = f.r.collectLEDDomains(ctx, tx, producer)
		return err
	}))
	_, err := f.r.preserveOriginal(ctx)
	must(t, err)
	must(t, f.r.transaction(ctx, func(tx *sql.Tx) error {
		sources, err := f.r.collectLEDSources(ctx, tx)
		if err != nil {
			return err
		}
		domains, err := f.r.collectLEDDomains(ctx, tx, producer)
		if err != nil {
			return err
		}
		if protocol.Hash(sources) != protocol.Hash(beforeSources) || protocol.Hash(domains) != protocol.Hash(beforeDomains) {
			t.Fatal("archival changed LED or domain evidence")
		}
		return nil
	}))
	v := submitLEDEvidence(t, f)
	if !v.Resolutions["selected"].DomainResolved {
		t.Fatal("archive prevented domain reconciliation", v)
	}
	// Reopen with prior LED evidence already present; preservation must not make
	// the recovery session dependent on an unresumable archive stage.
	must(t, f.r.Close())
	f.r, err = openRecoveryReconciler(ctx, f.path, f.config, nil)
	must(t, err)
	_, err = f.r.preserveOriginal(ctx)
	must(t, err)
}

func TestRecoveryOriginalInterruptedCopy(t *testing.T) {
	for _, point := range []string{"original_intent", "original_copy", "original_archive", "original_acknowledged", "original_ready"} {
		t.Run(point, func(t *testing.T) {
			r := originalFixture(t)
			ctx := context.Background()
			r.fault = func(at string) error {
				if at == point {
					return errors.New("interrupted")
				}
				return nil
			}
			if _, err := r.preserveOriginal(ctx); err == nil {
				t.Fatal("fault missed")
			}
			m, _, _, err := r.originalManifest(ctx, r.db)
			must(t, err)
			if point == "original_intent" {
				// A killed or disk-full copy can leave zero or partial bytes. Only the
				// intent-owned temporary may be removed on retry.
				must(t, os.WriteFile(filepath.Join(r.stage.File().Name(), originalCopyFile), []byte("partial"), 0600))
			}
			if point != "original_ready" {
				if db, e := r.originalDB(ctx, r.db); e == nil {
					db.Close()
					t.Fatal("pending archive fell back to mutable source")
				}
			}
			path, c := r.dir.File().Name(), r.config
			must(t, r.Close())
			resumed, err := openRecoveryReconciler(ctx, path, c, nil)
			must(t, err)
			defer resumed.Close()
			hash, err := resumed.preserveOriginal(ctx)
			must(t, err)
			if hash != m.hash() {
				t.Fatal("resume replaced manifest")
			}
			db, err := resumed.originalDB(ctx, resumed.db)
			must(t, err)
			db.Close()
		})
	}
}

func TestRecoveryOriginalDamagedAuthorityAndSourceRefused(t *testing.T) {
	for _, kind := range []string{"missing-archive", "archive-bytes", "missing-intent", "missing-ack", "empty-ack", "wrong-ack", "wrong-binding", "wrong-config", "archive-wal", "archive-shm", "archive-journal", "archive-symlink", "archive-hardlink", "archive-permissions", "manifest-permissions", "temporary-sidecar", "unexpected-temporary", "source-bytes", "source-wal", "source-identity", "lock-replaced", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			r := originalFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := r.preserveOriginal(ctx)
			must(t, err)
			archive := filepath.Join(r.stage.File().Name(), originalArchiveFile)
			switch kind {
			case "missing-archive":
				must(t, os.Remove(archive))
			case "archive-bytes":
				must(t, os.WriteFile(archive, []byte("damaged"), 0600))
			case "missing-intent":
				must(t, os.Remove(filepath.Join(r.stage.File().Name(), originalManifestFile)))
			case "missing-ack":
				_, err = r.db.Exec("DELETE FROM metadata WHERE key=?", originalArchiveKey)
				must(t, err)
			case "empty-ack", "wrong-ack":
				value := ""
				if kind == "wrong-ack" {
					value = protocol.Hash("other")
				}
				_, err = r.db.Exec("UPDATE metadata SET value=? WHERE key=?", value, originalArchiveKey)
				must(t, err)
			case "wrong-binding", "wrong-config":
				var m recoveryOriginalManifest
				must(t, r.stage.ReadJSON(originalManifestFile, &m))
				if kind == "wrong-binding" {
					m.Binding.Operation, _ = safefs.NewID("n")
				} else {
					m.ConfigHash = protocol.Hash("other")
				}
				must(t, r.stage.WriteJSON(originalManifestFile, m, true))
			case "archive-wal", "archive-shm", "archive-journal":
				suffix := map[string]string{"archive-wal": "-wal", "archive-shm": "-shm", "archive-journal": "-journal"}[kind]
				must(t, os.WriteFile(archive+suffix, []byte{}, 0600))
			case "archive-symlink":
				must(t, os.Rename(archive, archive+"-saved"))
				must(t, os.Symlink(archive+"-saved", archive))
			case "archive-hardlink":
				must(t, os.Link(archive, archive+"-linked"))
			case "archive-permissions":
				must(t, os.Chmod(archive, 0644))
			case "manifest-permissions":
				must(t, os.Chmod(filepath.Join(r.stage.File().Name(), originalManifestFile), 0644))
			case "temporary-sidecar":
				must(t, os.WriteFile(filepath.Join(r.stage.File().Name(), originalCopyFile+"-wal"), nil, 0600))
			case "unexpected-temporary":
				must(t, os.WriteFile(filepath.Join(r.stage.File().Name(), originalCopyFile), []byte("partial"), 0600))
			case "source-bytes":
				must(t, os.WriteFile(filepath.Join(r.dir.File().Name(), "state.db"), []byte("changed"), 0600))
			case "source-wal":
				var m recoveryOriginalManifest
				must(t, r.stage.ReadJSON(originalManifestFile, &m))
				leaveOriginalWAL(t, r)
				must(t, originalFileMatches(r.dir, "state.db", m)) // Main-file checksum alone misses this drift.
			case "source-identity":
				db, e := snapshotDB(r.dir, "state.db", false)
				must(t, e)
				_, err = db.Exec("UPDATE metadata SET value='{}' WHERE key='identity'")
				must(t, err)
				must(t, db.Close())
			case "lock-replaced":
				path := filepath.Join(r.dir.File().Name(), "worker.lock")
				must(t, os.Rename(path, path+"-held"))
				must(t, os.WriteFile(path, nil, 0600))
			case "cancelled":
				cancel()
			}
			if _, err = r.preserveOriginal(ctx); err == nil {
				t.Fatal("damaged archive/source accepted on retry")
			}
			if db, e := r.originalDB(ctx, r.db); e == nil {
				db.Close()
				t.Fatal("damaged archive/source served to recovery")
			}
		})
	}
}

func TestRecoveryOriginalRejectsUnownedArtifacts(t *testing.T) {
	for _, name := range []string{originalArchiveFile, originalCopyFile, originalArchiveFile + "-wal", originalCopyFile + "-journal"} {
		t.Run(name, func(t *testing.T) {
			r := originalFixture(t)
			path := filepath.Join(r.stage.File().Name(), name)
			must(t, os.WriteFile(path, []byte("unowned"), 0600))
			if _, err := r.preserveOriginal(context.Background()); err == nil {
				t.Fatal("adopted unowned artifact")
			}
			raw, err := os.ReadFile(path)
			must(t, err)
			if string(raw) != "unowned" {
				t.Fatal("unowned artifact modified")
			}
		})
	}
}

func TestRecoveryOriginalRefusesChangesAtCommitBoundaries(t *testing.T) {
	for _, point := range []string{"original_intent", "original_copy", "original_archive", "reconciliation_transaction", "original_acknowledged", "original_ready"} {
		t.Run(point, func(t *testing.T) {
			r := originalFixture(t)
			hit := false
			r.fault = func(at string) error {
				if at == point {
					hit = true
					path := filepath.Join(r.dir.File().Name(), "worker.lock")
					must(t, os.Rename(path, path+"-held"))
					must(t, os.WriteFile(path, nil, 0600))
				}
				return nil
			}
			if _, err := r.preserveOriginal(context.Background()); err == nil || !hit {
				t.Fatal("replaced lease accepted", point, err)
			}
		})
	}
	// The final result must not trust a cached acknowledgement after it was lost.
	r := originalFixture(t)
	r.fault = func(at string) error {
		if at == "original_ready" {
			_, err := r.db.Exec("DELETE FROM metadata WHERE key=?", originalArchiveKey)
			return err
		}
		return nil
	}
	if _, err := r.preserveOriginal(context.Background()); err == nil {
		t.Fatal("lost acknowledgement accepted at completion")
	}
}

func TestRecoveryOriginalResumeWithExistingLEDHistory(t *testing.T) {
	f := newLEDEvidenceFixture(t, true, false)
	submitLEDEvidence(t, f)
	f.r.fault = func(at string) error {
		if at == "original_archive" {
			return errors.New("interrupted")
		}
		return nil
	}
	if _, err := f.r.preserveOriginal(context.Background()); err == nil {
		t.Fatal("fault missed")
	}
	must(t, f.r.Close())
	var err error
	f.r, err = openRecoveryReconciler(context.Background(), f.path, f.config, nil)
	must(t, err)
	_, err = f.r.preserveOriginal(context.Background())
	must(t, err)
	_, err = f.r.LEDEvidence(context.Background(), f.inventory.InventoryID, producer)
	must(t, err)
}

func TestRecoveryOriginalInvalidSourceBeforeIntent(t *testing.T) {
	for _, kind := range []string{"identity", "schema", "enrollment", "symlink", "sidecar-symlink", "permissions", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			r := originalFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(r.dir.File().Name(), "state.db")
			switch kind {
			case "identity", "schema", "enrollment":
				db, err := snapshotDB(r.dir, "state.db", false)
				must(t, err)
				query := map[string]string{"identity": "UPDATE metadata SET value='{}' WHERE key='identity'", "schema": "PRAGMA user_version=999", "enrollment": "UPDATE producers SET epoch='foreign'"}[kind]
				_, err = db.Exec(query)
				must(t, err)
				must(t, db.Close())
			case "symlink":
				must(t, os.Rename(path, path+"-saved"))
				must(t, os.Symlink(path+"-saved", path))
			case "sidecar-symlink":
				target := filepath.Join(t.TempDir(), "unrelated")
				must(t, os.WriteFile(target, []byte("keep"), 0600))
				if err := os.Rename(path+"-wal", path+"-wal-saved"); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				must(t, os.Symlink(target, path+"-wal"))
			case "permissions":
				must(t, os.Chmod(path, 0644))
			case "cancelled":
				cancel()
			}
			if _, err := r.preserveOriginal(ctx); err == nil {
				t.Fatal("invalid source accepted")
			}
			if _, err := os.Stat(filepath.Join(r.stage.File().Name(), originalManifestFile)); !os.IsNotExist(err) {
				t.Fatal("invalid preflight published intent", err)
			}
		})
	}
}

func TestQuarantineRecoveryRechecksArchiveBeforeOutput(t *testing.T) {
	path, from, c, m := quarantineFixture(t)
	hit := false
	_, err := quarantineRecovery(context.Background(), path, from, c, m.ID, m.DatabaseHash, func(at string) error {
		if at == "quarantine_cli_result" {
			hit = true
			d, e := safefs.Open(path)
			must(t, e)
			defer d.Close()
			var j snapshotRecovery
			must(t, d.ReadJSON("recovery.json", &j))
			return os.WriteFile(filepath.Join(path, "recovery", j.Operation, originalArchiveFile), []byte("changed"), 0600)
		}
		return nil
	})
	if !hit || err == nil {
		t.Fatal("damaged archive claimed successful at output", err)
	}
}

func TestRecoveryOriginalDoesNotOverwriteChangedCompletionAuthority(t *testing.T) {
	for _, change := range []string{"intent", "ack", "permissions"} {
		t.Run(change, func(t *testing.T) {
			r := originalFixture(t)
			r.fault = func(at string) error {
				if at != "original_acknowledged" {
					return nil
				}
				switch change {
				case "intent":
					var m recoveryOriginalManifest
					must(t, r.stage.ReadJSON(originalManifestFile, &m))
					m.ConfigHash = protocol.Hash("changed")
					return r.stage.WriteJSON(originalManifestFile, m, true)
				case "ack":
					_, err := r.db.Exec("DELETE FROM metadata WHERE key=?", originalArchiveKey)
					return err
				case "permissions":
					return os.Chmod(filepath.Join(r.stage.File().Name(), originalManifestFile), 0644)
				}
				return nil
			}
			if _, err := r.preserveOriginal(context.Background()); err == nil {
				t.Fatal("changed authority overwritten")
			}
			var m recoveryOriginalManifest
			must(t, r.stage.ReadJSON(originalManifestFile, &m))
			if m.State != "copying" {
				t.Fatal("published damaged completion")
			}
			if change == "intent" && m.ConfigHash != protocol.Hash("changed") {
				t.Fatal("replaced conflicting intent")
			}
		})
	}
}

func TestRecoveryOriginalRevalidatesSourceBeforeSQLCommit(t *testing.T) {
	r := originalFixture(t)
	r.fault = func(at string) error {
		if at != "reconciliation_transaction" {
			return nil
		}
		db, err := snapshotDB(r.dir, "state.db", false)
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = db.Exec("INSERT INTO metadata VALUES('changed-before-commit','changed')")
		return err
	}
	if _, err := r.preserveOriginal(context.Background()); err == nil {
		t.Fatal("source drift committed")
	}
	var count int
	must(t, r.db.QueryRow("SELECT count(*) FROM metadata WHERE key=?", originalArchiveKey).Scan(&count))
	if count != 0 {
		t.Fatal("source drift acknowledged")
	}
}
