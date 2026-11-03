package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func statusCheck(t *testing.T, v RecoveryStatus, scope, stage, state string) {
	t.Helper()
	for _, c := range v.Checks {
		if c.Scope == scope && c.Stage == stage {
			if c.State != state {
				t.Fatalf("%s/%s = %s, want %s", scope, stage, c.State, state)
			}
			return
		}
	}
	t.Fatalf("missing %s/%s", scope, stage)
}

func TestRecoveryStatusReadOnlyStableAndQuarantined(t *testing.T) {
	ctx := context.Background()
	path, from, c, m := quarantineFixture(t)
	prepared, err := QuarantineRecovery(ctx, path, from, c, m.ID, m.DatabaseHash)
	must(t, err)
	before, err := os.ReadFile(filepath.Join(path, "recovery", prepared.Operation, "reconciled.db"))
	must(t, err)
	first, err := ReviewRecovery(ctx, path, c, prepared.Operation)
	must(t, err)
	second, err := ReviewRecovery(ctx, path, c, prepared.Operation)
	must(t, err)
	if protocol.Hash(first) != protocol.Hash(second) || first.PublicationAllowed || first.DeliveryEnabled || first.ExecutionEnabled || first.PendingStages == 0 {
		t.Fatal(first, second)
	}
	if first.OriginalSourceHash != prepared.OriginalSourceHash || !safefs.DigestPattern.MatchString(first.ProjectionHash) {
		t.Fatal(first)
	}
	hash := first.ReviewHash
	first.ReviewHash = ""
	if protocol.Hash(first) != hash {
		t.Fatal("review digest does not cover result")
	}
	after, err := os.ReadFile(filepath.Join(path, "recovery", prepared.Operation, "reconciled.db"))
	must(t, err)
	if protocol.Hash(before) != protocol.Hash(after) {
		t.Fatal("status mutated database")
	}
	statusCheck(t, first, "worker", "original_source", "complete")
	statusCheck(t, first, "producer/"+producer.ID, "ownership", "pending")
	if s, e := Open(path, c, false); e == nil {
		s.Close()
		t.Fatal("review activated worker")
	}
}

func TestRecoveryStatusDoesNotInitializeMissingReconciliation(t *testing.T) {
	path, from, c, _ := quarantineFixture(t)
	j, err := prepareSnapshotRecovery(context.Background(), path, from, c, nil)
	must(t, err)
	if _, err = ReviewRecovery(context.Background(), path, c, j.Operation); err == nil {
		t.Fatal("missing projection initialized")
	}
	if _, err = os.Stat(filepath.Join(path, "recovery", j.Operation, "reconciliation.json")); !os.IsNotExist(err) {
		t.Fatal("status wrote manifest", err)
	}
}

func TestRecoveryStatusTracksOwnershipAndRetry(t *testing.T) {
	f, d, _ := generationFixture(t)
	ctx := context.Background()
	_, err := f.r.preserveOriginal(ctx)
	must(t, err)
	before, err := f.r.reviewRecovery(ctx)
	must(t, err)
	statusCheck(t, before, "producer/"+producer.ID, "ownership", "complete")
	statusCheck(t, before, "producer/"+producer.ID, "generation", "complete")
	statusCheck(t, before, "producer/"+producer.ID, "message_dispositions", "pending")
	_, err = f.r.BindRecoveryRetry(ctx, producer, d.MessageID)
	must(t, err)
	after, err := f.r.reviewRecovery(ctx)
	must(t, err)
	statusCheck(t, after, "producer/"+producer.ID, "message_dispositions", "complete")
	if before.ProjectionHash == after.ProjectionHash || before.EvidenceHash == after.EvidenceHash || before.ReviewHash == after.ReviewHash {
		t.Fatal("review ignored a committed retry binding")
	}
}

func TestRecoveryStatusAbsentProducerRemainsExplicitlyFenced(t *testing.T) {
	r := originalFixture(t)
	_, err := r.fenceRecoveryOwnership(context.Background(), producer)
	must(t, err)
	out, err := r.reviewRecovery(context.Background())
	must(t, err)
	statusCheck(t, out, "producer/"+producer.ID, "ownership", "retained_fenced")
	statusCheck(t, out, "producer/"+producer.ID, "delivery_reconciliation", "pending")
}

