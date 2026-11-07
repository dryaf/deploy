package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dryaf/deploy/internal/notification/install"
	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func declaredQualificationFixture(t *testing.T) (string, *backupState, WorkerSetupJournal, WorkerInstallation) {
	t.Helper()
	home, state, j := setupObservationFixture(t)
	root, err := safefs.Open(home)
	testutil.Must(t, err)
	defer root.Close()
	var c WorkerInstallation
	testutil.Must(t, root.ReadJSON(".config/deploy/backup-worker/runtime.json", &c))
	c.Image = "localhost/backup@sha256:" + strings.Repeat("a", 64)
	body, err := json.Marshal(c)
	testutil.Must(t, err)
	testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/deploy/backup-worker/runtime.json"), body, 0600))
	j.OwnedFiles[".config/deploy/backup-worker/runtime.json"] = fmtDigest(body)
	testutil.Must(t, root.WriteJSON(".local/state/deploy/backup-worker/setup.json", j, true))
	return home, state, j, c
}

func TestBackupGuidedQualificationRetainsPartialEvidenceAndWindow(t *testing.T) {
	home, s, j, c := declaredQualificationFixture(t)
	ctx := context.Background()
	call := func(action string, o install.QualificationObservation) install.QualificationSession {
		v, err := QualifyDeclaredWorker(ctx, home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, action, o)
		testutil.Must(t, err)
		return v
	}
	draft := call("begin", install.QualificationObservation{})
	if draft.Recorded || draft.EvidenceComplete || len(draft.Checks) != 2 {
		t.Fatal(draft)
	}
	o := install.QualificationObservation{Session: draft.ID, Scope: "backup", ID: draft.Checks[0].ID, Result: "passed", EvidenceHash: strings.Repeat("d", 64)}
	call("observe", o)
	var before backupQualificationDraft
	testutil.Must(t, s.get("qualification_draft", &before))
	call("observe", o)
	var after backupQualificationDraft
	testutil.Must(t, s.get("qualification_draft", &after))
	if notify.Hash(before) != notify.Hash(after) {
		t.Fatal("duplicate observation renewed evidence")
	}
	partial := call("commit", install.QualificationObservation{Session: draft.ID})
	if !partial.Recorded || partial.EvidenceComplete {
		t.Fatal(partial)
	}
	if retry := call("commit", install.QualificationObservation{Session: draft.ID}); notify.Hash(retry) != notify.Hash(partial) {
		t.Fatal("commit retry changed outcome")
	}
	next := call("begin", install.QualificationObservation{})
	if next.ID == draft.ID || next.Checks[0].Result != "passed" || next.Recorded || !next.CreatedAt.Equal(draft.CreatedAt) {
		t.Fatal("partial continuation lost evidence or renewed window", next)
	}
	o.Session, o.ID = next.ID, next.Checks[1].ID
	call("observe", o)
	complete := call("commit", install.QualificationObservation{Session: next.ID})
	if !complete.EvidenceComplete {
		t.Fatal(complete)
	}
	if retry := call("begin", install.QualificationObservation{}); notify.Hash(retry) != notify.Hash(complete) {
		t.Fatal("completed begin changed report")
	}
	var proof WorkerQualification
	testutil.Must(t, s.get("qualification", &proof))
	if !currentBackupQualification(proof, c, time.Now().UTC()) {
		t.Fatal("exact completed proof was not recorded")
	}
	raw, _ := json.Marshal(complete)
	if strings.Contains(string(raw), o.EvidenceHash) || strings.Contains(string(raw), c.ConfigHash) {
		t.Fatal("private qualification binding leaked")
	}
	var installed string
	testutil.Must(t, s.get("installation", &installed))
	if installed != "staged-awaiting-qualification" {
		t.Fatal("qualification activated worker")
	}
}

func TestBackupGuidedQualificationDriftExpiryAndAtomicRollback(t *testing.T) {
	_, s, j, c := declaredQualificationFixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	call := func(action string, o install.QualificationObservation, at time.Time) (install.QualificationSession, error) {
		var out install.QualificationSession
		err := backupTransferTx(ctx, s, func(tx *sql.Tx) error {
			var e error
			out, e = backupQualificationDraftTx(tx, j, c, action, o, at)
			return e
		})
		return out, err
	}
	draft, err := call("begin", install.QualificationObservation{}, now)
	testutil.Must(t, err)
	o := install.QualificationObservation{Session: draft.ID, Scope: "backup", ID: draft.Checks[0].ID, Result: "passed", EvidenceHash: strings.Repeat("e", 64)}
	_, err = call("observe", o, now.Add(time.Second))
	testutil.Must(t, err)
	o.EvidenceHash = strings.Repeat("f", 64)
	if _, err = call("observe", o, now.Add(time.Second)); err == nil {
		t.Fatal("conflicting observation overwritten")
	}
	err = backupTransferTx(ctx, s, func(tx *sql.Tx) error {
		_, e := backupQualificationDraftTx(tx, j, c, "commit", install.QualificationObservation{Session: draft.ID}, now.Add(time.Second))
		if e != nil {
			return e
		}
		return errors.New("simulated crash before commit")
	})
	if err == nil {
		t.Fatal("missing simulated rollback")
	}
	var proof WorkerQualification
	if err = s.get("qualification", &proof); err != sql.ErrNoRows {
		t.Fatal("rollback published proof", err)
	}
	expired, err := call("begin", install.QualificationObservation{}, now.Add(8*24*time.Hour))
	testutil.Must(t, err)
	if expired.ID == draft.ID || expired.Checks[0].Result != "pending" {
		t.Fatal("expired evidence reused", expired)
	}
	if _, err = call("commit", install.QualificationObservation{Session: draft.ID}, now.Add(8*24*time.Hour)); err == nil {
		t.Fatal("expired commit accepted")
	}
	j.Declaration.DesiredHash = strings.Repeat("9", 64)
	if _, err = call("observe", install.QualificationObservation{Session: expired.ID, Scope: "backup", ID: expired.Checks[0].ID, Result: "passed", EvidenceHash: strings.Repeat("e", 64)}, now.Add(8*24*time.Hour)); err == nil {
		t.Fatal("changed desired policy accepted")
	}
}

