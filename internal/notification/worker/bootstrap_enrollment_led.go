package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

func checkBootstrapEnrollmentLEDHistory(q ledRowReader, e bootstrapEnrollment) error {
	if e.Receipt.LEDOutputHash == "" {
		return nil
	}
	r, err := readLEDOutputRecord(context.Background(), q, e.Receipt.LEDOutputOperation)
	if err != nil || !r.Complete || r.Intent.Request.Identity != e.Receipt.Identity || r.Intent.Request.Operation != e.Receipt.LEDOutputOperation || protocol.Hash(r) != e.Receipt.LEDOutputHash {
		return fmt.Errorf("enrollment lost its pinned LED output history")
	}
	return nil
}

// PreflightBootstrapEnrollment is read-only while verification is running. The
// stopped domain transaction repeats the policy, output and empty-store checks.
func PreflightBootstrapEnrollment(ctx context.Context, state string, id protocol.Identity, before, after Config, outputOperation string, outputDisabled bool) error {
	if err := after.Validate(); err != nil {
		return err
	}
	want := after
	want.Producers = nil
	want.Interaction = nil
	if protocol.Hash(before) != protocol.Hash(want) {
		return fmt.Errorf("enrollment changes more than consumers and initial command policy")
	}
	if err := PreflightBootstrapLimits(ctx, state, id, before, before); err != nil {
		return err
	}
	d, err := safefs.Open(state)
	if err != nil {
		return err
	}
	defer d.Close()
	db, err := snapshotDB(d, "state.db", true)
	if err != nil {
		return err
	}
	defer db.Close()
	s := &Store{dir: d, db: db, config: before, identity: id}
	if after.Interaction != nil {
		if err = checkBootstrapCommandEnrollment(db, bootstrapEnrollment{Receipt: BootstrapEnrollmentReceipt{Version: 3}, Config: after}); err != nil {
			return err
		}
	}
	return s.RequireLEDOutput(ctx, outputOperation, outputDisabled)
}

// BootstrapEnrollmentLEDRequired also checks SQL so a missing receipt cannot
// make a helper downgrade appear compatible with the installed database.
func BootstrapEnrollmentLEDRequired(state string) (bool, error) {
	e, err := retainedBootstrapEnrollment(state)
	return e != nil && e.Receipt.LEDOutputHash != "", err
}

func retainedBootstrapEnrollment(state string) (*bootstrapEnrollment, error) {
	d, err := safefs.Open(state)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	var e bootstrapEnrollment
	readErr := d.ReadJSON("bootstrap-enrollment.json", &e)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, readErr
	}
	db, err := snapshotDB(d, "state.db", true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var anchor string
	err = db.QueryRow("SELECT value FROM metadata WHERE key='bootstrap-enrollment'").Scan(&anchor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if errors.Is(readErr, os.ErrNotExist) {
		if anchor != "" {
			return nil, fmt.Errorf("bootstrap enrollment receipt missing")
		}
		return nil, nil
	}
	if e.validate() != nil || (anchor != e.digest() && (e.Complete || anchor != "")) {
		return nil, fmt.Errorf("bootstrap enrollment SQL authority changed")
	}
	if err = checkBootstrapEnrollmentLEDHistory(db, e); err != nil {
		return nil, err
	}
	if err = requireBootstrapCommandAliases(db, e); err != nil {
		return nil, err
	}
	return &e, nil
}
