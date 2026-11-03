package worker

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func readOwner(t *testing.T, s *Store) ownership {
	t.Helper()
	var value ownership
	must(t, s.dir.ReadJSON(ownershipName(producer)+".json", &value))
	return value
}

func TestOwnershipJournalRetainsTransitionsAndRevokedGenerations(t *testing.T) {
	s, _, dir := fixture(t)
	ctx := context.Background()
	must(t, s.Stage(producer, "two"))
	must(t, s.Revoke(ctx, producer, "two"))
	must(t, s.Revoke(ctx, producer, "generation-one"))
	before := readOwner(t, s)
	must(t, s.Revoke(ctx, producer, "generation-one"))
	if !sameOwnership(before, readOwner(t, s)) {
		t.Fatal("idempotent revoke appended history")
	}
	if err := s.Activate(producer, "two"); err == nil {
		t.Fatal("revoked generation reactivated")
	}
	must(t, s.Stage(producer, "three"))
	must(t, s.Activate(producer, "three"))
	want := readOwner(t, s)
	must(t, s.verifyOwnershipPrefix(want))
	ops := []string{}
	for n := uint64(1); n <= want.Revision; n++ {
		var e ownershipTransition
		must(t, s.dir.ReadJSON(ownershipHistoryPath(producer, n), &e))
		ops = append(ops, e.Operation)
	}
	if strings.Join(ops, ",") != "initialize,stage,activate,stage,drain,revoke,drain,revoke,stage,activate" {
		t.Fatal(ops)
	}
	must(t, s.Close())
	r, err := Open(dir, configuration(), false)
	must(t, err)
	defer r.Close()
	if !sameOwnership(want, readOwner(t, r)) {
		t.Fatal("restart changed ownership")
	}
}

func TestOwnershipJournalPendingIntentFencesReadAndGrant(t *testing.T) {
	for _, boundary := range []string{"intent", "history"} {
		t.Run(boundary, func(t *testing.T) {
			s, now, path := fixture(t)
			ctx := context.Background()
			must(t, s.Replace(ctx, snapshot(*now, 1, 1)))
			m := report(*now, "queued", 1, 1, 1)
			_, err := s.Enqueue(ctx, m)
			must(t, err)
			s.ownershipFault = func(stage string) error {
				if stage == boundary {
					return errors.New("interrupted")
				}
				return nil
			}
			if err = s.Revoke(ctx, producer, m.Generation); err == nil {
				t.Fatal("fault was not exercised")
			}
			journal, err := os.ReadFile(filepath.Join(path, ownershipName(producer)+".journal.json"))
			must(t, err)
			if _, err = s.OwnershipStatus(producer, m.Generation); err == nil {
				t.Fatal("partial transition reported as settled")
			}
			if _, granted, err := s.Grant(ctx, producer, m.ID); err == nil || granted {
				t.Fatal("grant bypassed incomplete ownership", granted, err)
			}
			after, err := os.ReadFile(filepath.Join(path, ownershipName(producer)+".journal.json"))
			must(t, err)
			if !bytes.Equal(journal, after) {
				t.Fatal("observation changed lifecycle intent")
			}
			must(t, s.Close())
			r, err := Open(path, configuration(), false)
			must(t, err)
			defer r.Close()
			status, err := r.OwnershipStatus(producer, m.Generation)
			must(t, err)
			if status.State != "draining" || status.Active {
				t.Fatal(status)
			}
			if _, granted, err := r.Grant(ctx, producer, m.ID); err != nil || granted {
				t.Fatal("recovered drain granted", granted, err)
			}
			must(t, r.Revoke(ctx, producer, m.Generation))
		})
	}
}