func TestBackupGuidedQualificationRejectsChangedAuthority(t *testing.T) {
	home, _, j, _ := declaredQualificationFixture(t)
	ctx := context.Background()
	for _, pins := range [][2]string{{strings.Repeat("0", 64), j.Declaration.HelperSHA256}, {j.Declaration.DesiredHash, strings.Repeat("0", 64)}} {
		if _, err := QualifyDeclaredWorker(ctx, home, pins[0], pins[1], "begin", install.QualificationObservation{}); err == nil {
			t.Fatal("changed authority accepted")
		}
	}
	testutil.Must(t, os.WriteFile(filepath.Join(home, ".config/deploy/backup-worker/runtime.json"), []byte("{}"), 0600))
	if _, err := QualifyDeclaredWorker(ctx, home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, "begin", install.QualificationObservation{}); err == nil {
		t.Fatal("drifted runtime accepted")
	}
}

func TestBackupQualificationPrivateInputRotationInvalidatesDraft(t *testing.T) {
	_, s, j, c := declaredQualificationFixture(t)
	file := filepath.Join(t.TempDir(), "monitor")
	testutil.Must(t, os.WriteFile(file, []byte("https://example.invalid/private-before"), 0600))
	c.Policy.Monitoring.BackupPingURLFile = file
	now := time.Now().UTC()
	var initial, next install.QualificationSession
	testutil.Must(t, backupTransferTx(context.Background(), s, func(tx *sql.Tx) error {
		var e error
		initial, e = backupQualificationDraftTx(tx, j, c, "begin", install.QualificationObservation{}, now)
		if e != nil {
			return e
		}
		_, e = backupQualificationDraftTx(tx, j, c, "observe", install.QualificationObservation{Session: initial.ID, Scope: "backup", ID: initial.Checks[0].ID, Result: "passed", EvidenceHash: strings.Repeat("a", 64)}, now)
		return e
	}))
	testutil.Must(t, os.WriteFile(file, []byte("https://example.invalid/private-after"), 0600))
	err := backupTransferTx(context.Background(), s, func(tx *sql.Tx) error {
		_, e := backupQualificationDraftTx(tx, j, c, "commit", install.QualificationObservation{Session: initial.ID}, now)
		return e
	})
	if err == nil {
		t.Fatal("rotated private input accepted old observations")
	}
	testutil.Must(t, backupTransferTx(context.Background(), s, func(tx *sql.Tx) error {
		var e error
		next, e = backupQualificationDraftTx(tx, j, c, "begin", install.QualificationObservation{}, now)
		return e
	}))
	if next.ID == initial.ID || next.Checks[0].Result != "pending" {
		t.Fatal("rotated input carried passing evidence")
	}
	var raw string
	testutil.Must(t, s.db.QueryRow("SELECT value FROM meta WHERE key='qualification_draft'").Scan(&raw))
	if strings.Contains(raw, "private-before") || strings.Contains(raw, "private-after") {
		t.Fatal("qualification journal leaked private input")
	}
}

func TestBackupGuidedQualificationRejectsLEDEvidence(t *testing.T) {
	home, state, j, _ := declaredQualificationFixture(t)
	ctx := context.Background()
	session, err := QualifyDeclaredWorker(ctx, home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, "begin", install.QualificationObservation{})
	testutil.Must(t, err)
	var before backupQualificationDraft
	testutil.Must(t, state.get("qualification_draft", &before))
	o := install.QualificationObservation{Session: session.ID, LEDVersion: "synthetic-v1", LEDConfigHash: strings.Repeat("a", 64)}
	for _, action := range []string{"begin", "observe", "commit"} {
		if _, err = QualifyDeclaredWorker(ctx, home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, action, o); err == nil {
			t.Fatal("backup accepted gateway LED environment", action)
		}
	}
	var after backupQualificationDraft
	testutil.Must(t, state.get("qualification_draft", &after))
	if notify.Hash(before) != notify.Hash(after) {
		t.Fatal("rejected environment changed backup draft")
	}
}
