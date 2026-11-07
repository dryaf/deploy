package worker

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

func pinLEDMappingOutput(ctx context.Context, s *Store, q ledRowReader, v *ledMappingIntent) error {
	r, present, recorded, err := s.readLEDOutput(ctx, q)
	if err != nil || !present {
		return err
	}
	if !recorded || !r.Complete {
		return fmt.Errorf("finish LED output disposition before mapping")
	}
	v.Version = 2
	before := s.config
	v.OutputBeforeConfig = &before
	v.LEDOutputOperation, v.LEDOutputHash = r.Intent.Request.Operation, protocol.Hash(r)
	v.LEDOutputDisabled = r.Intent.Request.Disabled
	return nil
}

func checkLEDMappingOutputPin(ctx context.Context, q ledRowReader, v ledMappingIntent) error {
	if v.Version != 2 {
		return nil
	}
	r, err := readLEDOutputRecord(ctx, q, v.LEDOutputOperation)
	if err != nil || !r.Complete || protocol.Hash(r) != v.LEDOutputHash || r.Intent.Request.Identity != v.Identity || r.Intent.Request.Operation != v.LEDOutputOperation || r.Intent.Request.Disabled != v.LEDOutputDisabled {
		return fmt.Errorf("LED mapping lost its pinned output disposition")
	}
	return nil
}

// MappingOutputRequired is read-only and refuses malformed or lost history.
// It prevents later helper upgrades from discarding support for version-2 pins.
func MappingOutputRequired(path string) (bool, error) {
	d, err := privateDirectory(path)
	if err != nil {
		return false, err
	}
	defer d.Close()
	db, err := snapshotDB(d, "state.db", true)
	if err != nil {
		return false, err
	}
	defer db.Close()
	var count int
	if err = db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='led_migrations'").Scan(&count); err != nil || count == 0 {
		return false, err
	}
	var position int
	if err = db.QueryRow("SELECT COALESCE(max(position),0) FROM led_migrations").Scan(&position); err != nil {
		return false, err
	}
	if position > maxLEDMigrations {
		return false, fmt.Errorf("LED mapping history exceeds bound")
	}
	required := false
	for ; position > 0; position-- {
		var id string
		if err = db.QueryRow("SELECT id FROM led_migrations WHERE position=?", position).Scan(&id); err != nil {
			return false, err
		}
		m, err := readLEDMapping(context.Background(), db, id)
		if err != nil {
			return false, err
		}
		if err = checkLEDMappingOutputPin(context.Background(), db, m.Intent); err != nil {
			return false, err
		}
		required = required || m.Intent.Version == 2
	}
	// A removed SQL chain cannot turn a versioned layout into a fresh layout.
	var raw string
	if err = db.QueryRow("SELECT value FROM metadata WHERE key='led-layout'").Scan(&raw); err != nil && err != sql.ErrNoRows {
		return false, err
	}
	if raw != "" {
		var layout ledLayout
		if err = protocol.Decode([]byte(raw), &layout, protocol.MaxRequest); err != nil {
			return false, err
		}
		if layout.Version == 2 {
			if _, err = readLEDMapping(context.Background(), db, layout.Revision); err != nil {
				return false, err
			}
		}
	}
	return required, nil
}