func TestOwnershipJournalRejectsMissingOrChangedAuthority(t *testing.T) {
	for _, damage := range []string{"owner", "journal", "lock", "prefix", "changed-prefix", "changed-head", "old-owner", "old-pair", "old-pair-with-gap", "legacy-rollback"} {
		t.Run(damage, func(t *testing.T) {
			s, _, dir := fixture(t)
			original := readOwner(t, s)
			var oldIntent ownershipIntent
			must(t, s.dir.ReadJSON(ownershipName(producer)+".journal.json", &oldIntent))
			must(t, s.Revoke(context.Background(), producer, "generation-one"))
			switch damage {
			case "owner", "journal", "lock":
				suffix := map[string]string{"owner": ".json", "journal": ".journal.json", "lock": ".lock"}[damage]
				must(t, os.Remove(filepath.Join(dir, ownershipName(producer)+suffix)))
			case "prefix":
				must(t, os.Remove(filepath.Join(dir, ownershipHistoryPath(producer, 2))))
			case "changed-prefix", "changed-head":
				revision := uint64(2)
				if damage == "changed-head" {
					revision = readOwner(t, s).Revision
				}
				var e ownershipTransition
				must(t, s.dir.ReadJSON(ownershipHistoryPath(producer, revision), &e))
				e.Generation = "different"
				must(t, s.dir.WriteJSON(ownershipHistoryPath(producer, revision), e, true))
			case "old-owner":
				must(t, s.dir.WriteJSON(ownershipName(producer)+".json", original, true))
			case "old-pair", "old-pair-with-gap":
				must(t, s.dir.WriteJSON(ownershipName(producer)+".json", original, true))
				must(t, s.dir.WriteJSON(ownershipName(producer)+".journal.json", oldIntent, true))
				if damage == "old-pair-with-gap" {
					must(t, os.Remove(filepath.Join(dir, ownershipHistoryPath(producer, original.Revision+1))))
				} else if _, err := s.OwnershipStatus(producer, "generation-one"); err == nil {
					t.Fatal("live observation ignored later revocation history")
				}
			case "legacy-rollback":
				original.Revision, original.History = 0, ""
				must(t, s.dir.WriteJSON(ownershipName(producer)+".json", original, true))
				must(t, os.Remove(filepath.Join(dir, ownershipName(producer)+".journal.json")))
				downgradeLEDLayout(t, s)
				_, err := s.db.Exec("PRAGMA user_version=4")
				must(t, err)
			}
			must(t, s.Close())
			if r, err := Open(dir, configuration(), false); err == nil {
				r.Close()
				t.Fatal("damaged authority reopened")
			}
		})
	}
}

func TestOwnershipJournalCompletesLostOwnerPublication(t *testing.T) {
	s, _, dir := fixture(t)
	must(t, s.Stage(producer, "new"))
	before := readOwner(t, s)
	must(t, s.Revoke(context.Background(), producer, "new"))
	want := readOwner(t, s)
	var intent ownershipIntent
	must(t, s.dir.ReadJSON(ownershipName(producer)+".journal.json", &intent))
	// Only the exact predecessor of the latest intent is recoverable. Arbitrary
	// old owners (including before drain) are rejected by the previous test.
	if intent.Before.Revision <= before.Revision {
		t.Fatal("missing drain")
	}
	must(t, s.dir.WriteJSON(ownershipName(producer)+".json", intent.Before, true))
	must(t, s.Close())
	r, err := Open(dir, configuration(), false)
	must(t, err)
	defer r.Close()
	if !sameOwnership(want, readOwner(t, r)) {
		t.Fatal("lost publication was not completed")
	}
}

func TestOwnershipJournalAdoptsLegacyEvidenceWithoutInventingHistory(t *testing.T) {
	s, _, path := fixture(t)
	legacy := legacyOwnershipFixture(t, s, path)
	must(t, s.Close())
	r, err := Open(path, configuration(), false)
	must(t, err)
	defer r.Close()
	var baseline ownershipTransition
	must(t, r.dir.ReadJSON(ownershipHistoryPath(producer, 1), &baseline))
	if baseline.Operation != "adopt" || baseline.Baseline == nil || !sameOwnership(*baseline.Baseline, legacy) {
		t.Fatal("legacy history was fabricated", baseline)
	}
	status, err := r.OwnershipStatus(producer, "generation-one")
	must(t, err)
	if !status.Active {
		t.Fatal("migration changed delivery authority")
	}
	must(t, r.Stage(producer, "new"))
	must(t, r.verifyOwnershipPrefix(readOwner(t, r)))
}

