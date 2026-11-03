package worker

import (
	"context"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

// VerifyManagedSchemaMigration inspects committed migration evidence while a
// verification process may own the worker lease. It never completes pending work.
func VerifyManagedSchemaMigration(ctx context.Context, path string, c Config, id protocol.Identity, operation string, from int) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !id.Valid() || !safefs.ValidID(operation, "n") || from != 9 || currentSchema != 10 {
		return fmt.Errorf("invalid migration completion request")
	}
	d, err := privateDirectory(path)
	if err != nil {
		return err
	}
	defer d.Close()
	var current protocol.Identity
	if err = d.ReadJSON("identity.json", &current); err != nil {
		return err
	}
	if current != id {
		return fmt.Errorf("migration identity changed")
	}
	var gate ManagedSchemaMigration
	if err = d.ReadJSON("schema-migration.json", &gate); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("schema migration has not released its state gate")
	}
	stage, err := d.Sub("schema-migrations/" + operation)
	if err != nil {
		return err
	}
	defer stage.Close()
	var intent, complete ManagedSchemaMigration
	if err = stage.ReadJSON("intent.json", &intent); err != nil {
		return err
	}
	if err = stage.ReadJSON("complete.json", &complete); err != nil {
		return err
	}
	if intent != complete || intent.Operation != operation || intent.Identity != id || intent.ConfigHash != protocol.Hash(c) || intent.From != from || intent.To != currentSchema || !safefs.DigestPattern.MatchString(intent.BeforeHash) || intent.BeforeBytes <= 0 || intent.BeforeBytes > c.MaxDatabaseBytes {
		return fmt.Errorf("invalid migration completion evidence")
	}
	hash, size, err := schemaDatabaseDigest(stage, "before.db", c.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	if hash != intent.BeforeHash || size != intent.BeforeBytes {
		return fmt.Errorf("retained schema backup changed")
	}
	f, err := snapshotFile(d, "state.db", c.MaxDatabaseBytes)
	if err != nil {
		return err
	}
	f.Close()
	db, err := snapshotDB(d, "state.db", true)
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	var body, marker string
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key='identity'").Scan(&body); err != nil {
		return err
	}
	var stored protocol.Identity
	if err = protocol.Decode([]byte(body), &stored, 4096); err != nil {
		return err
	}
	if err = db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", "schema-migration:"+operation).Scan(&marker); err != nil {
		return err
	}
	if version != currentSchema || stored != id || marker != protocol.Hash(intent) {
		return fmt.Errorf("migration database acknowledgement changed")
	}
	return nil
}
