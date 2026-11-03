package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestRecoveryFenceAbsentProducerAndLaterReconciliation(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	before := readOwner(t, &Store{dir: f.r.dir})
	fence, err := f.r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	if !fence.ReconciliationPending || fence.DeliveryEnabled || fence.Revision != before.Revision+1 || fence.PreviousHistory != before.History {
		t.Fatal(fence)
	}
	s := &Store{dir: f.r.dir, identity: f.r.journal.After, config: f.r.config}
	owner, err := s.readOwnership(producer)
	must(t, err)
	if owner.Active != "" || owner.RecoveryPending == nil {
		t.Fatal("absent producer not fenced", owner)
	}
	for _, state := range owner.Generations {
		if state != "revoked" {
			t.Fatal(owner)
		}
	}
	if err = s.Stage(producer, "new-generation"); err == nil {
		t.Fatal("absent producer staged")
	}
	if err = s.Activate(producer, "generation-one"); err == nil {
		t.Fatal("old generation activated")
	}
	if _, err = f.r.PrepareRecoveryOwnership(ctx, producer); !errors.Is(err, protocol.ErrRecoveryIncomplete) {
		t.Fatal(err)
	}
	var count int
	must(t, f.r.db.QueryRow("SELECT count(*) FROM reconciliation_producers").Scan(&count))
	if count != 0 {
		t.Fatal("fence fabricated reconciliation", count)
	}
	must(t, f.r.Close())
	r, err := openRecoveryReconciler(ctx, f.path, configuration(), nil)
	must(t, err)
	defer r.Close()
	r.now = func() time.Time { return f.now }
	f.r = r
	again, err := r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	if again != fence {
		t.Fatal("fence retry changed history")
	}
	ch := beginReconciliation(t, f)
	must(t, r.Import(ctx, ch, f.records))
	summary, err := r.Finish(ctx, ch, snapshot(f.now, 2, 1))
	must(t, err)
	proof, err := r.PrepareRecoveryOwnership(ctx, producer)
	must(t, err)
	if proof.SummaryHash != protocol.Hash(summary) || proof.PreviousHistory != fence.History || proof.Revision != fence.Revision+1 || proof.DeliveryEnabled {
		t.Fatal(proof)
	}
	must(t, proof.Validate())
	s.dir = r.dir
	owner, err = s.readOwnership(producer)
	must(t, err)
	if owner.RecoveryPending != nil {
		t.Fatal("completed reconciliation still pending")
	}
	must(t, s.verifyOwnershipPrefix(owner))
	if _, err = r.fenceRecoveryOwnership(ctx, producer); err == nil {
		t.Fatal("refenced reconciled producer")
	}
	// The ordinary generation path must recognize the schema-3 recovery proof.
	generation, err := r.PrepareRecoveryGeneration(ctx, producer)
	must(t, err)
	if generation.Ownership != proof {
		t.Fatal(generation)
	}
	againProof, err := r.PrepareRecoveryOwnership(ctx, producer)
	must(t, err)
	if againProof != proof {
		t.Fatal("staging changed recovery proof")
	}
	must(t, r.Close())
	if normal, err := Open(f.path, configuration(), false); err == nil {
		normal.Close()
		t.Fatal("fencing enabled ordinary service")
	}
}

