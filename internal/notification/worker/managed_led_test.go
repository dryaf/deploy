package worker

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func TestManagedLEDMappingPinsAuthorityAndInactiveOwnership(t *testing.T) {
	for _, mode := range []string{"active", "identity", "operation", "old-schema"} {
		t.Run(mode, func(t *testing.T) {
			path, before, after, api := mappingFixture(t)
			s, err := Open(path, before, false)
			must(t, err)
			id := s.Identity()
			operation, err := safefs.NewID("n")
			must(t, err)
			if mode != "active" {
				must(t, s.Revoke(context.Background(), producer, "generation-one"))
			}
			if mode == "old-schema" {
				_, err = s.db.Exec("PRAGMA user_version=9")
				must(t, err)
			}
			must(t, s.Close())
			if mode == "identity" {
				id.Recovery = "changed"
			}
			if mode == "operation" {
				operation = "invalid"
			}
			if mode != "operation" && PreflightManagedLEDBindings(context.Background(), path, before, after, id) == nil {
				t.Fatal("read-only preflight accepted inactive/identity/schema violation")
			}
			calls := len(api.cleared)
			states := protocol.Hash(api.states)
			if _, err = MigrateManagedLEDBindings(context.Background(), path, before, after, migrationSocket, api, id, operation); err == nil {
				t.Fatal("unpinned/active migration accepted")
			}
			if calls != len(api.cleared) || states != protocol.Hash(api.states) {
				t.Fatal("refused migration changed daemon")
			}
			if _, err = os.Stat(filepath.Join(path, "led-migration.json")); !os.IsNotExist(err) {
				t.Fatal("refusal created intent", err)
			}
		})
	}
}

func TestManagedLEDMappingPreflightWhileRuntimeOwnsLease(t *testing.T) {
	path, before, after, _ := mappingFixture(t)
	s, err := Open(path, before, false)
	must(t, err)
	defer s.Close()
	ctx := context.Background()
	must(t, s.Revoke(ctx, producer, "generation-one"))
	must(t, PreflightManagedLEDBindings(ctx, path, before, after, s.Identity()))
	// Fault ownership on the changed binding must be resolved before an installer
	// stops the service, even if the daemon currently has no matching state.
	must(t, s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := s.currentLEDState(tx, "selected", "critical")
		return err
	}))
	if err = PreflightManagedLEDBindings(ctx, path, before, after, s.Identity()); err == nil {
		t.Fatal("unresolved fault passed preflight")
	}
	var count int
	must(t, s.db.QueryRow("SELECT count(*) FROM led_states WHERE binding='selected'").Scan(&count))
	if count != 1 {
		t.Fatal("preflight removed fault ownership")
	}
	if _, err = os.Stat(filepath.Join(path, "led-migration.json")); !os.IsNotExist(err) {
		t.Fatal("preflight published migration gate", err)
	}
}

func TestManagedLEDMappingResumePinsOperation(t *testing.T) {
	path, before, after, api := mappingFixture(t)
	ctx := context.Background()
	s, err := Open(path, before, false)
	must(t, err)
	id := s.Identity()
	must(t, s.Revoke(ctx, producer, "generation-one"))
	must(t, s.Close())
	operation, err := safefs.NewID("n")
	must(t, err)
	authority := &managedLEDAuthority{id, operation}
	_, err = migrateLEDBindingsPinned(ctx, path, before, after, migrationSocket, api, func(at string) error {
		if at == "mapping_prepared" {
			return errors.New("interrupted")
		}
		return nil
	}, authority)
	if err == nil {
		t.Fatal("fault missing")
	}
	other, err := safefs.NewID("n")
	must(t, err)
	if _, err = MigrateManagedLEDBindings(ctx, path, before, after, migrationSocket, api, id, other); err == nil {
		t.Fatal("different installation operation resumed migration")
	}
	result, err := MigrateManagedLEDBindings(ctx, path, before, after, migrationSocket, api, id, operation)
	must(t, err)
	if result.Operation != operation {
		t.Fatal(result)
	}
	must(t, VerifyManagedLEDMapping(ctx, path, before, after, migrationSocket, id, operation))
	if err = VerifyManagedLEDMapping(ctx, path, before, after, migrationSocket, id, other); err == nil {
		t.Fatal("different completion accepted")
	}
	if _, err = MigrateManagedLEDBindings(ctx, path, before, after, migrationSocket, api, id, other); err == nil {
		t.Fatal("completed mapping adopted by different operation")
	}
}
