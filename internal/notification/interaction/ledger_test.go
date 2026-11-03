package interaction

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
	_ "modernc.org/sqlite"
)

func TestDomainLedgerInitializationRollsBackWithItsIdentity(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "domain.db")+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	p := protocol.Producer{ID: "backup", Epoch: "one"}
	id := protocol.Identity{Worker: "gateway", Recovery: "one"}
	initialize := func(tx *sql.Tx) error {
		_, err := tx.Exec("CREATE TABLE local_domain(id INTEGER PRIMARY KEY)")
		return err
	}
	if _, err = OpenDomainLedger(ctx, db, p, id, true, func(tx *sql.Tx) error {
		if e := initialize(tx); e != nil {
			return e
		}
		return fmt.Errorf("injected domain initialization failure")
	}); err == nil {
		t.Fatal("initialization failure ignored")
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&count); err != nil || count != 0 {
		t.Fatal("partial initialization survived", count, err)
	}
	if _, err = OpenDomainLedger(ctx, db, p, id, true, initialize); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenDomainLedger(ctx, db, p, id, false, func(*sql.Tx) error { t.Fatal("normal open reconstructed domain state"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestPermanentPreparationRejectionIsImmutableAndFencesExecution(t *testing.T) {
	l, _, c, g, _ := ledgerFixture(t)
	ctx := context.Background()
	c.ID = "C-refused"
	c.RequestKey = "refused"
	c.Descriptor.Confirm = true
	if err := l.Import(ctx, c); err != nil {
		t.Fatal(err)
	}
	first, err := l.RejectPreparation(ctx, c, "Current evidence does not authorize acceptance.")
	if err != nil {
		t.Fatal(err)
	}
	again, err := l.RejectPreparation(ctx, c, "Different refusal text")
	if err != nil || protocol.Hash(first) != protocol.Hash(again) {
		t.Fatal("rejection changed on replay", err)
	}
	g.CommandID = c.ID
	g.CommandHash = protocol.Hash(c)
	if _, err = l.Execute(ctx, c, g, "", c.IssuedAt, func(*sql.Tx, Command) (Result, error) { t.Fatal("rejected preparation executed"); return Result{}, nil }); err == nil {
		t.Fatal("rejected preparation received execution")
	}
	results, err := l.PendingResults(ctx)
	if err != nil || len(results) != 1 || results[0].Result.State != "rejected" || results[0].Result.Preparation != nil {
		t.Fatal(results, err)
	}
}

func ledgerFixture(t *testing.T) (*Ledger, *sql.DB, Command, Grant, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=synchronous(FULL)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	p := protocol.Producer{ID: "vuln", Epoch: "one"}
	identity := protocol.Identity{Worker: "gateway", Recovery: "one"}
	l, err := OpenLedger(context.Background(), db, p, identity, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE findings(subject TEXT,id TEXT,present INTEGER);INSERT INTO findings VALUES('a','visible',1),('b','confidential',1)"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	c := Command{ID: "C-one", RequestKey: "request-one", InputHash: "input-one", Producer: p, Scope: Scope{Subjects: []string{"a"}}, IssuedAt: now, Deadline: now.Add(time.Minute), Descriptor: Descriptor{Effect: "mutation"}}
	if err = l.Import(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	g := Grant{ID: "grant-one", Identity: identity, CommandID: c.ID, CommandHash: protocol.Hash(c), ExpiresAt: now.Add(10 * time.Second)}
	return l, db, c, g, path
}
func TestLedgerAtomicDomainAdmissionAndRestart(t *testing.T) {
	l, db, c, g, path := ledgerFixture(t)
	ctx := context.Background()
	calls := 0
	apply := func(tx *sql.Tx, cmd Command) (Result, error) {
		calls++
		if !cmd.Scope.Contains("a") {
			return Result{}, fmt.Errorf("denied")
		}
		_, err := tx.Exec("UPDATE findings SET present=0 WHERE subject='a'")
		return Result{State: "succeeded", Scope: cmd.Scope, Text: "A updated"}, err
	}
	r, err := l.Execute(ctx, c, g, "", c.IssuedAt, apply)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	db2.SetMaxOpenConns(1)
	l2, err := OpenLedger(ctx, db2, c.Producer, g.Identity, false)
	if err != nil {
		t.Fatal(err)
	}
	again, err := l2.Execute(ctx, c, g, "", c.IssuedAt.Add(time.Hour), apply)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.Hash(again) != protocol.Hash(r) || calls != 1 {
		t.Fatal("effect repeated after restart")
	}
	var secret int
	if err = db2.QueryRow("SELECT present FROM findings WHERE subject='b'").Scan(&secret); err != nil || secret != 1 {
		t.Fatal("other subject changed", err)
	}
}
func TestLedgerRollbackAndFence(t *testing.T) {
	l, db, c, g, _ := ledgerFixture(t)
	ctx := context.Background()
	_, err := l.Execute(ctx, c, g, "", c.IssuedAt, func(tx *sql.Tx, c Command) (Result, error) {
		_, e := tx.Exec("UPDATE findings SET present=0")
		if e != nil {
			return Result{}, e
		}
		return Result{}, fmt.Errorf("domain precondition failed")
	})
	if err == nil {
		t.Fatal("failed domain transaction committed")
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM findings WHERE present=1").Scan(&count); err != nil || count != 2 {
		t.Fatal("partial domain update", err)
	}
	if err = l.FenceAdmission(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	_, err = l.Execute(ctx, c, g, "", c.IssuedAt, func(*sql.Tx, Command) (Result, error) { t.Fatal("fenced callback ran"); return Result{}, nil })
	if err == nil {
		t.Fatal("old grant passed tombstone")
	}
}
func TestLedgerFiltersBeforeSummarizing(t *testing.T) {
	l, _, c, g, _ := ledgerFixture(t)
	r, err := l.Execute(context.Background(), c, g, "", c.IssuedAt, func(tx *sql.Tx, c Command) (Result, error) {
		count := 0
		for _, subject := range c.Scope.Subjects {
			var n int
			if err := tx.QueryRow("SELECT count(*) FROM findings WHERE subject=? AND present=1", subject).Scan(&n); err != nil {
				return Result{}, err
			}
			count += n
		}
		return Result{State: "succeeded", Scope: c.Scope, Text: fmt.Sprintf("%d findings in authorized scope", count)}, nil
	})
	if err != nil || r.Text != "1 findings in authorized scope" {
		t.Fatal(r, err)
	}
}
func TestLedgerRequiresExplicitInitialization(t *testing.T) {
	l, db, c, g, _ := ledgerFixture(t)
	_ = l
	if _, err := db.Exec("DELETE FROM interactive_identity"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(context.Background(), db, c.Producer, g.Identity, false); err == nil {
		t.Fatal("missing identity silently recreated")
	}
}

func TestLedgerRejectsUnsendableResultBeforeCommit(t *testing.T) {
	l, db, c, g, _ := ledgerFixture(t)
	_, err := l.Execute(context.Background(), c, g, "", c.IssuedAt, func(tx *sql.Tx, c Command) (Result, error) {
		_, err := tx.Exec("UPDATE findings SET present=0 WHERE subject='a'")
		return Result{State: "succeeded", Scope: c.Scope, Text: strings.Repeat("x", 3001)}, err
	})
	if err == nil {
		t.Fatal("unsendable result committed")
	}
	var count int
	if err = db.QueryRow("SELECT count(*) FROM findings WHERE present=1").Scan(&count); err != nil || count != 2 {
		t.Fatal("domain mutation survived invalid result", err)
	}
}

func TestLedgerCannotProveNonAdmissionAfterExecution(t *testing.T) {
	l, _, c, g, _ := ledgerFixture(t)
	_, err := l.Execute(context.Background(), c, g, "", c.IssuedAt, func(*sql.Tx, Command) (Result, error) {
		return Result{State: "succeeded", Scope: c.Scope, Text: "done"}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l.ProveNonAdmission(context.Background(), c, g); err == nil {
		t.Fatal("false non-admission proof")
	}
}
