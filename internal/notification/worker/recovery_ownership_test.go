package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestRecoveryOwnershipRevokesOldGenerationsAndPreservesEvidence(t *testing.T) {
	f, decision := dispositionFixture(t, "retry")
	ctx := context.Background()
	_, err := f.r.DecideRecoveryMessage(ctx, decision)
	must(t, err)
	old := &Store{dir: f.r.dir, identity: f.r.journal.Before, config: f.r.config}
	must(t, old.Stage(producer, "still-staged"))
	must(t, old.Stage(producer, "already-revoked"))
	o := readOwner(t, old)
	must(t, old.commitOwnership(&o, "drain", "already-revoked"))
	must(t, old.commitOwnership(&o, "revoke", "already-revoked"))
	before := readOwner(t, old)
	history := make(map[uint64][]byte)
	for n := uint64(1); n <= before.Revision; n++ {
		history[n], err = os.ReadFile(filepath.Join(f.path, ownershipHistoryPath(producer, n)))
		must(t, err)
	}
	result, err := f.r.PrepareRecoveryOwnership(ctx, producer)
	must(t, err)
	if result.After != f.r.journal.After || result.Before != f.r.journal.Before ||
		result.PreviousRevision != before.Revision || result.Revision != before.Revision+1 ||
		result.PreviousHistory != before.History || result.SummaryHash != decision.SummaryHash || result.DeliveryEnabled {
		t.Fatal(result)
	}
	current := &Store{dir: f.r.dir, identity: f.r.journal.After, config: f.r.config}
	after, err := current.readOwnership(producer)
	must(t, err)
	if after.Active != "" || len(after.Generations) != len(before.Generations) {
		t.Fatal(after)
	}
	for g := range before.Generations {
		if after.Generations[g] != "revoked" {
			t.Fatal("generation revived", g)
		}
		if err = current.Activate(producer, g); err == nil {
			t.Fatal("old generation activated", g)
		}
		if err = current.Stage(producer, g); err == nil {
			t.Fatal("old generation restaged", g)
		}
	}
	if _, err = old.readOwnership(producer); err == nil {
		t.Fatal("old epoch read new ownership")
	}
	for n, original := range history {
		actual, err := os.ReadFile(filepath.Join(f.path, ownershipHistoryPath(producer, n)))
		must(t, err)
		if !bytes.Equal(actual, original) {
			t.Fatal("ownership history rewritten", n)
		}
	}
	must(t, current.verifyOwnershipPrefix(after))
	var state string
	var receipts int
	must(t, f.r.db.QueryRow("SELECT state FROM messages WHERE id=?", decision.MessageID).Scan(&state))
	must(t, f.r.db.QueryRow("SELECT count(*) FROM receipts").Scan(&receipts))
	if state != "ambiguous" || receipts != 0 {
		t.Fatal("migration changed delivery", state, receipts)
	}
	record, err := f.r.RecoveryMessageDisposition(ctx, producer, decision.MessageID)
	must(t, err)
	if record.Decision != decision || record.DeliveryEnabled {
		t.Fatal(record)
	}
	must(t, f.r.Close())
	session, err := OpenRecoveryOwnershipSession(ctx, f.path, configuration())
	must(t, err)
	defer session.Close()
	again, err := session.PrepareRecoveryOwnership(ctx, producer)
	must(t, err)
	if protocol.Hash(again) != protocol.Hash(result) {
		t.Fatal("retry changed migration")
	}
	if normal, err := Open(f.path, configuration(), false); err == nil {
		normal.Close()
		t.Fatal("migration enabled service")
	}
}

func TestRecoveryOwnershipCompletesOldIntentBeforeRevocation(t *testing.T) {
	f, _ := dispositionFixture(t, "cancel")
	old := &Store{dir: f.r.dir, identity: f.r.journal.Before, config: f.r.config}
	before := readOwner(t, old)
	old.ownershipFault = func(stage string) error {
		if stage == "intent" {
			return errors.New("old lost response")
		}
		return nil
	}
	if err := old.Stage(producer, "pending-generation"); err == nil {
		t.Fatal("fault missing")
	}
	result, err := f.r.PrepareRecoveryOwnership(context.Background(), producer)
	must(t, err)
	if result.Revision != before.Revision+2 {
		t.Fatal("pending intent not preserved", result)
	}
	current := &Store{dir: f.r.dir, identity: f.r.journal.After, config: f.r.config}
	after, err := current.readOwnership(producer)
	must(t, err)
	if after.Generations["pending-generation"] != "revoked" {
		t.Fatal(after)
	}
	must(t, current.verifyOwnershipPrefix(after))
}

