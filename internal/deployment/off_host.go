package deployment

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/config"
	"github.com/dryaf/deploy/internal/process"
	"github.com/dryaf/deploy/internal/safefs"
	"github.com/dryaf/deploy/internal/sqlite"
	"github.com/dryaf/deploy/internal/transport"
	"github.com/dryaf/deploy/internal/ui"
)

func offHostEnvironment(e config.Environment) (config.Environment, error) {
	o := e.Database.OffHost
	if o == (config.OffHostConfig{}) {
		return config.Environment{}, fmt.Errorf("database.off_host is not configured")
	}
	if err := config.ValidateOffHost(o); err != nil {
		return config.Environment{}, err
	}
	return config.Environment{Host: o.Host, User: o.User, Port: o.Port, SSHKey: o.Key, Dir: path.Clean(o.Dir), Timeouts: e.Timeouts}, nil
}

func BackupOffHost(e config.Environment) (string, error) {
	destination, err := offHostEnvironment(e)
	if err != nil {
		return "", err
	}
	if ui.DryRun {
		fmt.Printf("[DRY] Verify and archive a consistent database snapshot at %s@%s:%s\n", destination.User, destination.Host, destination.Dir)
		return "", nil
	}
	dir, err := os.MkdirTemp("", "deploy-off-host-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	snapshot := filepath.Join(dir, "database.db")
	if err = downloadRemoteSnapshot(e, snapshot); err != nil {
		return "", err
	}
	checksum, err := safefs.FileChecksum(snapshot)
	if err != nil {
		return "", err
	}
	id, err := newReleaseID("database-" + filenameLabel(e.Name))
	if err != nil {
		return "", err
	}
	_, source, _ := config.DatabasePaths(e)
	record := backupRecord{Schema: 1, Database: source, Snapshot: "database.db", Environment: e.Name, Operation: "off-host", SHA256: checksum, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = safefs.WritePrivateJSON(filepath.Join(dir, "receipt.json"), record); err != nil {
		return "", err
	}
	stage := path.Join(destination.Dir, ".incoming-"+id)
	final := path.Join(destination.Dir, id)
	setup := "set -euo pipefail\numask 077\ncommand -v sqlite3 >/dev/null\n" + safeRemoteMkdir(destination.Dir) + "\nchmod 700 -- " + transport.ShellQuote(destination.Dir) + "\n" + safeRemoteMkdir(stage)
	if _, err = transport.RunSSHOutput(destination, setup); err != nil {
		return "", err
	}
	// Immutable unique directories allow a checksum-verified bundle to be published
	// with one rename. Interrupted incoming directories are never completed copies.
	if err = transport.RunRsyncSafe(destination, []string{snapshot, filepath.Join(dir, "receipt.json")}, remoteDestination(destination, stage+"/"), "--chmod=F600", "--checksum"); err != nil {
		return "", err
	}
	verify := `set -euo pipefail
umask 077
stage=` + transport.ShellQuote(stage) + `
final=` + transport.ShellQuote(final) + `
[ ! -e "$final" ] && [ ! -L "$final" ]
[ -f "$stage/database.db" ] && [ ! -L "$stage/database.db" ]
actual=$(sha256sum -- "$stage/database.db" | cut -d' ' -f1)
[ "$actual" = ` + transport.ShellQuote(checksum) + ` ]
[ "$(sqlite3 -readonly "$stage/database.db" 'PRAGMA quick_check;')" = ok ]
sync -f -- "$stage"
mv -T -- "$stage" "$final"
sync -f -- "$(dirname -- "$final")"
`
	if _, err = transport.RunSSHOutput(destination, verify); err != nil {
		return "", fmt.Errorf("off-host snapshot not confirmed: %w", err)
	}
	receipt := struct {
		backupRecord
		Host, User, Path string
	}{record, destination.Host, destination.User, final}
	body, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	receiptDir := path.Join(e.Dir, ".deploy/off-host")
	script := remoteOperationGuard(e) + "\nmake_dir " + transport.ShellQuote(receiptDir) + "\nprintf '%s\\n' " + transport.ShellQuote(string(body)) + " | atomic_json " + transport.ShellQuote(path.Join(receiptDir, id+".json")) + "\n"
	if _, err = transport.RunSSHOutput(e, script); err != nil {
		return "", fmt.Errorf("off-host bundle verified at %s; source receipt could not be recorded: %w", final, err)
	}
	return id, nil
}

func VerifyOffHostRestore(e config.Environment, id string) error {
	destination, err := offHostEnvironment(e)
	if err != nil {
		return err
	}
	if !config.ServicePattern.MatchString(id) || strings.Contains(id, "..") {
		return fmt.Errorf("invalid backup ID")
	}
	if e.Database.OffHost.RestoreCheck == "" {
		return fmt.Errorf("database.off_host.restore_check must verify an application read; DEPLOY_RESTORE_DATABASE names the disposable database")
	}
	if ui.DryRun {
		fmt.Printf("[DRY] Retrieve %s, verify its checksum and SQLite integrity, then run the configured application restore check\n", id)
		return nil
	}
	bundle := path.Join(destination.Dir, id)
	raw, err := transport.RunSSHOutput(destination, "set -eu\ncat -- "+transport.ShellQuote(path.Join(bundle, "receipt.json")))
	if err != nil {
		return err
	}
	var receipt backupRecord
	if err = json.Unmarshal([]byte(raw), &receipt); err != nil {
		return err
	}
	if receipt.Schema != 1 || len(receipt.SHA256) != 64 || receipt.Environment != e.Name || receipt.Snapshot != "database.db" {
		return fmt.Errorf("invalid off-host backup receipt")
	}
	dir, err := os.MkdirTemp("", "deploy-restore-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	snapshot := filepath.Join(dir, "database.db")
	f, err := os.OpenFile(snapshot, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	script := "set -euo pipefail\nfile=" + transport.ShellQuote(path.Join(bundle, "database.db")) + "\n[ -f \"$file\" ] && [ ! -L \"$file\" ]\ncat -- \"$file\""
	cmd := exec.Command("ssh", append(transport.GetSSHBaseArgs(destination), "bash -c "+transport.ShellQuote(script))...)
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	runErr := process.Run(cmd, config.Budget(e.Timeouts.Transfer, "10m"), config.Budget(e.Timeouts.ShutdownGrace, "10s"))
	closeErr := f.Close()
	if runErr != nil {
		return runErr
	}
	if closeErr != nil {
		return closeErr
	}
	checksum, err := safefs.FileChecksum(snapshot)
	if err != nil {
		return err
	}
	if checksum != receipt.SHA256 {
		return fmt.Errorf("off-host backup checksum mismatch")
	}
	if err = sqlite.ValidateSQLiteSnapshot(snapshot, config.Budget(e.Timeouts.Database, "5m")); err != nil {
		return err
	}
	check := exec.Command("sh", "-c", e.Database.OffHost.RestoreCheck)
	check.Env = append(os.Environ(), "DEPLOY_RESTORE_DATABASE="+snapshot)
	// The configured check can read the disposable database without receiving any
	// production environment file. Keep application output out of command logs.
	check.Stdout = io.Discard
	check.Stderr = io.Discard
	if err = process.Run(check, config.Budget(e.Timeouts.Database, "5m"), config.Budget(e.Timeouts.ShutdownGrace, "10s")); err != nil {
		return fmt.Errorf("application restore check: %w", err)
	}
	return nil
}