func TestRecoveryStatusProviderCursors(t *testing.T) {
	t.Run("signal", func(t *testing.T) {
		f, proof := signalCursorFixture(t)
		must(t, f.r.InstallSignalCursor(context.Background(), proof))
		out, err := f.r.reviewRecovery(context.Background())
		must(t, err)
		scope := "intake/signal/" + proof.Handoff.Target.Account
		statusCheck(t, out, scope, "cursor", "complete")
		statusCheck(t, out, scope, "activation_recheck", "pending")
		must(t, os.Remove(filepath.Join(f.r.stage.File().Name(), signalCursorFile(proof.Handoff.Target.Account))))
		if _, err = f.r.reviewRecovery(context.Background()); err == nil {
			t.Fatal("lost Signal cursor proof accepted")
		}
	})
	t.Run("telegram", func(t *testing.T) {
		f, r, _ := telegramCursorFixture(t)
		_, err := r.CommitTelegramRecoveryCursor(context.Background(), f.Account)
		must(t, err)
		out, err := r.reviewRecovery(context.Background())
		must(t, err)
		scope := "intake/telegram/" + f.Account
		statusCheck(t, out, scope, "cursor", "complete")
		statusCheck(t, out, scope, "activation_recheck", "pending")
		_, err = r.db.Exec("UPDATE telegram_cursors SET offset=1")
		must(t, err)
		if _, err = r.reviewRecovery(context.Background()); err == nil {
			t.Fatal("advanced Telegram cursor accepted")
		}
	})
}

func TestRecoveryStatusRejectsChangedEvidence(t *testing.T) {
	for _, kind := range []string{"wrong-operation", "worker-busy", "producer-busy", "candidate", "original", "manifest", "source-during-review", "candidate-during-review", "history-during-review", "file-during-review", "lease-during-review", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			r := originalFixture(t)
			_, err := r.preserveOriginal(context.Background())
			must(t, err)
			path, c, op := r.dir.File().Name(), r.config, r.journal.Operation
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "wrong-operation":
				op, _ = safefs.NewID("n")
			case "worker-busy":
				if _, err = ReviewRecovery(ctx, path, c, op); err == nil {
					t.Fatal("review bypassed worker lease")
				}
				return
			case "producer-busy":
				l, e := r.dir.Lock(ownershipName(producer)+".lock", true, false)
				must(t, e)
				defer l.Close()
			case "candidate":
				must(t, os.WriteFile(filepath.Join(r.stage.File().Name(), "candidate.db"), []byte("changed"), 0600))
			case "original":
				must(t, os.WriteFile(filepath.Join(r.stage.File().Name(), originalArchiveFile), []byte("changed"), 0600))
			case "manifest":
				must(t, r.stage.WriteJSON("reconciliation.json", map[string]string{}, true))
			case "cancelled":
				cancel()
			case "source-during-review", "candidate-during-review", "history-during-review", "file-during-review", "lease-during-review":
				r.fault = func(at string) error {
					if at != "recovery_status_observed" {
						return nil
					}
					switch kind {
					case "source-during-review":
						return os.WriteFile(filepath.Join(path, "state.db"), []byte("changed"), 0600)
					case "candidate-during-review":
						return os.WriteFile(filepath.Join(r.stage.File().Name(), "candidate.db"), []byte("changed"), 0600)
					case "history-during-review":
						return r.dir.WriteJSON(ownershipHistoryPath(producer, 1), map[string]string{"invalid": "history"}, true)
					case "file-during-review":
						return r.stage.WriteJSON("new-proof.json", map[string]string{"proof": "changed"}, false)
					default:
						lock := filepath.Join(path, "worker.lock")
						if e := os.Rename(lock, lock+"-old"); e != nil {
							return e
						}
						return os.WriteFile(lock, nil, 0600)
					}
				}
				if _, err = r.reviewRecovery(ctx); err == nil {
					t.Fatal("changed evidence returned review")
				}
				return
			}
			must(t, r.Close())
			if _, err = ReviewRecovery(ctx, path, c, op); err == nil {
				t.Fatal("invalid review accepted")
			}
		})
	}
}