func TestRecoveryFenceRejectsDamagedAuthority(t *testing.T) {
	for _, mode := range []string{"owner", "journal", "history", "tail", "lock", "busy", "binding", "marker", "rollback", "identity", "producer"} {
		t.Run(mode, func(t *testing.T) {
			f := newReconcileFixture(t)
			before := readOwner(t, &Store{dir: f.r.dir})
			var original ownershipIntent
			must(t, f.r.dir.ReadJSON(ownershipName(producer)+".journal.json", &original))
			ctx := context.Background()
			_, err := f.r.fenceRecoveryOwnership(ctx, producer)
			must(t, err)
			p := producer
			switch mode {
			case "owner":
				must(t, os.Remove(filepath.Join(f.path, ownershipName(p)+".json")))
			case "journal":
				must(t, os.Remove(filepath.Join(f.path, ownershipName(p)+".journal.json")))
			case "history":
				must(t, os.Remove(filepath.Join(f.path, ownershipHistoryPath(p, 1))))
			case "tail":
				must(t, f.r.dir.WriteJSON(ownershipHistoryPath(p, before.Revision+3), original.Transition, false))
			case "lock":
				must(t, os.Remove(filepath.Join(f.path, ownershipName(p)+".lock")))
			case "busy":
				lease, err := f.r.dir.Lock(ownershipName(p)+".lock", true, false)
				must(t, err)
				defer lease.Close()
			case "binding":
				var intent ownershipIntent
				must(t, f.r.dir.ReadJSON(ownershipName(p)+".journal.json", &intent))
				intent.Transition.Recovery.SummaryHash = protocol.Hash("invented summary")
				must(t, f.r.dir.WriteJSON(ownershipName(p)+".journal.json", intent, true))
			case "marker":
				owner := readOwner(t, &Store{dir: f.r.dir})
				owner.RecoveryPending = nil
				must(t, f.r.dir.WriteJSON(ownershipName(p)+".json", owner, true))
			case "rollback":
				must(t, f.r.dir.WriteJSON(ownershipName(p)+".json", before, true))
				must(t, f.r.dir.WriteJSON(ownershipName(p)+".journal.json", original, true))
			case "identity":
				must(t, f.r.dir.WriteJSON("identity.json", f.r.journal.Before, true))
			case "producer":
				p.Epoch = "different"
			}
			if _, err = f.r.fenceRecoveryOwnership(ctx, p); err == nil {
				t.Fatal("damaged authority accepted")
			}
		})
	}
}

func TestRecoveryFenceResumesBothTransitions(t *testing.T) {
	for _, complete := range []bool{false, true} {
		for _, stage := range []string{"intent", "history", "owner"} {
			name := "fence/" + stage
			if complete {
				name = "reconcile/" + stage
			}
			t.Run(name, func(t *testing.T) {
				f := newReconcileFixture(t)
				ctx := context.Background()
				operation := f.r.fenceRecoveryOwnership
				prefix := "recovery_fence_"
				if complete {
					_, err := operation(ctx, producer)
					must(t, err)
					ch := beginReconciliation(t, f)
					must(t, f.r.Import(ctx, ch, f.records))
					_, err = f.r.Finish(ctx, ch, snapshot(f.now, 2, 1))
					must(t, err)
					prefix = "recovery_ownership_"
				}
				before := readOwner(t, &Store{dir: f.r.dir})
				f.r.fault = func(at string) error {
					if at == prefix+stage {
						return errors.New("lost acknowledgement")
					}
					return nil
				}
				var err error
				if complete {
					_, err = f.r.PrepareRecoveryOwnership(ctx, producer)
				} else {
					_, err = operation(ctx, producer)
				}
				if err == nil {
					t.Fatal("fault missing")
				}
				must(t, f.r.Close())
				r, err := openRecoveryReconciler(ctx, f.path, configuration(), nil)
				must(t, err)
				defer r.Close()
				if complete {
					proof, err := r.PrepareRecoveryOwnership(ctx, producer)
					must(t, err)
					if proof.Revision != before.Revision+1 {
						t.Fatal(proof)
					}
					again, err := r.PrepareRecoveryOwnership(ctx, producer)
					must(t, err)
					if again != proof {
						t.Fatal("replay changed proof")
					}
				} else {
					fence, err := r.fenceRecoveryOwnership(ctx, producer)
					must(t, err)
					if fence.Revision != before.Revision+1 {
						t.Fatal(fence)
					}
					again, err := r.fenceRecoveryOwnership(ctx, producer)
					must(t, err)
					if again != fence {
						t.Fatal("replay changed fence")
					}
				}
			})
		}
	}
}

func TestRecoveryFenceLegacyOwnershipEncoding(t *testing.T) {
	const legacy = `{"identity":{"worker":"worker","recovery":"epoch"},"producer":{"id":"backup","epoch":"epoch-one"},"active":"","generations":{},"revision":0,"history":""}`
	var owner ownership
	must(t, json.Unmarshal([]byte(legacy), &owner))
	data, err := json.Marshal(owner)
	must(t, err)
	if string(data) != legacy {
		t.Fatal("old ownership hash changed", string(data))
	}
}

