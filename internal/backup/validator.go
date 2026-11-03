package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime/debug"
	"time"

	"github.com/dryaf/deploy/internal/backup/protocol"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/sqlite"
	"golang.org/x/sys/unix"
)

type isolatedValidationResponse struct {
	Schema     int                         `json:"schema"`
	Validation protocol.SnapshotValidation `json:"validation"`
	ErrorKind  string                      `json:"error_kind,omitempty"`
}

// Only closed DELETE-mode snapshots may use immutable mode. Never call this on
// a live source. The production entrypoint runs inside a disposable bwrap sandbox.
func validateArchivedFile(ctx context.Context, file *os.File, full bool) (protocol.SnapshotValidation, error) {
	result := protocol.SnapshotValidation{Check: "quick_check", CheckedAt: time.Now().UTC()}
	if full {
		result.Check = "integrity_check"
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 512 {
		return result, fmt.Errorf("invalid standalone database file")
	}
	header := make([]byte, 100)
	if _, err = file.ReadAt(header, 0); err != nil {
		return result, err
	}
	if string(header[:16]) != "SQLite format 3\x00" || header[18] != 1 || header[19] != 1 {
		return result, fmt.Errorf("unsupported standalone SQLite format")
	}
	db, err := sql.Open("sqlite", fmt.Sprintf("file:/proc/self/fd/%d?mode=ro&immutable=1&_pragma=trusted_schema(OFF)&_pragma=query_only(ON)&_pragma=cache_size(-65536)", file.Fd()))
	if err != nil {
		return result, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err = db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&result.SQLiteVersion); err != nil {
		return result, sqlite.ClassifySQLiteValidationError(err)
	}
	rows, err := db.QueryContext(ctx, "PRAGMA "+result.Check+"(100)")
	if err != nil {
		return result, sqlite.ClassifySQLiteValidationError(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var answer string
		if err = rows.Scan(&answer); err != nil {
			return result, err
		}
		count++
		if answer != "ok" || count > 1 {
			return result, &sqlite.ValidationError{Kind: "integrity", Cause: errors.New("SQLite reported an integrity finding")}
		}
	}
	if err = rows.Err(); err != nil {
		return result, sqlite.ClassifySQLiteValidationError(err)
	}
	if count != 1 {
		return result, fmt.Errorf("validator returned no definitive result")
	}
	if full {
		rows.Close()
		foreign, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
		if err != nil {
			return result, sqlite.ClassifySQLiteValidationError(err)
		}
		result.ForeignKeys = "ok"
		if foreign.Next() {
			result.ForeignKeys = "violations"
		}
		err = foreign.Err()
		foreign.Close()
		if err != nil {
			return result, sqlite.ClassifySQLiteValidationError(err)
		}
	}
	result.Result = "ok"
	return result, nil
}

func isolatedSnapshotValidation(file *os.File, full bool, timeout time.Duration) (protocol.SnapshotValidation, error) {
	var result protocol.SnapshotValidation
	executable, err := os.Executable()
	if err != nil {
		return result, err
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return result, fmt.Errorf("bubblewrap is required for isolated snapshot validation")
	}
	args := []string{"--unshare-all", "--die-with-parent", "--new-session", "--clearenv", "--ro-bind", executable, "/deploy", "--ro-bind", "/proc/self/fd/3", "/database.db", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--chdir", "/tmp", "/deploy", "__backup_validate"}
	if full {
		args = append(args, "--full")
	}
	cmd := exec.Command(bwrap, args...)
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	var out, stderr process.Buffer
	out.Limit = protocol.MaxBackupHeader
	stderr.Limit = 4096
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err = process.Run(cmd, timeout, 10*time.Second); err != nil {
		return result, fmt.Errorf("isolated validator failed; check static helper and user namespace prerequisites: %w", err)
	}
	var response isolatedValidationResponse
	if err = protocol.StrictBackupJSON(out.Bytes(), &response); err != nil || response.Schema != 1 {
		return result, fmt.Errorf("invalid isolated validator result")
	}
	result = response.Validation
	if response.ErrorKind != "" {
		switch response.ErrorKind {
		case "integrity", "unsupported-validator", "execution":
			return result, &sqlite.ValidationError{Kind: response.ErrorKind, Cause: errors.New("isolated SQLite validation failed")}
		default:
			return result, fmt.Errorf("unknown validator failure classification")
		}
	}
	if result.Result != "ok" {
		return result, fmt.Errorf("isolated validator did not establish integrity")
	}
	return result, nil
}

func InternalBackupValidate(full bool) error {
	debug.SetMemoryLimit(256 << 20)
	if err := unix.Setrlimit(unix.RLIMIT_AS, &unix.Rlimit{Cur: 2 << 30, Max: 2 << 30}); err != nil {
		return err
	}
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &unix.Rlimit{Cur: 128 << 20, Max: 128 << 20}); err != nil {
		return err
	}
	if err := unix.Setrlimit(unix.RLIMIT_CPU, &unix.Rlimit{Cur: 300, Max: 310}); err != nil {
		return err
	}
	file, err := os.Open("/database.db")
	if err != nil {
		return err
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := validateArchivedFile(ctx, file, full)
	response := isolatedValidationResponse{Schema: 1, Validation: result}
	if err != nil {
		response.ErrorKind = "execution"
		var classified *sqlite.ValidationError
		if errors.As(err, &classified) {
			response.ErrorKind = classified.Kind
		}
	}
	return json.NewEncoder(os.Stdout).Encode(response)
}