func TestRecoveryOwnershipRejectsMissingOrChangedAuthority(t *testing.T) {
	for _, mode := range []string{"summary", "summary_changed", "history", "tail", "owner", "lock", "busy", "binding", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := dispositionFixture(t, "retry")
			ctx := context.Background()
			old := &Store{dir: f.r.dir, identity: f.r.journal.Before, config: f.r.config}
			before := readOwner(t, old)
			var original ownershipIntent
			must(t, f.r.dir.ReadJSON(ownershipName(producer)+".journal.json", &original))
			if mode == "binding" || mode == "rollback" || mode == "summary_changed" {
				_, err := f.r.PrepareRecoveryOwnership(ctx, producer)
				must(t, err)
			}
			switch mode {
			case "summary":
				_, err := f.r.db.Exec("UPDATE reconciliation_producers SET completion=''")
				must(t, err)
			case "summary_changed":
				_, err := f.r.db.Exec("UPDATE reconciliation_producers SET completion=json_set(completion,'$.Ambiguous',0)")
				must(t, err)
			case "history":
				must(t, os.Remove(filepath.Join(f.path, ownershipHistoryPath(producer, 1))))
			case "tail":
				must(t, f.r.dir.WriteJSON(ownershipHistoryPath(producer, before.Revision+2), original.Transition, false))
			case "owner":
				changed := before
				changed.Identity.Recovery = "foreign"
				must(t, f.r.dir.WriteJSON(ownershipName(producer)+".json", changed, true))
			case "lock":
				must(t, os.Remove(filepath.Join(f.path, ownershipName(producer)+".lock")))
			case "busy":
				lease, err := f.r.dir.Lock(ownershipName(producer)+".lock", true, false)
				must(t, err)
				defer lease.Close()
			case "binding":
				var intent ownershipIntent
				must(t, f.r.dir.ReadJSON(ownershipName(producer)+".journal.json", &intent))
				intent.Transition.Recovery.SummaryHash = protocol.Hash("other summary")
				must(t, f.r.dir.WriteJSON(ownershipName(producer)+".journal.json", intent, true))
			case "rollback":
				must(t, f.r.dir.WriteJSON(ownershipName(producer)+".json", before, true))
				must(t, f.r.dir.WriteJSON(ownershipName(producer)+".journal.json", original, true))
			}
			if _, err := f.r.PrepareRecoveryOwnership(ctx, producer); err == nil {
				t.Fatal("damaged authority migrated")
			}
		})
	}
}

func TestRecoveryOwnershipCrashChild(t *testing.T) {
	path := os.Getenv("RECOVERY_OWNERSHIP_PATH")
	if path == "" {
		t.Skip("subprocess helper")
	}
	s, err := OpenRecoveryOwnershipSession(context.Background(), path, configuration())
	must(t, err)
	defer s.Close()
	r := s.(*recoveryReconciler)
	r.fault = func(stage string) error {
		if stage == os.Getenv("RECOVERY_OWNERSHIP_STAGE") {
			fmt.Println("READY")
			select {}
		}
		return nil
	}
	if os.Getenv("RECOVERY_OWNERSHIP_FENCE") == "1" {
		_, err = r.fenceRecoveryOwnership(context.Background(), producer)
	} else {
		_, err = s.PrepareRecoveryOwnership(context.Background(), producer)
	}
	must(t, err)
	t.Fatal("fault missing")
}

