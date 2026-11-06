//go:build linux

package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	notify "github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/testutil"
	"github.com/dryaf/deploy/internal/ui"
)

func (*cutoverFixtureServices) PrepareMigrationCycle(context.Context, WorkerUnitPaths, string) error {
	return nil
}

func TestBackupMigrationCycleDryRun(t *testing.T) {
	before := ui.DryRun
	ui.DryRun = true
	defer func() { ui.DryRun = before }()
	for _, args := range [][]string{{"--local", "--review"}, {"--local", "--spec", "/absent/cycle.json"}} {
		testutil.Must(t, runBackupMigrationCycleCLI(args, &bytes.Buffer{}))
	}
	for _, args := range [][]string{{"--review"}, {"--local"}, {"--local", "--spec", "relative"}, {"--local", "--review", "--spec", "/absent"}} {
		if runBackupMigrationCycleCLI(args, &bytes.Buffer{}) == nil {
			t.Fatal("invalid cycle scope accepted", args)
		}
	}
}

func TestBackupMigrationCycleRetainsHeldResolution(t *testing.T) {
	home, c, remigration, deps, _, _ := remigrationFixture(t, "ambiguous", true)
	ctx := context.Background()
	testutil.Must(t, runBackupRemigration(ctx, home, c, remigration, false, deps))
	state, err := openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	var owner backupNotificationOwner
	testutil.Must(t, state.get("notification_owner", &owner))
	spec := backupMigrationCycleSpec{Version: 1, Managed: remigration.Cutover.Managed, ConfigHash: notify.Hash(c), HelperHash: remigration.Cutover.HelperHash, OwnerHash: notify.Hash(owner), Reason: "preserve reviewed ambiguous source"}
	if _, err := runBackupMigrationCycle(ctx, home, c, spec, deps); err == nil {
		t.Fatal("unresolved source entered next cycle")
	}
	var review backupLegacyReview
	testutil.Must(t, backupTransferTx(ctx, state, func(tx *sql.Tx) error {
		var e error
		review, e = readBackupLegacyReview(tx, c, "legacy-one", time.Now().UTC())
		return e
	}))
	resolution := backupLegacyResolution{Version: 1, Review: review, ReviewHash: notify.Hash(review), Action: "retry", Reason: "reviewed duplicate risk", AckDuplicateRisk: true}
	testutil.Must(t, resolveBackupLegacy(ctx, state, c, resolution, review.ReviewedAt, nil))
	testutil.Must(t, state.Close())
	_, err = runBackupMigrationCycle(ctx, home, c, spec, deps)
	testutil.Must(t, err)
	state, err = openBackupState(backupCutoverPaths(home, c).State, false)
	testutil.Must(t, err)
	defer state.Close()
	testutil.Must(t, state.get("notification_owner", &owner))
	testutil.Must(t, requireBackupCutoverOwner(state, owner))
	testutil.Must(t, backupTransferTx(ctx, state, requireResolvedLegacyRows))
	testutil.Must(t, os.Remove(filepath.Join(state.directory, legacyResolutionFile(legacyResolutionHash(review)))))
	if requireBackupCutoverOwner(state, owner) == nil {
		t.Fatal("lost archived resolution proof accepted")
	}
}

