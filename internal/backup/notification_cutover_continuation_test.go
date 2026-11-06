//go:build linux

package backup

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestBackupCutoverContinuationAfterUpdate(t *testing.T) {
	home, c, original, deps, session := backupCutoverFixture(t)
	ctx := context.Background()
	testutil.Must(t, runBackupCutover(ctx, home, c, original, false, deps))
	d, err := safefs.Open(backupCutoverPaths(home, c).State)
	testutil.Must(t, err)
	defer d.Close()
	var intent backupCutoverIntent
	testutil.Must(t, d.ReadJSON(backupCutoverFile, &intent))
	// Model the committed configuration and artifact changes of a completed
	// setup update; ownership and the original source decision stay immutable.
	c.Policy.Storage.MinFreeBytes++
	testutil.Must(t, safefs.WritePrivateJSON(backupCutoverPaths(home, c).Config, c))
	next := original
	next.Version, next.Predecessor = 2, notify.Hash(intent)
	next.ConfigHash, next.HelperHash = notify.Hash(c), notify.Hash("new-helper")
	next.Managed.ManifestHash = notify.Hash("current-managed-manifest")
	if err = runBackupCutover(ctx, home, c, original, true, deps); err == nil {
		t.Fatal("stale installation pin accepted")
	}
	if err = runBackupCutover(ctx, home, c, next, false, deps); err == nil {
		t.Fatal("continuation created authority")
	}
	for _, mutate := range []func(*backupCutoverSpec){
		func(s *backupCutoverSpec) { s.Predecessor = notify.Hash("foreign") },
		func(s *backupCutoverSpec) { s.Managed.Generation = "foreign" },
		func(s *backupCutoverSpec) { s.Managed.Route = "foreign" },
		func(s *backupCutoverSpec) { s.Managed.Root += "/foreign" },
		func(s *backupCutoverSpec) { s.EvidenceHash = notify.Hash("foreign") },
	} {
		bad := next
		mutate(&bad)
		if err = runBackupCutover(ctx, home, c, bad, true, deps); err == nil {
			t.Fatal("changed lineage accepted")
		}
	}
	stop := errors.New("interrupted after canonical revoke")
	deps.Fault = func(at string) error {
		if at == "cutover-revoked" {
			return stop
		}
		return nil
	}
	if err = runBackupCutover(ctx, home, c, next, true, deps); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	deps.Fault = nil
	testutil.Must(t, runBackupCutover(ctx, home, c, next, true, deps))
	testutil.Must(t, runBackupCutover(ctx, home, c, next, true, deps))
	rollback := backupLegacyRollbackSpec{Version: 1, Cutover: next, CredentialHash: legacyCredentialPin(t, c), Generation: "legacy-after-update", Reason: "updated installation rollback"}
	testutil.Must(t, runBackupLegacyRollback(ctx, home, c, rollback, false, deps))
	testutil.Must(t, runBackupLegacyRollback(ctx, home, c, rollback, true, deps))
	var retained backupCutoverIntent
	testutil.Must(t, d.ReadJSON(backupCutoverFile, &retained))
	if notify.Hash(retained) != notify.Hash(intent) {
		t.Fatal("original evidence rewritten")
	}
	var restored backupLegacyRollbackIntent
	testutil.Must(t, d.ReadJSON(legacyRollbackFile, &restored))
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	var evidence backupCutoverEvidence
	testutil.Must(t, backupTransferTx(ctx, state, func(tx *sql.Tx) error { var e error; evidence, e = readBackupCutoverEvidence(tx); return e }))
	testutil.Must(t, state.Close())
	shared := next
	shared.Version, shared.Predecessor, shared.Managed.Generation = 1, "", "shared-after-update"
	shared.EvidenceHash, shared.Decisions = notify.Hash(evidence), map[string]backupCutoverDecision{}
	for _, row := range evidence.Rows {
		shared.Decisions[row.ID] = backupCutoverDecision{notify.Hash(row), "unsent", "retained unattempted source"}
	}
	session.g = shared.Managed.Generation
	testutil.Must(t, runBackupRemigration(ctx, home, c, backupRemigrationSpec{1, shared, notify.Hash(restored)}, false, deps))
}