func TestRecoveryOwnershipRejectsReplacedProducerLease(t *testing.T) {
	for _, stage := range []string{"intent", "history", "owner"} {
		t.Run(stage, func(t *testing.T) {
			f, _ := dispositionFixture(t, "retry")
			lockPath := filepath.Join(f.path, ownershipName(producer)+".lock")
			f.r.fault = func(boundary string) error {
				if boundary == "recovery_ownership_"+stage {
					must(t, os.Rename(lockPath, lockPath+".held"))
					replacement, err := f.r.dir.Lock(ownershipName(producer)+".lock", true, true)
					must(t, err)
					replacement.Close()
				}
				return nil
			}
			if _, err := f.r.PrepareRecoveryOwnership(context.Background(), producer); err == nil {
				t.Fatal("replaced producer lease accepted")
			}
			// Restore the original inode and resume the exact published intent.
			must(t, os.Remove(lockPath))
			must(t, os.Rename(lockPath+".held", lockPath))
			f.r.fault = nil
			result, err := f.r.PrepareRecoveryOwnership(context.Background(), producer)
			must(t, err)
			if result.DeliveryEnabled {
				t.Fatal("migration enabled delivery")
			}
		})
	}
}
func TestRecoveryOwnershipSIGKILL(t *testing.T) {
	for _, stage := range []string{"recovery_ownership_intent", "recovery_ownership_history", "recovery_ownership_owner"} {
		t.Run(stage, func(t *testing.T) {
			f, _ := dispositionFixture(t, "retry")
			before := readOwner(t, &Store{dir: f.r.dir})
			must(t, f.r.Close())
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryOwnershipCrashChild$")
			cmd.Env = append(os.Environ(), "RECOVERY_OWNERSHIP_PATH="+f.path, "RECOVERY_OWNERSHIP_STAGE="+stage)
			pipe, err := cmd.StdoutPipe()
			must(t, err)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			must(t, cmd.Start())
			t.Cleanup(func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			})
			scan := bufio.NewScanner(pipe)
			if !scan.Scan() || scan.Text() != "READY" {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatal("fault missing", stderr.String())
			}
			must(t, cmd.Process.Kill())
			if cmd.Wait() == nil {
				t.Fatal("expected SIGKILL")
			}
			session, err := OpenRecoveryOwnershipSession(ctx, f.path, configuration())
			must(t, err)
			defer session.Close()
			result, err := session.PrepareRecoveryOwnership(ctx, producer)
			must(t, err)
			again, err := session.PrepareRecoveryOwnership(ctx, producer)
			must(t, err)
			if result.DeliveryEnabled || result.Revision != before.Revision+1 || protocol.Hash(result) != protocol.Hash(again) {
				t.Fatal(result, again)
			}
		})
	}
}

func TestOwnershipRecoveryPrefixAcrossEpochsAndLegacyHash(t *testing.T) {
	f, _ := dispositionFixture(t, "retry")
	_, err := f.r.PrepareRecoveryOwnership(context.Background(), producer)
	must(t, err)
	s := &Store{dir: f.r.dir, identity: f.r.journal.After, config: f.r.config}
	must(t, s.Stage(producer, "next-generation"))
	must(t, s.Activate(producer, "next-generation"))
	before, err := s.readOwnership(producer)
	must(t, err)
	b := f.r.journal.binding()
	b.Before = before.Identity
	b.After = before.Identity
	b.After.Recovery = "second-recovery"
	b.Operation, err = safefs.NewID("n")
	must(t, err)
	event := ownershipTransition{Schema: 2, Identity: before.Identity, Producer: producer, Revision: before.Revision + 1,
		Previous: before.History, BeforeHash: protocol.Hash(before), Operation: "recover",
		Recovery: &ownershipRecovery{Binding: b, SummaryHash: protocol.Hash("second summary")}}
	s.identity = b.After
	must(t, s.commitOwnershipEvent(&before, event))
	must(t, s.verifyOwnershipPrefix(before))
	if before.Generations["next-generation"] != "revoked" {
		t.Fatal(before)
	}
	// The optional schema-2 field must not alter hashes of existing schema-1 files.
	const legacy = "{\"schema\":1,\"identity\":{\"worker\":\"worker\",\"recovery\":\"epoch\"},\"producer\":{\"id\":\"backup\",\"epoch\":\"epoch-one\"},\"revision\":1,\"previous\":\"\",\"before_hash\":\"hash\",\"operation\":\"initialize\"}"
	var decoded ownershipTransition
	must(t, json.Unmarshal([]byte(legacy), &decoded))
	raw, err := json.Marshal(decoded)
	must(t, err)
	if string(raw) != legacy {
		t.Fatal("schema-1 history encoding changed", string(raw))
	}
}