func TestRecoveryProjectionHashTracksWALAndTypes(t *testing.T) {
	r := originalFixture(t)
	ctx := context.Background()
	_, err := r.db.Exec("CREATE TABLE review_values(v); INSERT INTO review_values VALUES(1),(1.0),('1'),(x'31')")
	must(t, err)
	digest := func() string {
		t.Helper()
		tx, e := r.db.BeginTx(ctx, nil)
		must(t, e)
		defer tx.Rollback()
		h, e := recoveryProjectionHash(ctx, tx, r.config.MaxDatabaseBytes)
		must(t, e)
		return h
	}
	first := digest()
	_, err = r.db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; INSERT INTO review_values VALUES(CAST(x'80' AS TEXT))")
	must(t, err)
	second := digest()
	_, err = r.db.Exec("UPDATE review_values SET v=CAST(x'81' AS TEXT) WHERE typeof(v)='text' AND hex(v)='80'")
	must(t, err)
	third := digest()
	if first == second || second == third {
		t.Fatal("hash ignored WAL-visible or invalid-UTF8 text changes")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	must(t, err)
	defer tx.Rollback()
	if _, err = recoveryProjectionHash(ctx, tx, 8); err == nil {
		t.Fatal("projection budget ignored")
	}
}

func TestRecoveryStatusPreservesCommandOutcomes(t *testing.T) {
	f, _ := commandInstallFixture(t, false)
	ctx := context.Background()
	before, err := f.r.reviewRecovery(ctx)
	must(t, err)
	statusCheck(t, before, "producer/"+producer.ID, "commands", "pending")
	installed, err := interaction.InstallCommandRecovery(ctx, f.ledger, f.r)
	must(t, err)
	var original string
	must(t, f.r.db.QueryRow("SELECT json_group_array(json_array(id,state,grant,revision,body)) FROM commands ORDER BY id").Scan(&original))
	after, err := f.r.reviewRecovery(ctx)
	must(t, err)
	statusCheck(t, after, "producer/"+producer.ID, "commands", "complete")
	if after.ReviewHash == before.ReviewHash || after.ExecutionEnabled {
		t.Fatal("installation review changed authority")
	}
	var current string
	must(t, f.r.db.QueryRow("SELECT json_group_array(json_array(id,state,grant,revision,body)) FROM commands ORDER BY id").Scan(&current))
	if original != current {
		t.Fatal("review mutated command outcomes")
	}
	replay, err := interaction.InstallCommandRecovery(ctx, f.ledger, f.r)
	must(t, err)
	if protocol.Hash(replay) != protocol.Hash(installed) {
		t.Fatal("review changed command handoff")
	}
}

func TestRecoveryStatusRejectsConcurrentProjectionCommit(t *testing.T) {
	r := originalFixture(t)
	_, err := r.db.Exec("PRAGMA journal_mode=WAL")
	must(t, err)
	peer, err := snapshotDB(r.stage, "reconciled.db", false)
	must(t, err)
	defer peer.Close()
	r.fault = func(at string) error {
		if at == "recovery_status_observed" {
			_, err := peer.Exec("INSERT INTO metadata VALUES('concurrent-review-change','changed')")
			return err
		}
		return nil
	}
	if _, err = r.reviewRecovery(context.Background()); err == nil {
		t.Fatal("concurrent projection commit ignored")
	}
}

func TestRecoveryStatusRetainsInheritedLEDs(t *testing.T) {
	r, path, c, api, adoption := epochFixture(t, true)
	ctx := context.Background()
	before := protocol.Hash(api.states)
	_, err := r.handoffLED(ctx, migrationSocket, adoption)
	must(t, err)
	_, err = r.preserveOriginal(ctx)
	must(t, err)
	out, err := r.reviewRecovery(ctx)
	must(t, err)
	statusCheck(t, out, "led", "ownership", "complete")
	statusCheck(t, out, "led", "epoch", "complete")
	statusCheck(t, out, "led", "inherited_faults", "retained_fenced")
	must(t, r.Close())
	reopened, err := ReviewRecovery(ctx, path, c, r.journal.Operation)
	must(t, err)
	if protocol.Hash(out) != protocol.Hash(reopened) {
		t.Fatal("read-only reopened review changed inherited ownership")
	}
	if before != protocol.Hash(api.states) {
		t.Fatal("status contacted LED daemon")
	}
}

func TestRecoveryStatusDoesNotHideDamagedCommandInstallation(t *testing.T) {
	for _, kind := range []string{"schema", "session", "file", "row", "grant"} {
		t.Run(kind, func(t *testing.T) {
			f, _ := commandInstallFixture(t, false)
			_, err := interaction.InstallCommandRecovery(context.Background(), f.ledger, f.r)
			must(t, err)
			switch kind {
			case "schema":
				_, err = f.r.db.Exec("DELETE FROM metadata WHERE key='command_reconciliation_schema'; DROP TABLE reconciliation_command_sessions; DROP TABLE reconciliation_command_records")
			case "session":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_command_sessions")
			case "file":
				err = os.Remove(filepath.Join(f.r.stage.File().Name(), commandInstallationFile(producer)))
			case "row":
				_, err = f.r.db.Exec("UPDATE reconciliation_command_installations SET hash='wrong'")
			case "grant":
				_, err = f.r.db.Exec("UPDATE commands SET grant='{}'")
			}
			must(t, err)
			if _, err = f.r.reviewRecovery(context.Background()); err == nil {
				t.Fatal("damaged installation reported as pending or complete")
			}
		})
	}
}

func TestRecoveryStatusRejectsLostDeliveryEvidence(t *testing.T) {
	for _, kind := range []string{"receipts", "record", "payload", "summary", "watermark"} {
		t.Run(kind, func(t *testing.T) {
			f, _, _ := generationFixture(t)
			var err error
			switch kind {
			case "receipts":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_receipts")
			case "record":
				_, err = f.r.db.Exec("DELETE FROM reconciliation_records WHERE seq=1")
			case "payload":
				_, err = f.r.db.Exec("DELETE FROM messages WHERE id='first'")
			case "summary":
				_, err = f.r.db.Exec("UPDATE reconciliation_producers SET completion=json_set(completion,'$.Manifest.HighestIssued',99)")
			case "watermark":
				_, err = f.r.db.Exec("UPDATE producers SET submitted=submitted+1")
			}
			must(t, err)
			if _, err = f.r.reviewRecovery(context.Background()); err == nil {
				t.Fatal("damaged delivery evidence accepted")
			}
		})
	}
}

func TestRecoveryStatusPendingCursorIntentsAreNotCompleted(t *testing.T) {
	t.Run("signal", func(t *testing.T) {
		f, proof := signalCursorFixture(t)
		f.r.fault = func(at string) error {
			if at == "signal_cursor_intent" {
				return errors.New("interrupted")
			}
			return nil
		}
		if err := f.r.InstallSignalCursor(context.Background(), proof); err == nil {
			t.Fatal("fault missed")
		}
		f.r.fault = nil
		out, err := f.r.reviewRecovery(context.Background())
		must(t, err)
		statusCheck(t, out, "intake/signal/"+proof.Handoff.Target.Account, "cursor", "pending")
		var count int
		must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_signal_cursors").Scan(&count))
		if count != 0 {
			t.Fatal("status completed cursor intent")
		}
	})
	t.Run("telegram", func(t *testing.T) {
		f, r, _ := telegramCursorFixture(t)
		r.fault = func(at string) error {
			if at == "telegram_cursor_intent" {
				return errors.New("interrupted")
			}
			return nil
		}
		if _, err := r.CommitTelegramRecoveryCursor(context.Background(), f.Account); err == nil {
			t.Fatal("fault missed")
		}
		r.fault = nil
		out, err := r.reviewRecovery(context.Background())
		must(t, err)
		statusCheck(t, out, "intake/telegram/"+f.Account, "cursor", "pending")
		var count int
		must(t, r.db.QueryRow("SELECT count(*) FROM reconciliation_telegram_cursors").Scan(&count))
		if count != 0 {
			t.Fatal("status completed cursor intent")
		}
	})
}

func TestRecoveryStatusPendingCommandIntentIsNotCompleted(t *testing.T) {
	f, proof := commandInstallFixture(t, false)
	f.r.fault = func(at string) error {
		if at == "command_install_intent" {
			return errors.New("interrupted")
		}
		return nil
	}
	if _, err := f.r.InstallCommandRecoveryOutcomes(context.Background(), proof); err == nil {
		t.Fatal("fault missed")
	}
	f.r.fault = nil
	out, err := f.r.reviewRecovery(context.Background())
	must(t, err)
	statusCheck(t, out, "producer/"+producer.ID, "commands", "pending")
	var count int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_command_installations").Scan(&count))
	if count != 0 {
		t.Fatal("status completed command intent")
	}
}

func TestRecoveryStatusHoldsAllProducerLeases(t *testing.T) {
	ctx := context.Background()
	c := configuration()
	other := c.Producers[0]
	other.Producer.ID = "aaa-other"
	c.Producers = append(c.Producers, other)
	path := t.TempDir()
	must(t, os.Chmod(path, 0700))
	store, err := Open(path, c, true)
	must(t, err)
	from := filepath.Join(t.TempDir(), "snapshot")
	manifest, err := store.Snapshot(ctx, from)
	must(t, err)
	must(t, store.Close())
	prepared, err := QuarantineRecovery(ctx, path, from, c, manifest.ID, manifest.DatabaseHash)
	must(t, err)
	r, err := openRecoveryStatus(ctx, path, c, prepared.Operation)
	must(t, err)
	defer r.Close()
	held := false
	r.fault = func(at string) error {
		if at != "recovery_status_observed" {
			return nil
		}
		held = true
		for _, p := range []protocol.Producer{producer, other.Producer} {
			if lock, e := r.dir.Lock(ownershipName(p)+".lock", true, false); e == nil {
				lock.Close()
				t.Fatal("producer lease released before review completed", p)
			}
		}
		return nil
	}
	_, err = r.reviewRecovery(ctx)
	must(t, err)
	if !held {
		t.Fatal("review boundary missed")
	}
	r.fault = nil
	busy, err := r.dir.Lock(ownershipName(producer)+".lock", true, false)
	must(t, err)
	if _, err = r.reviewRecovery(ctx); err == nil {
		t.Fatal("busy producer ignored")
	}
	busy.Close()
	released, err := r.dir.Lock(ownershipName(other.Producer)+".lock", true, false)
	must(t, err)
	released.Close()
}