func TestRecoveryFenceRejectsReplacedProducerLease(t *testing.T) {
	for _, point := range []string{"intent", "history", "owner"} {
		t.Run(point, func(t *testing.T) {
			f := newReconcileFixture(t)
			lockPath := filepath.Join(f.path, ownershipName(producer)+".lock")
			f.r.fault = func(at string) error {
				if at == "recovery_fence_"+point {
					must(t, os.Rename(lockPath, lockPath+".held"))
					replacement, err := f.r.dir.Lock(ownershipName(producer)+".lock", true, true)
					must(t, err)
					replacement.Close()
				}
				return nil
			}
			if _, err := f.r.fenceRecoveryOwnership(context.Background(), producer); err == nil {
				t.Fatal("replaced lease allowed fence acknowledgement")
			}
			must(t, os.Remove(lockPath))
			must(t, os.Rename(lockPath+".held", lockPath))
			f.r.fault = nil
			proof, err := f.r.fenceRecoveryOwnership(context.Background(), producer)
			must(t, err)
			if !proof.ReconciliationPending || proof.DeliveryEnabled {
				t.Fatal(proof)
			}
		})
	}
}

func TestRecoveryFenceDoesNotBlockAnotherProducer(t *testing.T) {
	ctx := context.Background()
	c := configuration()
	absent := c.Producers[0]
	absent.Producer = protocol.Producer{ID: "uptime", Epoch: "epoch-one"}
	c.Producers = append(c.Producers, absent)
	path := t.TempDir()
	must(t, os.Chmod(path, 0700))
	s, err := Open(path, c, true)
	must(t, err)
	defer s.Close()
	now := time.Now().UTC()
	for _, p := range []protocol.Producer{producer, absent.Producer} {
		must(t, s.Stage(p, "generation-one"))
		must(t, s.Activate(p, "generation-one"))
		status := snapshot(now, 1, 1)
		status.Producer = p
		must(t, s.Replace(ctx, status))
	}
	artifact := filepath.Join(t.TempDir(), "snapshot")
	_, err = s.Snapshot(ctx, artifact)
	must(t, err)
	before := s.Identity()
	must(t, s.Close())
	_, err = prepareSnapshotRecovery(ctx, path, artifact, c, nil)
	must(t, err)
	r, err := openRecoveryReconciler(ctx, path, c, nil)
	must(t, err)
	defer r.Close()
	fence, err := r.fenceRecoveryOwnership(ctx, absent.Producer)
	must(t, err)
	ch, err := r.Challenge(ctx, producer)
	must(t, err)
	must(t, r.Begin(ctx, ch, protocol.RecoveryManifest{ImportedReceipts: protocol.Cursor{Identity: before, Producer: producer}}))
	now = time.Now().UTC()
	_, err = r.Finish(ctx, ch, snapshot(now, 2, 1))
	must(t, err)
	_, err = r.PrepareRecoveryOwnership(ctx, producer)
	must(t, err)
	g, err := r.PrepareRecoveryGeneration(ctx, producer)
	must(t, err)
	// Scoped runtime exercises independent producers; it does not publish the
	// quarantine or bypass production activation in the actual installation.
	runtime := &Store{dir: r.dir, db: r.db, identity: r.journal.After, config: c, session: "fixture", now: func() time.Time { return now }}
	must(t, runtime.prepareOwnership(false, false))
	must(t, runtime.Activate(producer, g.Generation))
	must(t, runtime.Replace(ctx, snapshot(now, 2, 1)))
	m := report(now, "new-event", 1, 1, 1)
	m.Generation = g.Generation
	_, err = runtime.Enqueue(ctx, m)
	must(t, err)
	_, granted, err := runtime.Grant(ctx, producer, m.ID)
	must(t, err)
	if !granted {
		t.Fatal("absent producer blocked reconciled producer")
	}
	if err = runtime.Stage(absent.Producer, "bypass"); err == nil {
		t.Fatal("absent producer gained authority")
	}
	status, err := runtime.Health(ctx)
	must(t, err)
	if status.Condition == "ok" {
		t.Fatal("absent required producer appeared healthy")
	}
	again, err := r.fenceRecoveryOwnership(ctx, absent.Producer)
	must(t, err)
	if again != fence {
		t.Fatal("another producer changed absent producer's fence")
	}
}

