package backup

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/dryaf/deploy/internal/backup/source"
	"github.com/dryaf/deploy/internal/notification/install"
	"github.com/dryaf/deploy/internal/testutil"
)

func TestDeclaredSourceTargetsRequireExactEndpointSet(t *testing.T) {
	c := WorkerInstallation{Sources: []source.WorkerSource{{Alias: "one", Host: "source.invalid", User: "app", Port: 22}}}
	matching := []SetupSourceTarget{{Alias: "one", Host: "source.invalid", User: "app", Port: 22}}
	testutil.Must(t, requireDeclaredSourceTargets(c, matching))
	for _, targets := range [][]SetupSourceTarget{{}, {{Alias: "other", Host: "source.invalid", User: "app", Port: 22}}, {{Alias: "one", Host: "other.invalid", User: "app", Port: 22}}, {{Alias: "one", Host: "source.invalid", User: "other", Port: 22}}, {{Alias: "one", Host: "source.invalid", User: "app", Port: 2222}}, {matching[0], matching[0]}} {
		if !errors.Is(requireDeclaredSourceTargets(c, targets), ErrSourceEndpointMismatch) {
			t.Fatal("different endpoint set accepted")
		}
	}
}

func TestDeclaredBackupActionsRefuseEndpointDriftBeforeEffects(t *testing.T) {
	home, state, j := manualRunFixture(t)
	targets := []SetupSourceTarget{{Alias: "not-installed", Host: "new.invalid", User: "app", Port: 2222}}
	ctx := context.Background()
	_, err := requestDeclaredBackup(ctx, home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, manualBackupDependencies{
		stopped: func(context.Context, string, backupActivityUnit) (bool, error) {
			t.Fatal("mismatch reached service inspection")
			return false, nil
		},
		start: func(context.Context) error { t.Fatal("mismatch started backup"); return nil },
	}, targets...)
	if !errors.Is(err, ErrSourceEndpointMismatch) {
		t.Fatal(err)
	}
	// Empty dependencies make any premature service access fail the test.
	_, err = activateDeclaredWorker(ctx, home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, activationDependencies{}, targets...)
	if !errors.Is(err, ErrSourceEndpointMismatch) {
		t.Fatal(err)
	}
	_, err = QualifyDeclaredWorker(ctx, home, j.Declaration.DesiredHash, j.Declaration.HelperSHA256, "begin", install.QualificationObservation{}, targets...)
	if !errors.Is(err, ErrSourceEndpointMismatch) {
		t.Fatal(err)
	}
	for _, name := range []string{"declared_activation", "qualification_draft"} {
		var value any
		if err := state.get(name, &value); err != sql.ErrNoRows {
			t.Fatal("endpoint mismatch wrote lifecycle evidence", name, err)
		}
	}
}
