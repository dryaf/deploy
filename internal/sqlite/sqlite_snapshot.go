package sqlite

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/dryaf/deploy/internal/process"
)

var sourceSQLiteLimits bool

var sourceSQLiteLockFiles []*os.File

// ConfigureSourceCapture is called once during startup of the isolated source
// capture process. The worker controller never enables these process-wide limits.
func ConfigureSourceCapture(locks []*os.File) {
	sourceSQLiteLimits = true
	sourceSQLiteLockFiles = locks
}

// FDs 3..5 are the shared open descriptions for export, service and target.
// Each exec explicitly forwards them; closing one process's copies must not
// release another process's lease. They never reach the archive streamer.
func InheritSourceCaptureLocks() ([]*os.File, error) {
	var files []*os.File
	for fd := 3; fd < 6; fd++ {
		file := os.NewFile(uintptr(fd), "source-capture-lock")
		info, err := file.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("invalid capture lock descriptor")
		}
		if err == nil {
			err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		}
		if err != nil {
			file.Close()
			for _, f := range files {
				f.Close()
			}
			return nil, fmt.Errorf("source capture requires inherited deployment locks: %w", err)
		}
		files = append(files, file)
	}
	return files, nil
}

// quick_check alone accepts a zero-byte source after .backup initializes it.
// Check the source before asking SQLite to produce a snapshot.
func checkSQLiteHeader(file string) error {
	info, err := os.Lstat(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 100 {
		return fmt.Errorf("SQLite source must be an initialized regular file with a complete header")
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	header := make([]byte, 100)
	if _, err = io.ReadFull(f, header); err != nil {
		return err
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return fmt.Errorf("SQLite source has an invalid or unsupported header")
	}
	pageSize := int(header[16])<<8 | int(header[17])
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || pageSize > 65536 || pageSize&(pageSize-1) != 0 {
		return fmt.Errorf("SQLite source has an invalid page size")
	}
	return nil
}

func Output(args []string, duration time.Duration) ([]byte, error) {
	if duration <= 0 {
		return nil, context.DeadlineExceeded
	}
	cmd, err := Command(args, duration)
	if err != nil {
		return nil, err
	}
	var out, stderr process.Buffer
	out.Limit = 1 << 20
	stderr.Limit = 16 << 10
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := process.Run(cmd, duration, 10*time.Second); err != nil {
		return out.Bytes(), fmt.Errorf("%w: %s", err, stderr.String())
	}
	return out.Bytes(), nil
}

func Command(args []string, duration time.Duration) (*exec.Cmd, error) {
	// Override the user startup file, bound diagnostics and disable interactive
	// behavior. This is process hygiene, not a filesystem/network sandbox claim.
	sqliteArgs := append([]string{"-init", "/dev/null", "-batch", "-bail"}, args...)
	watchdogArgs := append([]string{"--signal=TERM", "--kill-after=10s", fmt.Sprintf("%.3fs", duration.Seconds()), "sqlite3"}, sqliteArgs...)
	cmd := exec.Command("sqlite3", sqliteArgs...)
	if sourceSQLiteLimits {
		cmd = exec.Command("prlimit", append([]string{"--as=268435456", "--cpu=310", "--", "timeout"}, watchdogArgs...)...)
		if len(sourceSQLiteLockFiles) != 3 {
			return nil, fmt.Errorf("missing source capture locks")
		}
		cmd.ExtraFiles = sourceSQLiteLockFiles
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C", "TZ=UTC"}
	return cmd, nil
}

func checkSnapshotDestination(source, destination string) error {
	src, err := os.Stat(source)
	if err != nil {
		return err
	}
	dst, err := os.Lstat(destination)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !dst.Mode().IsRegular() || os.SameFile(src, dst) {
		return fmt.Errorf("snapshot destination must be an independent regular file")
	}
	if stat, ok := dst.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
		return fmt.Errorf("snapshot destination must not have hard links")
	}
	return nil
}

type ValidationError struct {
	Kind  string
	Cause error
}

func (e *ValidationError) Error() string {
	return "SQLite validation (" + e.Kind + "): " + e.Cause.Error()
}

func (e *ValidationError) Unwrap() error { return e.Cause }

func ClassifySQLiteValidationError(err error) error {
	kind := "execution"
	message := strings.ToLower(err.Error())
	for _, unsupported := range []string{"no such collation", "no such function", "unknown function", "no such module", "unsafe use of"} {
		if strings.Contains(message, unsupported) {
			kind = "unsupported-validator"
			break
		}
	}
	if strings.Contains(message, "database disk image is malformed") {
		kind = "integrity"
	}
	return &ValidationError{Kind: kind, Cause: err}
}

func ValidateStandaloneSQLiteSnapshot(file string, limit time.Duration) error {
	if limit <= 0 {
		return context.DeadlineExceeded
	}
	if err := checkSQLiteHeader(file); err != nil {
		return err
	}
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	header := make([]byte, 20)
	_, err = io.ReadFull(f, header)
	f.Close()
	if err != nil {
		return err
	}
	if header[18] != 1 || header[19] != 1 {
		return fmt.Errorf("snapshot is not standalone rollback-journal format")
	}
	if err := requireNoSQLiteSidecars(file); err != nil {
		return err
	}
	if err := ValidateSQLiteSnapshot(file, limit); err != nil {
		return err
	}
	return requireNoSQLiteSidecars(file)
}

func requireNoSQLiteSidecars(file string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(file + suffix); !os.IsNotExist(err) {
			if err != nil {
				return err
			}
			return fmt.Errorf("standalone snapshot has pending SQLite sidecar %s", strings.TrimPrefix(suffix, "-"))
		}
	}
	return nil
}