func TestRecoveryFencePreservesPendingIntentAndRepeatedEpochs(t *testing.T) {
	f := newReconcileFixture(t)
	ctx := context.Background()
	old := &Store{dir: f.r.dir, identity: f.r.journal.Before, config: f.r.config}
	old.ownershipFault = func(stage string) error {
		if stage == "intent" {
			return errors.New("lost stage acknowledgement")
		}
		return nil
	}
	if err := old.Stage(producer, "interrupted"); err == nil {
		t.Fatal("fault missing")
	}
	fence, err := f.r.fenceRecoveryOwnership(ctx, producer)
	must(t, err)
	s := &Store{dir: f.r.dir, identity: f.r.journal.After, config: f.r.config}
	current, err := s.readOwnership(producer)
	must(t, err)
	if current.Generations["interrupted"] != "revoked" {
		t.Fatal(current)
	}
	// A later disaster must preserve the original pending fence as history,
	// while the new fence binds exactly to the immediately preceding epoch.
	b := f.r.journal.binding()
	b.Before = b.After
	b.After.Recovery = "later-recovery"
	b.Operation, err = safefs.NewID("n")
	must(t, err)
	event := ownershipTransition{Schema: 3, Identity: current.Identity, Producer: producer,
		Revision: current.Revision + 1, Previous: current.History, BeforeHash: protocol.Hash(current),
		Operation: "recovery-fence", Recovery: &ownershipRecovery{Binding: b}}
	s.identity = b.After
	must(t, s.commitOwnershipEvent(&current, event))
	must(t, s.verifyOwnershipPrefix(current))
	if current.RecoveryPending == nil || *current.RecoveryPending != b || current.Revision != fence.Revision+1 {
		t.Fatal(current)
	}
	if err = s.Stage(producer, "bypass"); err == nil {
		t.Fatal("repeated recovery removed fence")
	}
}

func TestRecoveryFenceSIGKILL(t *testing.T) {
	for _, mode := range []string{"fence", "reconcile"} {
		for _, point := range []string{"intent", "history", "owner"} {
			t.Run(mode+"/"+point, func(t *testing.T) {
				f, _ := dispositionFixture(t, "retry")
				stage, fenceMode := "recovery_fence_"+point, "1"
				if mode == "reconcile" {
					_, err := f.r.fenceRecoveryOwnership(context.Background(), producer)
					must(t, err)
					stage, fenceMode = "recovery_ownership_"+point, "0"
				}
				before := readOwner(t, &Store{dir: f.r.dir})
				must(t, f.r.Close())
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRecoveryOwnershipCrashChild$")
				cmd.Env = append(os.Environ(), "RECOVERY_OWNERSHIP_PATH="+f.path, "RECOVERY_OWNERSHIP_STAGE="+stage, "RECOVERY_OWNERSHIP_FENCE="+fenceMode)
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
				scanner := bufio.NewScanner(pipe)
				if !scanner.Scan() || scanner.Text() != "READY" {
					cmd.Process.Kill()
					cmd.Wait()
					t.Fatal("fault missing", stderr.String())
				}
				must(t, cmd.Process.Kill())
				if cmd.Wait() == nil {
					t.Fatal("expected SIGKILL")
				}
				r, err := openRecoveryReconciler(ctx, f.path, configuration(), nil)
				must(t, err)
				defer r.Close()
				if mode == "fence" {
					proof, err := r.fenceRecoveryOwnership(ctx, producer)
					must(t, err)
					again, err := r.fenceRecoveryOwnership(ctx, producer)
					must(t, err)
					if proof != again || proof.Revision != before.Revision+1 || proof.DeliveryEnabled {
						t.Fatal(proof, again)
					}
				} else {
					proof, err := r.PrepareRecoveryOwnership(ctx, producer)
					must(t, err)
					again, err := r.PrepareRecoveryOwnership(ctx, producer)
					must(t, err)
					if proof != again || proof.Revision != before.Revision+1 || proof.DeliveryEnabled {
						t.Fatal(proof, again)
					}
				}
			})
		}
	}
}