func legacyOwnershipFixture(t *testing.T, s *Store, path string) ownership {
	t.Helper()
	legacy := readOwner(t, s)
	legacy.Revision, legacy.History = 0, ""
	must(t, s.dir.WriteJSON(ownershipName(producer)+".json", legacy, true))
	must(t, os.Remove(filepath.Join(path, ownershipName(producer)+".journal.json")))
	must(t, os.RemoveAll(filepath.Join(path, "ownership")))
	downgradeLEDLayout(t, s)
	_, err := s.db.Exec("PRAGMA user_version=4")
	must(t, err)
	return legacy
}

func TestOwnershipJournalRejectsForeignLegacyEpoch(t *testing.T) {
	s, _, path := fixture(t)
	legacy := legacyOwnershipFixture(t, s, path)
	legacy.Identity.Recovery = "foreign-epoch"
	must(t, s.dir.WriteJSON(ownershipName(producer)+".json", legacy, true))
	if err := s.prepareProducerOwnership(producer, false, true); err == nil {
		t.Fatal("foreign legacy ownership adopted")
	}
	if !sameOwnership(readOwner(t, s), legacy) {
		t.Fatal("foreign legacy evidence changed")
	}
	if _, err := os.Stat(filepath.Join(path, ownershipName(producer)+".journal.json")); !os.IsNotExist(err) {
		t.Fatal("foreign legacy journal created", err)
	}
}

// A real process is killed at each durable boundary; no deferred cleanup runs.
func TestOwnershipJournalCrashChild(t *testing.T) {
	path := os.Getenv("DEPLOY_OWNERSHIP_CRASH_STATE")
	if path == "" {
		t.Skip("subprocess fixture")
	}
	target := os.Getenv("DEPLOY_OWNERSHIP_CRASH_BOUNDARY")
	fault := func(stage string) error {
		if stage == target {
			fmt.Println("READY")
			for {
				time.Sleep(time.Second)
			}
		}
		return nil
	}
	operation := os.Getenv("DEPLOY_OWNERSHIP_CRASH_OPERATION")
	var openFault func(string) error
	if operation == "adopt" {
		openFault = fault
	}
	s, err := openWithOwnershipFault(path, configuration(), false, openFault)
	must(t, err)
	s.ownershipFault = fault
	if operation == "activate" {
		must(t, s.Activate(producer, "candidate"))
	} else if operation == "stage" {
		must(t, s.Stage(producer, "candidate"))
	} else {
		must(t, s.Revoke(context.Background(), producer, "generation-one"))
	}
	t.Fatal("boundary not reached")
}

func TestOwnershipJournalSIGKILL(t *testing.T) {
	for _, operation := range []string{"stage", "activate", "drain", "revoke", "adopt"} {
		for _, boundary := range []string{"intent", "history", "owner"} {
			t.Run(operation+"/"+boundary, func(t *testing.T) {
				s, _, path := fixture(t)
				if operation == "activate" {
					must(t, s.Revoke(context.Background(), producer, "generation-one"))
					must(t, s.Stage(producer, "candidate"))
				}
				if operation == "revoke" {
					must(t, s.ownership(producer, func(o *ownership) error { return s.commitOwnership(o, "drain", "generation-one") }))
				}
				if operation == "adopt" {
					legacyOwnershipFixture(t, s, path)
				}
				must(t, s.Close())
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOwnershipJournalCrashChild$")
				cmd.Env = append(os.Environ(), "DEPLOY_OWNERSHIP_CRASH_STATE="+path, "DEPLOY_OWNERSHIP_CRASH_BOUNDARY="+boundary, "DEPLOY_OWNERSHIP_CRASH_OPERATION="+operation)
				pipe, err := cmd.StdoutPipe()
				must(t, err)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				must(t, cmd.Start())
				defer func() {
					if cmd.ProcessState == nil {
						cmd.Process.Kill()
						cmd.Wait()
					}
				}()
				scanner := bufio.NewScanner(pipe)
				if !scanner.Scan() || scanner.Text() != "READY" {
					t.Fatalf("boundary not reached: %s %s", scanner.Text(), stderr.String())
				}
				if other, err := Open(path, configuration(), false); err == nil {
					other.Close()
					t.Fatal("live worker lease bypassed")
				}
				must(t, cmd.Process.Kill())
				if cmd.Wait() == nil {
					t.Fatal("expected SIGKILL")
				}
				r, err := Open(path, configuration(), false)
				must(t, err)
				defer r.Close()
				g, state := "generation-one", "draining"
				switch operation {
				case "activate":
					g, state = "candidate", "active"
				case "stage":
					g, state = "candidate", "staged"
				case "revoke":
					state = "revoked"
				case "adopt":
					state = "active"
				}
				status, err := r.OwnershipStatus(producer, g)
				must(t, err)
				if status.State != state || status.Active != (state == "active") {
					t.Fatal(status)
				}
				must(t, r.verifyOwnershipPrefix(readOwner(t, r)))
			})
		}
	}
}