func TestBackupMigrationCyclesRetainGenerations(t *testing.T) {
	for _, boundary := range []string{"cycle-captured", "cycle-intent", "cycle-revoked", "cycle-archive", "cycle-transaction", "cycle-committed"} {
		t.Run(boundary, func(t *testing.T) {
			home, c, first, deps, session, oldMessage := remigrationFixture(t, "unsent")
			ctx := context.Background()
			testutil.Must(t, runBackupRemigration(ctx, home, c, first, false, deps))
			d, err := safefs.Open(backupCutoverPaths(home, c).State)
			testutil.Must(t, err)
			defer d.Close()
			var original backupCutoverIntent
			testutil.Must(t, d.ReadJSON(backupCutoverFile, &original))
			generation := first.Cutover.Managed.Generation
			var oldestCycle string
			for cycle := 0; cycle < 3; cycle++ {
				state, err := openBackupState(backupCutoverPaths(home, c).State, false)
				testutil.Must(t, err)
				var owner backupNotificationOwner
				testutil.Must(t, state.get("notification_owner", &owner))
				testutil.Must(t, requireBackupCutoverOwner(state, owner))
				testutil.Must(t, state.Close())
				managed := first.Cutover.Managed
				managed.Generation = generation
				spec := backupMigrationCycleSpec{Version: 1, Managed: managed, HelperHash: first.Cutover.HelperHash, ConfigHash: notify.Hash(c), OwnerHash: notify.Hash(owner), Reason: "synthetic repeated rollback/remigration"}
				stop := errors.New("cycle interrupted")
				deps.Fault = func(at string) error {
					if at == boundary {
						return stop
					}
					return nil
				}
				if _, err = runBackupMigrationCycle(ctx, home, c, spec, deps); !errors.Is(err, stop) {
					t.Fatal("expected interruption", cycle, err)
				}
				if boundary != "cycle-committed" {
					fencedState, e := openBackupState(backupCutoverPaths(home, c).State, false)
					testutil.Must(t, e)
					if _, e = currentSharedBackupOwner(fencedState); e == nil {
						t.Fatal("pending cycle admitted reporting")
					}
					if requireNotificationRecoveryInactiveTx(fencedState) == nil {
						t.Fatal("pending cycle admitted fresh exports")
					}
					testutil.Must(t, fencedState.Close())
				}
				deps.Fault = nil
				record, err := runBackupMigrationCycle(ctx, home, c, spec, deps)
				testutil.Must(t, err)
				again, err := runBackupMigrationCycle(ctx, home, c, spec, deps)
				testutil.Must(t, err)
				if notify.Hash(again) != notify.Hash(record) {
					t.Fatal("cycle retry changed archive")
				}
				if cycle == 0 {
					oldestCycle = notify.Hash(record)
				}
				rollbackCutover := original.Spec
				rollbackCutover.Version, rollbackCutover.Predecessor = 4, notify.Hash(record)
				rollbackCutover.Managed = managed
				rollback := backupLegacyRollbackSpec{Version: 1, Cutover: rollbackCutover, CredentialHash: legacyCredentialPin(t, c), Generation: fmt.Sprintf("cycle-legacy-%d", cycle), Reason: "reviewed next legacy generation"}
				testutil.Must(t, runBackupLegacyRollback(ctx, home, c, rollback, false, deps))
				testutil.Must(t, runBackupLegacyRollback(ctx, home, c, rollback, true, deps))
				state, err = openBackupState(backupCutoverPaths(home, c).State, false)
				testutil.Must(t, err)
				rollbackPath, err := currentMigrationArtifact(state, legacyRollbackFile)
				testutil.Must(t, err)
				var retained backupLegacyRollbackIntent
				testutil.Must(t, d.ReadJSON(rollbackPath, &retained))
				var evidence backupCutoverEvidence
				testutil.Must(t, backupTransferTx(ctx, state, func(tx *sql.Tx) error { var e error; evidence, e = readBackupCutoverEvidence(tx); return e }))
				testutil.Must(t, state.Close())
				next := original.Spec
				generation = fmt.Sprintf("cycle-shared-%d", cycle)
				next.Managed.Generation, next.EvidenceHash = generation, notify.Hash(evidence)
				next.Decisions = map[string]backupCutoverDecision{}
				for _, row := range evidence.Rows {
					next.Decisions[row.ID] = backupCutoverDecision{notify.Hash(row), "unsent", "retained unattempted source"}
				}
				testutil.Must(t, runBackupRemigration(ctx, home, c, backupRemigrationSpec{1, next, notify.Hash(retained)}, false, deps))
				if _, granted, err := session.store.Grant(ctx, oldMessage.Producer, oldMessage.ID); err != nil || granted {
					t.Fatal("old generation revived", granted, err)
				}
			}
			state, err := openBackupState(backupCutoverPaths(home, c).State, false)
			testutil.Must(t, err)
			defer state.Close()
			var owner backupNotificationOwner
			testutil.Must(t, state.get("notification_owner", &owner))
			testutil.Must(t, requireBackupCutoverOwner(state, owner))
			testutil.Must(t, os.Remove(filepath.Join(state.directory, migrationArtifact(oldestCycle, "cycle.json"))))
			if err = requireBackupCutoverOwner(state, owner); err == nil {
				t.Fatal("lost oldest cycle evidence accepted")
			}
		})
	}
}
