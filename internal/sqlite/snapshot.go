package sqlite

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/safefs"
)

func sqliteDuration(limits []time.Duration) time.Duration {
	if len(limits) > 0 && limits[0] > 0 {
		return limits[0]
	}
	return 5 * time.Minute
}

func Snapshot(source, destination string, limits ...time.Duration) error {
	if err := checkSQLiteHeader(source); err != nil {
		return err
	}
	if err := checkSnapshotDestination(source, destination); err != nil {
		return err
	}
	deadline := time.Now().Add(sqliteDuration(limits))
	out, err := Output([]string{"-nofollow", "-readonly", source, ".backup " + strconv.Quote(destination)}, time.Until(deadline))
	if err != nil {
		return fmt.Errorf("SQLite snapshot: %w: %s", err, out)
	}
	if err := os.Chmod(destination, 0600); err != nil {
		return err
	}
	// A backup from a WAL source can retain WAL format. Normalize only the
	// private destination before hashing; published files must not need sidecars.
	out, err = Output([]string{destination, "PRAGMA journal_mode=DELETE;"}, time.Until(deadline))
	if err != nil {
		return fmt.Errorf("SQLite snapshot could not enter standalone DELETE mode: %w", err)
	}
	if strings.TrimSpace(string(out)) != "delete" {
		return fmt.Errorf("SQLite snapshot did not confirm standalone DELETE mode")
	}
	if err := ValidateStandaloneSQLiteSnapshot(destination, time.Until(deadline)); err != nil {
		return err
	}
	return safefs.SyncFile(destination)
}

func ValidateSQLiteSnapshot(file string, limits ...time.Duration) error {
	if err := checkSQLiteHeader(file); err != nil {
		return err
	}
	out, err := Output([]string{"-readonly", file, "PRAGMA trusted_schema=OFF; PRAGMA quick_check;"}, sqliteDuration(limits))
	if err != nil {
		return ClassifySQLiteValidationError(err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return &ValidationError{Kind: "integrity", Cause: fmt.Errorf("quick_check did not return ok: %s", out)}
	}
	return nil
}