func TestOwnershipObservationDoesNotReserveUnknownGeneration(t *testing.T) {
	s, _, _ := fixture(t)
	before := readOwner(t, s)
	status, err := s.OwnershipStatus(producer, "unknown")
	must(t, err)
	if status.State != "unknown" || status.Active {
		t.Fatal(status)
	}
	if !sameOwnership(before, readOwner(t, s)) {
		t.Fatal("read reserved generation")
	}
	// A future generation still has to enter via the journaled lifecycle.
	must(t, s.Stage(producer, "unknown"))
	if protocol.Hash(before) == protocol.Hash(readOwner(t, s)) {
		t.Fatal("stage did not change ownership")
	}
}

func TestOwnershipJournalReservesSpaceBeforePublishingIntent(t *testing.T) {
	s, _, path := fixture(t)
	before := readOwner(t, s)
	journal, err := os.ReadFile(filepath.Join(path, ownershipName(producer)+".journal.json"))
	must(t, err)
	s.config.MinFreeBytes = math.MaxInt64 // Impossible reserve, including overflow.
	if err = s.Stage(producer, "candidate"); err == nil {
		t.Fatal("stage ignored space reserve")
	}
	after, err := os.ReadFile(filepath.Join(path, ownershipName(producer)+".journal.json"))
	must(t, err)
	if !bytes.Equal(journal, after) || !sameOwnership(before, readOwner(t, s)) {
		t.Fatal("failed preflight changed authority")
	}
}

func TestOwnershipJournalInterruptedProducerDoesNotChangeAnother(t *testing.T) {
	c := configuration()
	other := c.Producers[0]
	other.Producer = protocol.Producer{ID: "uptime", Epoch: "epoch-one"}
	c.Producers = append(c.Producers, other)
	path := t.TempDir()
	must(t, os.Chmod(path, 0700))
	s, err := Open(path, c, true)
	must(t, err)
	defer s.Close()
	for _, p := range []protocol.Producer{producer, other.Producer} {
		must(t, s.Stage(p, "generation-one"))
		must(t, s.Activate(p, "generation-one"))
	}
	s.ownershipFault = func(stage string) error {
		if stage == "intent" {
			return errors.New("interrupted")
		}
		return nil
	}
	if err = s.Revoke(context.Background(), producer, "generation-one"); err == nil {
		t.Fatal("missing interruption")
	}
	status, err := s.OwnershipStatus(other.Producer, "generation-one")
	must(t, err)
	if !status.Active {
		t.Fatal("unrelated producer lost authority", status)
	}
}

func TestOwnershipJournalMissingInstanceLeaseIsNotRecreated(t *testing.T) {
	s, _, path := fixture(t)
	lock := filepath.Join(path, "worker.lock")
	must(t, os.Remove(lock))
	if r, err := Open(path, configuration(), false); err == nil {
		r.Close()
		t.Fatal("missing live instance lock recreated")
	}
	if r, err := Open(path, configuration(), true); err == nil {
		r.Close()
		t.Fatal("initialization recreated existing worker's lock")
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatal("failed reopen published a replacement instance lock")
	}
	must(t, s.Close())
	if r, err := Open(path, configuration(), false); err == nil {
		r.Close()
		t.Fatal("missing instance lease inferred safe after restart")
	}
}
